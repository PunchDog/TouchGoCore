package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNoEntry 表示 Redis 中无该键（对应 redis.Nil）
var ErrNoEntry = errors.New("cache: no entry")

// KV 是缓存层对一级缓存（Redis）的全部要求。
// 刻意收窄：便于内存 fake 单测，也避免集群/单机差异泄漏进读写线。
type KV interface {
	// Get 返回值；键不存在返回 ErrNoEntry
	Get(ctx context.Context, key string) (string, error)
	// MGet 按序返回值，缺失位用 ""（等价 ErrNoEntry）
	MGet(ctx context.Context, keys []string) ([]string, error)
	// Set 写入并强制带过期（ttl<=0 由实现回落框架默认，禁止永不过期）
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Del 删除
	Del(ctx context.Context, keys ...string) error
}

// Locker 是 KV 可选实现的「分布式锁」能力（SETNX + 原子释放脚本）。
//
// 跨进程防缓存击穿：读线 miss 回源前先抢这把锁，只允许一个进程实例真正打 DB，
// 其余进程轮询 Redis 直到锁持有者回填完成；写线写 Redis 前同样抢锁串行。
// Cache 构造时类型断言探测；KV 不实现则锁自动禁用（读回源/写路径退回无锁旧行为）。
type Locker interface {
	// SetNX 仅当 key 不存在时写入 value 并返回 true；已存在返回 false（抢锁失败）。
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Eval 执行 Lua 脚本（释放锁的「比较 token 再 DEL」必须原子），返回脚本返回值。
	Eval(ctx context.Context, script string, keys []string, args ...any) (any, error)
}

// SeqKV 是 KV 可选实现的「按 seq 条件写」窄接口（Redis 侧 Lua CAS）。
//
// 动机：写线的三步「seq 自增 → Redis 写 → 入写缓冲」并非原子，同键并发写时
// 先取到较小 seq 的一方可能后落地——Redis 会被旧值倒灌，随后写缓冲把旧值落库。
// 本接口把 Redis 那一步条件化：仅当现有 envelope 的 seq 小于本次 seq 才写，
// 与 buffer.put 的 max-seq 覆盖一起构成写线保序防线。
//
// Cache 构造时类型断言探测；KV 不实现则写线退回无条件 Set/Del（旧行为）。
type SeqKV interface {
	// SetSeq 仅当键不存在、值不可解析、或现有 envelope.Seq 不大于 seq 时写入 value。
	// 返回 true=本次写入生效；false=Redis 已有更大 seq 的值，本次过期写被丢弃。
	// （容许同 seq 重写：renewIfAged 靠它刷新超龄条目的新鲜度）
	SetSeq(ctx context.Context, key, value string, ttl time.Duration, seq int64) (bool, error)
	// DelSeq 同 SetSeq 语义的删除（Remove 用）。
	DelSeq(ctx context.Context, key string, seq int64) (bool, error)
}

// redisStore 基于 redis.Cmdable 的 KV 实现，单机/集群共用一段代码。
type redisStore struct {
	cmd       redis.Cmdable
	isCluster bool
	conc      int
}

// NewRedisStore 用原始 redis 客户端构造 KV。conc 为集群模式下批量读的并发度（<=0 用 16）。
func NewRedisStore(cmd redis.Cmdable, conc int) KV {
	if conc <= 0 {
		conc = 16
	}
	_, cluster := cmd.(*redis.ClusterClient)
	return &redisStore{cmd: cmd, isCluster: cluster, conc: conc}
}

func (s *redisStore) Get(ctx context.Context, key string) (string, error) {
	v, err := s.cmd.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNoEntry
	}
	return v, err
}

func (s *redisStore) MGet(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if !s.isCluster {
		vv, err := s.cmd.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, err
		}
		out := make([]string, len(vv))
		for i, e := range vv {
			if e == nil {
				continue // 缺失 → ""
			}
			if str, ok := e.(string); ok {
				out[i] = str
			}
		}
		return out, nil
	}
	// 集群：跨槽 MGET 会报 CROSSSLOT，退化为并发单 key Get
	out := make([]string, len(keys))
	var mu sync.Mutex
	sem := make(chan struct{}, s.conc)
	var wg sync.WaitGroup
	var firstErr error
	for i, k := range keys {
		wg.Add(1)
		go func(i int, k string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := s.Get(ctx, k)
			if err != nil {
				if !errors.Is(err, ErrNoEntry) {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
				return
			}
			out[i] = v
		}(i, k)
	}
	wg.Wait()
	return out, firstErr
}

func (s *redisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if ttl <= 0 {
		// 缓存层绝不允许写入永不过期的键：物理 TTL 是唯一失效权威
		ttl = 5 * time.Minute
	}
	return s.cmd.Set(ctx, key, value, ttl).Err()
}

func (s *redisStore) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.cmd.Del(ctx, keys...).Err()
}

func (s *redisStore) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 5 * time.Minute // 锁同样禁止永不过期
	}
	return s.cmd.SetNX(ctx, key, value, ttl).Result()
}

func (s *redisStore) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return s.cmd.Eval(ctx, script, keys, args...).Result()
}

// ---------- SeqKV：按 seq 条件写（Lua CAS） ----------
//
// 脚本均只用一个 KEY，集群模式下不会踩 CROSSSLOT。
// envelope 的 seq 字段名固定为 "s"（见 envelope.go），且位于 JSON 顶层；
// 业务值嵌在 "v" 里，所以只看顶层 t.s 不会与业务字段同名混淆。
// cjson 为 Redis 内建；值不可解析（旧版本/脏值）时当作「无 seq」放行本次写，
// 让新值自愈覆盖。
//
// 守卫用「现有 seq 严格大于本次才拒」而非「大于等于」：seq 由 Cache 内部原子自增，
// 不同写操作永不重号；同 seq 重写只来自 renewIfAged（超龄条目用同一个值刷新
// 逻辑新鲜期），必须放行，否则那个功能会变成永远空转。

const seqGuardLua = `
local cur = redis.call('GET', KEYS[1])
if cur then
  local ok, t = pcall(cjson.decode, cur)
  if ok and type(t) == 'table' and t.s ~= nil and tonumber(t.s) > tonumber(ARGV[1]) then
    return 0
  end
end
`

// seqSetScript ARGV[1]=seq ARGV[2]=value ARGV[3]=ttl(ms)
const seqSetScript = seqGuardLua + `
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1
`

// seqDelScript ARGV[1]=seq
const seqDelScript = seqGuardLua + `
redis.call('DEL', KEYS[1])
return 1
`

func ttlMS(ttl time.Duration) int64 {
	if ttl <= 0 {
		// 缓存层绝不允许写入永不过期的键：物理 TTL 是唯一失效权威
		ttl = 5 * time.Minute
	}
	return ttl.Milliseconds()
}

func (s *redisStore) SetSeq(ctx context.Context, key, value string, ttl time.Duration, seq int64) (bool, error) {
	n, err := s.cmd.Eval(ctx, seqSetScript, []string{key}, seq, value, ttlMS(ttl)).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *redisStore) DelSeq(ctx context.Context, key string, seq int64) (bool, error) {
	n, err := s.cmd.Eval(ctx, seqDelScript, []string{key}, seq).Int64()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
