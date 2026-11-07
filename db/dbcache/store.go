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
type kvStore interface {
	HGetAll(ctx context.Context, key string) (map[string]string, error)
	HSetAll(ctx context.Context, rowKey, tombKey string, fields []string, ttl time.Duration) error
	BackfillAndGet(ctx context.Context, key string, fields []string, ttl time.Duration) (map[string]string, error)
	ZAdd(ctx context.Context, key, member string, score int64) error
	DueMembers(ctx context.Context, key string, maxScore int64, count int) ([]dirtyMember, error)
	Claim(ctx context.Context, dirtyKey, rowKey, tombKey, member string, score int64) (map[string]string, bool, error)
	ZRem(ctx context.Context, key, member string) error
	ZScore(ctx context.Context, key, member string) (int64, bool, error)
	HDel(ctx context.Context, key, member string) error
	HIncrBy(ctx context.Context, key, member string) (int64, error)
	SetTomb(ctx context.Context, tombKey string, ttl time.Duration) error
	DelTomb(ctx context.Context, tombKey string) error
	DelRow(ctx context.Context, dirtyKey, rowKey, retryKey, tombKey, member string, tombTTL time.Duration) (int64, error)
	Ping(ctx context.Context) error
}

// dirtyMember 是脏 ZSET 中的一个待落库主键。
type dirtyMember struct {
	Member string
	Score  int64
}

// tableKeys 是一张表在 Redis 中的键集合。统一使用 hash tag 保证集群同槽。
// tombPrefix 下的键是删除墓碑：存在期间该行禁止被脏账回写。
type tableKeys struct {
	rowPrefix  string
	dirty      string
	retry      string
	tombPrefix string
}

var tableNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// makeTableKeys 生成表级键；表名只允许字母数字、下划线、点与短横线。
func makeTableKeys(prefix, table string) (tableKeys, error) {
	if !tableNameRe.MatchString(table) {
		return tableKeys{}, fmt.Errorf("%w: table %q", ErrBadColumn, table)
	}
	tag := "{" + prefix + ":" + table + "}"
	return tableKeys{
		rowPrefix:  tag + ":r:",
		dirty:      tag + ":dirty",
		retry:      tag + ":retry",
		tombPrefix: tag + ":t:",
	}, nil
}

const (
	// backfillScript：Redis 已有该行则不覆盖；否则写入全字段（可选 PEXPIRE）后读回。
	backfillScript = `
local h = redis.call('HGETALL', KEYS[1])
if #h > 0 then return h end
local ttl = tonumber(ARGV[1])
if #ARGV > 1 then redis.call('HSET', KEYS[1], unpack(ARGV, 2)) end
if ttl and ttl > 0 then redis.call('PEXPIRE', KEYS[1], ttl) end
return redis.call('HGETALL', KEYS[1])`
	// claimScript：先查删除墓碑，仅当脏账 score 未被刷新时才读取行并销账，
	// 避免误删并发写入，也避免已删除的行被在途脏账回写。
	claimScript = `
if redis.call('EXISTS', KEYS[3]) == 1 then
  redis.call('ZREM', KEYS[1], ARGV[1])
  return {}
end
local s = redis.call('ZSCORE', KEYS[1], ARGV[1])
if not s or tonumber(s) ~= tonumber(ARGV[2]) then return {} end
local h = redis.call('HGETALL', KEYS[2])
redis.call('ZREM', KEYS[1], ARGV[1])
return h`
	// deleteScript：立墓碑、销脏账与重试计数、删行 hash，返回行 hash 是否曾存在。
	deleteScript = `
local ttl = tonumber(ARGV[2])
if ttl and ttl > 0 then
  redis.call('SET', KEYS[4], '1', 'PX', ttl)
else
  redis.call('SET', KEYS[4], '1')
end
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
return redis.call('DEL', KEYS[2])`
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

func (s *redisStore) BackfillAndGet(ctx context.Context, key string, fields []string, ttl time.Duration) (map[string]string, error) {
	args := make([]any, 0, len(fields)+1)
	args = append(args, ttl.Milliseconds())
	for _, f := range fields {
		args = append(args, f)
	}
	raw, err := s.rdb.Eval(ctx, backfillScript, []string{key}, args...).Result()
	if err != nil {
		return nil, err
	}
	return flatToMap(raw), nil
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

func (s *redisStore) Claim(ctx context.Context, dirtyKey, rowKey, tombKey, member string, score int64) (map[string]string, bool, error) {
	raw, err := s.rdb.Eval(ctx, claimScript, []string{dirtyKey, rowKey, tombKey}, member, score).Result()
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

func (s *redisStore) DelRow(ctx context.Context, dirtyKey, rowKey, retryKey, tombKey, member string, tombTTL time.Duration) (int64, error) {
	raw, err := s.rdb.Eval(ctx, deleteScript, []string{dirtyKey, rowKey, retryKey, tombKey}, member, tombTTL.Milliseconds()).Result()
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
