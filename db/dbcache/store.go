package dbcache

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// kvStore 是缓存层依赖的窄 Redis 语义接口；测试用内存实现替换。
//
// ApplyWrite 是权威写路径的唯一入口（Lua writeScript 原子执行行/索引/守卫/键集/
// 秘密/脏账）；其余方法供读路径、flush 租约账本与测试 seeding 使用。
type kvStore interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HSetAll(ctx context.Context, rowKey, tombKey string, fields []string, ttl time.Duration) error
	ApplyWrite(ctx context.Context, keys []string, payload, txid string) (string, error)
	ApplyTx(ctx context.Context, keys []string, payload string) (string, error)
	ZAdd(ctx context.Context, key, member string, score int64) error
	DueMembers(ctx context.Context, key string, maxScore int64, count int) ([]dirtyMember, error)
	Claim(ctx context.Context, dirtyKey, rowKey, tombKey, inflightKey, member string, score, leaseDue int64) (map[string]string, bool, error)
	ZRem(ctx context.Context, key, member string) error
	ZScore(ctx context.Context, key, member string) (int64, bool, error)
	HDel(ctx context.Context, key, member string) error
	HIncrBy(ctx context.Context, key, member string) (int64, error)
	SetTomb(ctx context.Context, tombKey string, ttl time.Duration) error
	DelTomb(ctx context.Context, tombKey string) error
	TombAlive(ctx context.Context, tombKey string) (bool, error)
	KeySetScan(ctx context.Context, key string, budget int) ([]string, error)
	IndexRange(ctx context.Context, key, min, max string, byLex bool, offset, count int) ([]string, error)
	IndexCard(ctx context.Context, key string) (int64, error)
	RowsGet(ctx context.Context, keys []string) ([]map[string]string, error)
	SeqAlloc(ctx context.Context, key string, delta int64) (int64, error)
	TryLock(ctx context.Context, key, txid string, ttlMS, waitMS int64) (bool, error)
	Unlock(ctx context.Context, key, txid string) error
	RequeueInflight(ctx context.Context, inflightKey, dirtyKey string, nowMS int64) (int64, error)
	Ping(ctx context.Context) error
}

// dirtyMember 是脏 ZSET 中的一个待落库主键。
type dirtyMember struct {
	Member string
	Score  int64
}

// tableKeys 是一张表在 Redis 中的键集合。统一使用 hash tag 保证集群同槽；
// GlobalSlot=true 时 tag 缩成 {prefix} 全表同槽（跨表事务 Lua 多键必须）。
// tombPrefix 下的键是删除墓碑：存在期间该行禁止被脏账回写与 dashed 回填。
type tableKeys struct {
	rowPrefix    string // {tag}:r:<key> 行 hash
	secretPrefix string // {tag}:s:<key> 秘密列 hash（永不进行 hash）
	keySet       string // {tag}:k 存活行键集 SET
	ixPrefix     string // {tag}:ix:<col> 二级索引 ZSET
	uq           string // {tag}:uq 唯一守卫 HASH（field→rowKey）
	seq          string // {tag}:seq 自增序列 STRING
	dirty        string // {tag}:dirty 脏账 ZSET
	dirtyDel     string // {tag}:dirtyDel 异步删除账 ZSET
	inflight     string // {tag}:inflight 在途租约 ZSET
	retry        string // {tag}:retry 重试计数 HASH
	tombPrefix   string // {tag}:t:<key> 墓碑 STRING PX
	lockPrefix   string // {tag}:lk:<key> 行锁 STRING(txid) PX
}

var tableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// makeTableKeys 生成表级键；表名只允许字母数字、下划线、点与短横线。
func makeTableKeys(prefix, table string, globalSlot bool) (tableKeys, error) {
	if !tableNameRe.MatchString(table) {
		return tableKeys{}, fmt.Errorf("%w: table %q", ErrBadColumn, table)
	}
	tag := "{" + prefix + ":" + table + "}"
	if globalSlot {
		tag = "{" + prefix + "}"
	}
	return tableKeys{
		rowPrefix:    tag + ":r:",
		secretPrefix: tag + ":s:",
		keySet:       tag + ":k",
		ixPrefix:     tag + ":ix:",
		uq:           tag + ":uq",
		seq:          tag + ":seq",
		dirty:        tag + ":dirty",
		dirtyDel:     tag + ":dirtyDel",
		inflight:     tag + ":inflight",
		retry:        tag + ":retry",
		tombPrefix:   tag + ":t:",
		lockPrefix:   tag + ":lk:",
	}, nil
}

const (
	// claimScript：先查删除墓碑，仅当脏账 score 未被刷新时读取行，把成员从脏账
	// 移入 inflight 租约账（score=租约到期），sink 成功后才销账。Claim 后崩溃由
	// RequeueInflight 回收过期租约重挂脏账，关闭「领了没落库就丢更新」的窗口。
	// KEYS[1]=dirty [2]=row [3]=tomb [4]=inflight；ARGV=member,score,leaseDue。
	claimScript = `
if redis.call('EXISTS', KEYS[3]) == 1 then
  redis.call('ZREM', KEYS[1], ARGV[1])
  return {}
end
local s = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not s or tonumber(s) ~= tonumber(ARGV[2]) then return {} end
local h = redis.call('HGETALL', KEYS[2])
if #h == 0 then
  redis.call('ZREM', KEYS[1], ARGV[1])
  return {}
end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('ZADD', KEYS[4], ARGV[3], ARGV[1])
return h`
	// requeueScript：回收过期 inflight 租约重挂脏账（立即到期）。
	// KEYS[1]=inflight [2]=dirty；ARGV[1]=nowMS。返回回收条数。
	requeueScript = `
local members = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1])
for _, m in ipairs(members) do
  redis.call('ZADD', KEYS[2], ARGV[1], m)
  redis.call('ZREM', KEYS[1], m)
end
return #members`
	// unlockScript：比较删除，只释放自己的锁（锁 TTL 过期被抢后不误删他人锁）。
	unlockScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`
)

// redisStore 是 kvStore 的 go-redis 实现（单机/集群统一走 Cmdable）。
type redisStore struct {
	rdb redis.Cmdable
}

func newRedisStore(rdb redis.Cmdable) *redisStore { return &redisStore{rdb: rdb} }

func (s *redisStore) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return s.rdb.HGetAll(ctx, key).Result()
}

func (s *redisStore) HSetAll(ctx context.Context, rowKey, tombKey string, fields []string, ttl time.Duration) error {
	if len(fields) == 0 && tombKey == "" {
		return nil
	}
	args := make([]any, len(fields))
	for i, f := range fields {
		args[i] = f
	}
	p := s.rdb.Pipeline()
	// 重建该行前先清墓碑，否则删除后的首次写入会被 Claim 丢弃。
	if tombKey != "" {
		p.Del(ctx, tombKey)
	}
	if len(fields) > 0 {
		p.HSet(ctx, rowKey, args...)
		if ttl > 0 {
			p.PExpire(ctx, rowKey, ttl)
		}
	}
	_, err := p.Exec(ctx)
	return err
}

func (s *redisStore) ApplyWrite(ctx context.Context, keys []string, payload, txid string) (string, error) {
	raw, err := s.rdb.Eval(ctx, writeScript, keys, payload, txid).Result()
	if err != nil {
		return "", err
	}
	return fmt.Sprint(raw), nil
}

func (s *redisStore) ApplyTx(ctx context.Context, keys []string, payload string) (string, error) {
	raw, err := s.rdb.Eval(ctx, txScript, keys, payload).Result()
	if err != nil {
		return "", err
	}
	return fmt.Sprint(raw), nil
}

func (s *redisStore) ZAdd(ctx context.Context, key, member string, score int64) error {
	return s.rdb.ZAdd(ctx, key, redis.Z{Score: float64(score), Member: member}).Err()
}

func (s *redisStore) DueMembers(ctx context.Context, key string, maxScore int64, count int) ([]dirtyMember, error) {
	zs, err := s.rdb.ZRangeByScoreWithScores(ctx, key, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(maxScore, 10),
		Count: int64(count),
	}).Result()
	if err != nil {
		return nil, err
	}
	out := make([]dirtyMember, 0, len(zs))
	for _, z := range zs {
		member, ok := z.Member.(string)
		if !ok {
			continue
		}
		out = append(out, dirtyMember{Member: member, Score: int64(z.Score)})
	}
	return out, nil
}

func (s *redisStore) Claim(ctx context.Context, dirtyKey, rowKey, tombKey, inflightKey, member string, score, leaseDue int64) (map[string]string, bool, error) {
	raw, err := s.rdb.Eval(ctx, claimScript, []string{dirtyKey, rowKey, tombKey, inflightKey}, member, score, leaseDue).Result()
	if err != nil {
		return nil, false, err
	}
	h := flatToMap(raw)
	if len(h) == 0 {
		return nil, false, nil
	}
	return h, true, nil
}

func (s *redisStore) ZRem(ctx context.Context, key, member string) error {
	return s.rdb.ZRem(ctx, key, member).Err()
}

func (s *redisStore) ZScore(ctx context.Context, key, member string) (int64, bool, error) {
	f, err := s.rdb.ZScore(ctx, key, member).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return int64(f), true, nil
}

func (s *redisStore) HDel(ctx context.Context, key, member string) error {
	return s.rdb.HDel(ctx, key, member).Err()
}

func (s *redisStore) SetTomb(ctx context.Context, tombKey string, ttl time.Duration) error {
	return s.rdb.Set(ctx, tombKey, "1", ttl).Err()
}

func (s *redisStore) DelTomb(ctx context.Context, tombKey string) error {
	return s.rdb.Del(ctx, tombKey).Err()
}

func (s *redisStore) TombAlive(ctx context.Context, tombKey string) (bool, error) {
	n, err := s.rdb.Exists(ctx, tombKey).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *redisStore) KeySetScan(ctx context.Context, key string, budget int) ([]string, error) {
	n, err := s.rdb.SCard(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if budget > 0 && n > int64(budget) {
		return nil, fmt.Errorf("%w: %s has %d rows", ErrScanBudgetExceeded, key, n)
	}
	return s.rdb.SMembers(ctx, key).Result()
}

func (s *redisStore) IndexRange(ctx context.Context, key, min, max string, byLex bool, offset, count int) ([]string, error) {
	rb := &redis.ZRangeBy{Min: min, Max: max, Offset: int64(offset), Count: int64(count)}
	if byLex {
		return s.rdb.ZRangeByLex(ctx, key, rb).Result()
	}
	return s.rdb.ZRangeByScore(ctx, key, rb).Result()
}

func (s *redisStore) IndexCard(ctx context.Context, key string) (int64, error) {
	return s.rdb.ZCard(ctx, key).Result()
}

func (s *redisStore) RowsGet(ctx context.Context, keys []string) ([]map[string]string, error) {
	p := s.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(keys))
	for i, key := range keys {
		cmds[i] = p.HGetAll(ctx, key)
	}
	if _, err := p.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]map[string]string, len(keys))
	for i, cmd := range cmds {
		h, err := cmd.Result()
		if err != nil {
			return nil, err
		}
		out[i] = h
	}
	return out, nil
}

func (s *redisStore) SeqAlloc(ctx context.Context, key string, delta int64) (int64, error) {
	return s.rdb.IncrBy(ctx, key, delta).Result()
}

func (s *redisStore) TryLock(ctx context.Context, key, txid string, ttlMS, waitMS int64) (bool, error) {
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
	ttl := time.Duration(ttlMS) * time.Millisecond
	for {
		ok, err := s.rdb.SetNX(ctx, key, txid, ttl).Result()
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *redisStore) Unlock(ctx context.Context, key, txid string) error {
	return s.rdb.Eval(ctx, unlockScript, []string{key}, txid).Err()
}

func (s *redisStore) RequeueInflight(ctx context.Context, inflightKey, dirtyKey string, nowMS int64) (int64, error) {
	raw, err := s.rdb.Eval(ctx, requeueScript, []string{inflightKey, dirtyKey}, nowMS).Result()
	if err != nil {
		return 0, err
	}
	n, _ := raw.(int64)
	return n, nil
}

func (s *redisStore) HIncrBy(ctx context.Context, key, member string) (int64, error) {
	return s.rdb.HIncrBy(ctx, key, member, 1).Result()
}

func (s *redisStore) Ping(ctx context.Context) error {
	return s.rdb.Ping(ctx).Err()
}

// flatToMap 把 Lua HGETALL 返回的扁平数组转成 map。
func flatToMap(raw any) map[string]string {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make(map[string]string, len(arr)/2)
	for i := 0; i+1 < len(arr); i += 2 {
		out[fmt.Sprint(arr[i])] = fmt.Sprint(arr[i+1])
	}
	return out
}
