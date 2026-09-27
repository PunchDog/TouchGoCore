package cache

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"touchgocore/vars"
)

// ==================== 脏账本（crash-journal） ====================
//
// 动机：写线「同步写 Redis 即返回」，落库靠进程内缓冲 + 定时器。
// kill -9 / OOM / Stop 超时这类非优雅退出会丢掉进程内缓冲——若此时
// Redis 里的值又恰好过期，读线回源拿到旧库值，造成静默丢数据。
//
// 对策：Write/Remove 在同步写 Redis 的同一次 pipeline 里，把「哪些键
// 待落库」记进 Redis 自身的 ZSET 账本（member=op|键，score=seq）；
// 落库成功或超限丢弃时销账。进程重启（Layer.Start → Cache.Run 开头）
// 扫账本，把还活得着的 envelope 值重新落库——把丢失窗口从「整个缓冲」
// 压缩到「envelope 已 TTL 过期」这一小段（该段以 RecoverMiss 告警兜底）。
//
// 停服（ctx cancel / Close）final flush 若落库仍失败：不得清账、不得 Del Redis，
// 返回错误；下次启动与宕机同一 recoverJournal 路径。清账留 Redis 会使值不可恢复。
//
// 账本成员按 keyStr 去重：同一键重复写只留最新一条（ZADD 覆盖 score），
// op 翻转时（先写后删）同时 ZREM 旧 op 成员，保证一键至多一条账。
// 销账用 JClearIfSeq（score==本条 seq）——旧 flush 不得清掉更新 Write 的账。

// JournalEntry 账本的一条待落库记录（JScan 返回，按 Seq 升序）。
type JournalEntry struct {
	Op     OpKind
	KeyStr string // keyFn(key) 的原始字符串（非 KeyOf，无转义）
	Seq    int64
}

// Journaler 是 KV（Redis store）可选实现的账本窄接口。
// Cache 构造时类型断言探测：KV 不实现（如内存 fake 之外的场景）则账本自动关闭，
// 行为退回无账本版本，不影响读写线。
type Journaler interface {
	// JAddUpsert 原子完成：SET 缓存值 + 销 tombstone 账 + 记 upsert 账
	JAddUpsert(ctx context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) error
	// JAddDelete 原子完成：DEL 缓存值 + 销 upsert 账 + 记 tombstone 账
	JAddDelete(ctx context.Context, key, zkey, keyStr string, seq int64) error
	// JClear 无条件销账（WriteSync 成功 / 恢复止损等；members 为完整 member）
	JClear(ctx context.Context, zkey string, members ...string) error
	// JClearIfSeq 仅当 member 的 score 仍等于 seq 时 ZREM——防旧 flush 误清新 Write 的账
	JClearIfSeq(ctx context.Context, zkey, member string, seq int64) error
	// JScan 全量读账本，按 Seq 升序
	JScan(ctx context.Context, zkey string) ([]JournalEntry, error)
}

// JournalSeq 是 Journaler 可选的「按 seq 条件记账」扩展（Redis 侧 Lua CAS）。
//
// 动机：无条件的 JAddUpsert/JAddDelete 在同键并发写下会让账本 score 被更小
// 的 seq 覆盖（后落地的小 seq 写把 score 拉低），于是落库侧的 JClearIfSeq
// （score==本条 seq 才 ZREM）永远对不上，账本残留旧条目并在下次启动重放。
// 本扩展把「写值 + 记账」按 seq 单调化：CAS 未生效（已有更大 seq）时既不写值也不记账。
//
// Cache 构造时类型断言探测；KV 不实现则退回 JAddUpsert/JAddDelete（旧行为）。
type JournalSeq interface {
	Journaler
	// JAddUpsertSeq 原子完成：seq 条件 SET（CAS）+ 销 tombstone 账 + 按 seq 单调记 upsert 账。
	// 返回 false 表示 Redis 已有更大 seq 的值，本次未写值也未记账。
	JAddUpsertSeq(ctx context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) (bool, error)
	// JAddDeleteSeq 同上语义的删除 + 记 tombstone 账。
	JAddDeleteSeq(ctx context.Context, key, zkey, keyStr string, seq int64) (bool, error)
}

const (
	jOpUpsertPrefix = "u|"
	jOpDeletePrefix = "d|"
	jDirtySuffix    = ":dirty#zset" // 避开正常键名；与 envelope 同前缀不同形态(zset vs string)
)

func jMember(op OpKind, keyStr string) string {
	if op == OpDelete {
		return jOpDeletePrefix + keyStr
	}
	return jOpUpsertPrefix + keyStr
}

func jParseMember(m string) (JournalEntry, bool) {
	// keyStr 自身可含 "|"，只按第一个 '|' 前的 op 前缀切分
	if rest, ok := strings.CutPrefix(m, jOpUpsertPrefix); ok {
		return JournalEntry{Op: OpUpsert, KeyStr: rest}, true
	}
	if rest, ok := strings.CutPrefix(m, jOpDeletePrefix); ok {
		return JournalEntry{Op: OpDelete, KeyStr: rest}, true
	}
	return JournalEntry{}, false
}

// journalKey 该 Cache 的账本 ZSET 键（与 envelope 键同一命名空间，按 name 隔离）。
// 单机（非集群）下这就是写线/销账/扫账实际使用的键；集群下写线改用 clusterJournalKey
// 派生的同槽 per-key 键，本键退化为「命名空间锚点」——只用于反推 keyBase（见下）。
func (c *Cache[K, V]) journalKey() string {
	return c.keyBase() + jDirtySuffix
}

// ---------- 集群同槽（hashtag）派生 ----------
//
// 症结：写线的「写值 + 记账」是双键操作。单机 pipeline / 双键 Lua 天然原子；
// 集群下值键与全局账本键分属不同槽（不同节点），go-redis 只能按节点拆分执行，
// 于是出现「值已写、账未记」的非原子窗口——崩溃后账本扫不到这条，envelope 又已
// 落不进库，造成静默丢数据。
//
// 对策：集群下把账本键派生成与值键**同槽**的 hashtag 键，双键 Lua 便不再 CROSSSLOT，
// 可原子执行，彻底关闭该窗口。值键格式（KeyOf 产物）一律不动——线上已有数据、
// TestKeyOf 亦断言其形状；hashtag 只加在账本侧键的派生上。

// clusterJournalKey 集群模式下的账本 ZSET 键：以值键整体作 hashtag，形如
//
//	{<值键>}:dirty#zset
//
// Redis Cluster 只对键中第一个非空 {...} 内的子串算 CRC16 槽；无 hashtag 时对整键算槽。
// 值键（如 tg:g1:t:1001）不含 hashtag，其槽 = CRC16(整键)；本键把同一个值键塞进 {} 作
// hashtag，其槽 = CRC16(值键) —— 两者恒等，故双键 Lua（KEYS[1]=值键, KEYS[2]=本键）
// 在集群下同槽、不报 CROSSSLOT，可原子执行。
//
// 迁移影响：旧版本集群把账本记在全局键 <keyBase>:dirty#zset（无 hashtag）。升级后写线
// 改记到 {<值键>}:dirty#zset，旧全局键不再被扫描/销账而成为孤儿。账本是崩溃恢复用的
// **瞬态**数据：旧全局键里未销的条目，其引用的 envelope 值会按物理 TTL 自然过期，即便
// 被重放也只会命中 RecoverMiss（幂等、无副作用），故在途旧格式账本可安全忽略、待运维
// 清理即可，无需在线迁移。单机路径完全不变，仍用全局账本键。
//
// 已知边界：若值键本身含 '{' 或 '}'，hashtag 会截断到第一个 '}'，槽不再与值键一致，双键
// Lua 将报 CROSSSLOT。这类键属病态输入（sanitizeKey 只收敛 ':' 与超长，不改写值键格式），
// 账本本就是尽力而为的保险，此边界按可接受降级处理。
func clusterJournalKey(valueKey string) string {
	return "{" + valueKey + "}" + jDirtySuffix
}

// clusterValueKeyFromMember 由「全局账本键 + 账本 member」反推值键（KeyOf 产物）。
// 集群销账只拿得到全局键与 member（op|keyStr），需据此还原写线用过的同槽账本键：
// keyBase = 全局键去掉 jDirtySuffix 后缀；值键 = keyBase + ":" + sanitizeKey(keyStr)。
// 这与 Cache.KeyOf 逐字一致——KeyOf = keyBase + ":" + sanitizeKey(keyFn(key))，而 member
// 里的 keyStr 正是 keyFn(key)。member 前缀按固定长度 CutPrefix，keyStr 自身含 '|' 也不歧义。
func clusterValueKeyFromMember(zkey, member string) (string, bool) {
	var keyStr string
	if rest, ok := strings.CutPrefix(member, jOpUpsertPrefix); ok {
		keyStr = rest
	} else if rest, ok := strings.CutPrefix(member, jOpDeletePrefix); ok {
		keyStr = rest
	} else {
		return "", false
	}
	keyBase := strings.TrimSuffix(zkey, jDirtySuffix)
	return keyBase + ":" + sanitizeKey(keyStr), true
}

// clusterJournalScanPattern 集群启动扫账用的 glob：匹配本 Cache 命名空间下所有同槽账本键
// {<keyBase>:*}<jDirtySuffix>。'{' '}' 在 Redis glob 中是字面量，'*' 匹配 sanitize 后的键尾。
func clusterJournalScanPattern(zkey string) string {
	keyBase := strings.TrimSuffix(zkey, jDirtySuffix)
	return "{" + keyBase + ":*}" + jDirtySuffix
}

// journalZKey 本次账本操作实际使用的 ZSET 键：集群下用与值键同槽的 hashtag 键，
// 单机下沿用调用方传入的全局账本键（行为逐字不变）。
func (s *redisStore) journalZKey(valueKey, zkey string) string {
	if s.isCluster {
		return clusterJournalKey(valueKey)
	}
	return zkey
}

// ---------- redisStore 实现 ----------
//
// 单机：pipeline 天然原子（单节点），沿用不变。
// 集群：值键与账本键经 clusterJournalKey 同槽后，改用双键 Lua 原子执行，
// 关闭 pipeline 按节点拆分带来的「值已写、账未记」窗口。

// jAddUpsertScript（非 seq 路径、集群用）KEYS[1]=值键 KEYS[2]=同槽账本 ZSET
// ARGV[1]=value ARGV[2]=ttl(ms) ARGV[3]=反向 member ARGV[4]=seq ARGV[5]=新 member
const jAddUpsertScript = `
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
redis.call('ZREM', KEYS[2], ARGV[3])
redis.call('ZADD', KEYS[2], ARGV[4], ARGV[5])
return 1
`

// jAddDeleteScript（非 seq 路径、集群用）KEYS[1]=值键 KEYS[2]=同槽账本 ZSET
// ARGV[1]=反向 member ARGV[2]=seq ARGV[3]=新 member
const jAddDeleteScript = `
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[3])
return 1
`

func (s *redisStore) JAddUpsert(ctx context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) error {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if s.isCluster {
		// 同槽双键 Lua：SET 值 + 销 tombstone 账 + 记 upsert 账，原子完成。
		return s.cmd.Eval(ctx, jAddUpsertScript, []string{key, clusterJournalKey(key)},
			value, ttlMS(ttl), jOpDeletePrefix+keyStr, seq, jOpUpsertPrefix+keyStr).Err()
	}
	pipe := s.cmd.Pipeline()
	pipe.Set(ctx, key, value, ttl)
	pipe.ZRem(ctx, zkey, jOpDeletePrefix+keyStr)
	pipe.ZAdd(ctx, zkey, redis.Z{Score: float64(seq), Member: jOpUpsertPrefix + keyStr})
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) JAddDelete(ctx context.Context, key, zkey, keyStr string, seq int64) error {
	if s.isCluster {
		// 同槽双键 Lua：DEL 值 + 销 upsert 账 + 记 tombstone 账，原子完成。
		return s.cmd.Eval(ctx, jAddDeleteScript, []string{key, clusterJournalKey(key)},
			jOpUpsertPrefix+keyStr, seq, jOpDeletePrefix+keyStr).Err()
	}
	pipe := s.cmd.Pipeline()
	pipe.Del(ctx, key)
	pipe.ZRem(ctx, zkey, jOpUpsertPrefix+keyStr)
	pipe.ZAdd(ctx, zkey, redis.Z{Score: float64(seq), Member: jOpDeletePrefix + keyStr})
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) JClear(ctx context.Context, zkey string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	anyMembers := make([]any, len(members))
	for i, m := range members {
		anyMembers[i] = m
	}
	zk := zkey
	if s.isCluster {
		// 集群：账记在与值键同槽的 per-key 键上，销账需由 member 反推该键。
		// 同一次调用的 members 属同一 keyStr（jClearKey/jClearEntries/recoverJournal 语义），
		// 同槽，故按 members[0] 定位即可。
		vk, ok := clusterValueKeyFromMember(zkey, members[0])
		if !ok {
			return fmt.Errorf("cache: 集群销账无法从 member %q 反推同槽账本键", members[0])
		}
		zk = clusterJournalKey(vk)
	}
	return s.cmd.ZRem(ctx, zk, anyMembers...).Err()
}

// jClearIfSeqScript：score 仍为本条 seq 才 ZREM，避免旧 flush 清掉更新 Write 的账。
const jClearIfSeqScript = `
local score = redis.call('ZSCORE', KEYS[1], ARGV[1])
if score and tonumber(score) == tonumber(ARGV[2]) then
  return redis.call('ZREM', KEYS[1], ARGV[1])
end
return 0
`

func (s *redisStore) JClearIfSeq(ctx context.Context, zkey, member string, seq int64) error {
	zk := zkey
	if s.isCluster {
		// 单键 Eval，同槽后集群下也不报 CROSSSLOT；由 member 反推写线用过的 per-key 账本键。
		vk, ok := clusterValueKeyFromMember(zkey, member)
		if !ok {
			return fmt.Errorf("cache: 集群条件销账无法从 member %q 反推同槽账本键", member)
		}
		zk = clusterJournalKey(vk)
	}
	return s.cmd.Eval(ctx, jClearIfSeqScript, []string{zk}, member, seq).Err()
}

func (s *redisStore) JScan(ctx context.Context, zkey string) ([]JournalEntry, error) {
	if s.isCluster {
		return s.jScanCluster(ctx, zkey)
	}
	zs, err := s.cmd.ZRangeWithScores(ctx, zkey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return parseJournalZRange(zs), nil
}

// jScanCluster 集群启动扫账：账本分散在每个值键同槽的 per-key ZSET 上，
// 用 SCAN 按命名空间 glob（clusterJournalScanPattern）逐个枚举后汇总。
// ClusterClient.Scan 会遍历所有主节点，故能扫全。
func (s *redisStore) jScanCluster(ctx context.Context, zkey string) ([]JournalEntry, error) {
	pattern := clusterJournalScanPattern(zkey)
	var out []JournalEntry
	iter := s.cmd.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		zs, err := s.cmd.ZRangeWithScores(ctx, iter.Val(), 0, -1).Result()
		if err != nil {
			return nil, err
		}
		out = append(out, parseJournalZRange(zs)...)
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	sortJournalEntries(out)
	return out, nil
}

// parseJournalZRange 把 ZRANGE...WITHSCORES 结果解析成 JournalEntry 列表（跳过坏 member）。
func parseJournalZRange(zs []redis.Z) []JournalEntry {
	out := make([]JournalEntry, 0, len(zs))
	for _, z := range zs {
		m, ok := z.Member.(string)
		if !ok {
			continue
		}
		je, ok := jParseMember(m)
		if !ok {
			continue
		}
		je.Seq = int64(z.Score)
		out = append(out, je)
	}
	return out
}

// ---------- JournalSeq：按 seq 条件写值 + 单调记账 ----------
//
// 单机与集群走同一段双键 Lua：「CAS 写值 / 销反向账 / ZSCORE 守卫下 ZADD」完全原子。
// 集群下值键与账本 ZSET 经 clusterJournalKey 派生成同槽（{值键}:dirty#zset），双键
// Lua 不再 CROSSSLOT——于是彻底关闭了旧版「单键 CAS + pipeline 记账」退化路径的
// 「值已写、账未记」非原子窗口（journalZKey 在单机下仍返回全局账本键，行为不变）。
// ZADD 前用 ZSCORE 守卫而非 ZADD GT：后者要 Redis 6.2+，旧服务端会直接报错。

// jAddUpsertSeqScript KEYS[1]=缓存键 KEYS[2]=账本 ZSET
// ARGV[1]=seq ARGV[2]=value ARGV[3]=ttl(ms) ARGV[4]=反向 member ARGV[5]=新 member
const jAddUpsertSeqScript = seqGuardLua + `
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
redis.call('ZREM', KEYS[2], ARGV[4])
local sc = redis.call('ZSCORE', KEYS[2], ARGV[5])
if (not sc) or tonumber(sc) < tonumber(ARGV[1]) then
  redis.call('ZADD', KEYS[2], ARGV[1], ARGV[5])
end
return 1
`

// jAddDeleteSeqScript KEYS[1]=缓存键 KEYS[2]=账本 ZSET
// ARGV[1]=seq ARGV[2]=反向 member ARGV[3]=新 member
const jAddDeleteSeqScript = seqGuardLua + `
redis.call('DEL', KEYS[1])
redis.call('ZREM', KEYS[2], ARGV[2])
local sc = redis.call('ZSCORE', KEYS[2], ARGV[3])
if (not sc) or tonumber(sc) < tonumber(ARGV[1]) then
  redis.call('ZADD', KEYS[2], ARGV[1], ARGV[3])
end
return 1
`

// JAddUpsertSeq 实现 JournalSeq：CAS 写值 + 销反向账 + score 单调上行记 upsert 账。
// 单机/集群同走一段双键 Lua：journalZKey 保证集群下账本键与值键同槽，不报 CROSSSLOT。
func (s *redisStore) JAddUpsertSeq(ctx context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) (bool, error) {
	newMember := jOpUpsertPrefix + keyStr
	oppMember := jOpDeletePrefix + keyStr
	n, err := s.cmd.Eval(ctx, jAddUpsertSeqScript, []string{key, s.journalZKey(key, zkey)},
		seq, value, ttlMS(ttl), oppMember, newMember).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// JAddDeleteSeq 实现 JournalSeq：CAS 删值 + 销 upsert 账 + score 单调记 tombstone 账。
func (s *redisStore) JAddDeleteSeq(ctx context.Context, key, zkey, keyStr string, seq int64) (bool, error) {
	newMember := jOpDeletePrefix + keyStr
	oppMember := jOpUpsertPrefix + keyStr
	n, err := s.cmd.Eval(ctx, jAddDeleteSeqScript, []string{key, s.journalZKey(key, zkey)},
		seq, oppMember, newMember).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ---------- 启动恢复（扫账重放） ----------

// recoverJournal 在 Cache.Run 开头执行：扫本 Cache 的脏账本，把上次进程
// 崩溃/被杀时来不及落库的条目重新落库。逐条尽力：
//   - tombstone 账 → 重放 Saver.Delete；
//   - upsert 账 → 读 Redis envelope（值本体），成功则重放 Saver.Save；
//     envelope 已物理过期/坏值 → 不可恢复，RecoverMiss 计数 + Error 告警；
//   - 落库失败 → 保留账目，下次启动再试（不丢不复活已销的账）。
func (c *Cache[K, V]) recoverJournal(ctx context.Context) {
	if c.jr == nil || c.sn == nil || !c.cfg.Enabled {
		return
	}
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.ReadTimeout*30)
	defer cancel()
	entries, err := c.jr.JScan(jctx, c.jz)
	if err != nil {
		c.st.JournalErr.Add(1)
		vars.Warning("cache[%s] 启动扫账失败（跳过恢复）: %v", c.name, err)
		return
	}
	if len(entries) == 0 {
		return
	}
	sortJournalEntries(entries)
	vars.Info("cache[%s] 启动恢复：账本有 %d 条未落库记录", c.name, len(entries))
	for i, je := range entries {
		if jctx.Err() != nil {
			vars.Warning(recoverAbortMsg(c.name, len(entries)-i))
			return
		}
		member := jMember(je.Op, je.KeyStr)
		k, derr := c.keyDec(je.KeyStr)
		if derr != nil {
			c.st.RecoverMiss.Add(1)
			vars.Error("cache[%s] 恢复跳过：键无法反序列化 %v（需 WithKeyDecoder）key=%s", c.name, derr, je.KeyStr)
			_ = c.jr.JClear(jctx, c.jz, member) // 解不开就永远重放不掉，销账止损
			continue
		}
		// 缓冲在途（恢复期间已有新 Write）：写线自己会落库并销账，跳过
		if c.buf != nil && c.buf.has(c.KeyOf(k)) {
			continue
		}
		if je.Op == OpDelete {
			if err := c.sn.Delete(jctx, k); err != nil {
				vars.Warning("cache[%s] 恢复重放删除失败 key=%s: %v（留待下次启动）", c.name, je.KeyStr, err)
				continue
			}
			_ = c.jr.JClear(jctx, c.jz, member)
			c.st.Recovered.Add(1)
			continue
		}
		raw, gerr := c.kv.Get(jctx, c.KeyOf(k))
		if gerr != nil {
			c.st.RecoverMiss.Add(1)
			vars.Error("cache[%s] 恢复失败：账本在但 Redis 值已消失（TTL 早于重启过期）key=%s seq=%d", c.name, je.KeyStr, je.Seq)
			_ = c.jr.JClear(jctx, c.jz, member)
			continue
		}
		e, derr := decode[V](c.codec, raw)
		if derr != nil || e.Null {
			c.st.RecoverMiss.Add(1)
			vars.Error("cache[%s] 恢复失败：账本值坏/空标记 key=%s seq=%d", c.name, je.KeyStr, je.Seq)
			_ = c.jr.JClear(jctx, c.jz, member)
			continue
		}
		val := e.Value
		if err := c.sn.Save(jctx, k, &val); err != nil {
			vars.Warning("cache[%s] 恢复重放落库失败 key=%s: %v（留待下次启动）", c.name, je.KeyStr, err)
			continue
		}
		_ = c.jr.JClear(jctx, c.jz, member)
		c.st.Recovered.Add(1)
	}
	vars.Info("cache[%s] 启动恢复完成：重放 %d，不可恢复 %d", c.name, c.st.Recovered.Load(), c.st.RecoverMiss.Load())
}

// recoverAbortMsg 恢复超时中止的告警文案。单独成函数是因为 vars 没有日志捕获设施，
// 「剩余 N 条」这个语义只能对格式化结果做断言：此前这里误传 je.KeyStr（键名），
// 打出的是「剩余 u|k1 条」，运维看到的是一个键而不是条数，恢复进度彻底失真。
func recoverAbortMsg(name string, remaining int) string {
	return fmt.Sprintf("cache[%s] 启动恢复超时中止（剩余 %d 条留待下次）", name, remaining)
}

// ---------- keyStr → K 反序列化（恢复重放用） ----------

// decodeKeyDefault 覆盖常见可比较键类型：string 与整数族。
// 其余类型返回错误，需用户经 WithKeyDecoder 显式提供。
func decodeKeyDefault[K comparable](s string) (K, error) {
	var z K
	v := reflect.ValueOf(&z).Elem()
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
		return z, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return z, fmt.Errorf("cache: 恢复解码 int 键 %q: %w", s, err)
		}
		if v.OverflowInt(n) {
			return z, fmt.Errorf("cache: 恢复解码 int 键 %q 溢出 %T", s, z)
		}
		v.SetInt(n)
		return z, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return z, fmt.Errorf("cache: 恢复解码 uint 键 %q: %w", s, err)
		}
		if v.OverflowUint(n) {
			return z, fmt.Errorf("cache: 恢复解码 uint 键 %q 溢出 %T", s, z)
		}
		v.SetUint(n)
		return z, nil
	default:
		return z, fmt.Errorf("cache: 键类型 %T 无法自动反序列化，请用 WithKeyDecoder 提供解码函数", z)
	}
}

// 编译期确认 redisStore 同时是 KV、Journaler 与 JournalSeq
var (
	_ KV         = (*redisStore)(nil)
	_ Journaler  = (*redisStore)(nil)
	_ JournalSeq = (*redisStore)(nil)
	_ SeqKV      = (*redisStore)(nil)
)

// sortJournalEntries 按 seq 升序（fake JScan 复用；redis ZRANGE 本已有序）
func sortJournalEntries(es []JournalEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Seq < es[j].Seq })
}
