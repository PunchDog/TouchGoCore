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
// 账本成员按 keyStr 去重：同一键重复写只留最新一条（ZADD 覆盖 score），
// op 翻转时（先写后删）同时 ZREM 旧 op 成员，保证一键至多一条账。

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
	// JClear 销账（落库成功/丢弃后调用；members 为完整 member）
	JClear(ctx context.Context, zkey string, members ...string) error
	// JScan 全量读账本，按 Seq 升序
	JScan(ctx context.Context, zkey string) ([]JournalEntry, error)
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
func (c *Cache[K, V]) journalKey() string {
	return c.keyBase() + jDirtySuffix
}

// ---------- redisStore 实现 ----------
//
// 单机 pipeline 天然原子；集群下 go-redis 按节点拆分执行，理论上可能部分
// 成功——账本本身是尽力而为的保险（销账幂等、重放按 seq 收敛），可接受。

func (s *redisStore) JAddUpsert(ctx context.Context, key, value string, ttl time.Duration, zkey, keyStr string, seq int64) error {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	pipe := s.cmd.Pipeline()
	pipe.Set(ctx, key, value, ttl)
	pipe.ZRem(ctx, zkey, jOpDeletePrefix+keyStr)
	pipe.ZAdd(ctx, zkey, redis.Z{Score: float64(seq), Member: jOpUpsertPrefix + keyStr})
	_, err := pipe.Exec(ctx)
	return err
}

func (s *redisStore) JAddDelete(ctx context.Context, key, zkey, keyStr string, seq int64) error {
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
	return s.cmd.ZRem(ctx, zkey, anyMembers...).Err()
}

func (s *redisStore) JScan(ctx context.Context, zkey string) ([]JournalEntry, error) {
	zs, err := s.cmd.ZRangeWithScores(ctx, zkey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
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
	return out, nil
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
	for _, je := range entries {
		if jctx.Err() != nil {
			vars.Warning("cache[%s] 启动恢复超时中止（剩余 %s 条留待下次）", c.name, je.KeyStr)
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

// 编译期确认 redisStore 同时是 KV 与 Journaler
var (
	_ KV        = (*redisStore)(nil)
	_ Journaler = (*redisStore)(nil)
)

// sortJournalEntries 按 seq 升序（fake JScan 复用；redis ZRANGE 本已有序）
func sortJournalEntries(es []JournalEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Seq < es[j].Seq })
}
