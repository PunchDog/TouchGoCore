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
