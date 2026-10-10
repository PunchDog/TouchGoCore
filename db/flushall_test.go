package db

import (
	"context"
	"sync"
	"testing"
	"time"

	cachepkg "touchgocore/db/cache"
)

// memKV 内存版一级缓存替身：只实现 KV 窄接口（不实现 SeqKV/Journaler，
// 写线自动退化为无条件 Set/Del），用于验证 FlushAll 的落库行为。
type memKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *memKV) Get(_ context.Context, key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	if !ok {
		return "", cachepkg.ErrNoEntry
	}
	return v, nil
}

func (k *memKV) MGet(ctx context.Context, keys []string) ([]string, error) {
	out := make([]string, len(keys))
	for i, key := range keys {
		v, err := k.Get(ctx, key)
		if err == cachepkg.ErrNoEntry {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (k *memKV) Set(_ context.Context, key, value string, _ time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

func (k *memKV) Del(_ context.Context, keys ...string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, key := range keys {
		delete(k.m, key)
	}
	return nil
}

// recSaver 记录落库内容的假「数据库」。
type recSaver struct {
	mu    sync.Mutex
	saved map[string]int
}

func (s *recSaver) Save(_ context.Context, key string, val *testVal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved[key] = val.N
	return nil
}

func (s *recSaver) Delete(_ context.Context, _ string) error {
	return nil
}

func (s *recSaver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// TestFlushAllFlushesAllCaches 验证 FlushAll 把 Layer 上全部 Cache 的
// 写缓冲立即落库（同步），而不是等周期定时器。
func TestFlushAllFlushesAllCaches(t *testing.T) {
	kv := &memKV{m: map[string]string{}}
	sn := &recSaver{saved: map[string]int{}}

	cfg := DefaultCacheConfig()
	cfg.TTL = time.Hour
	cfg.FlushInterval = time.Hour // 周期落库被拉长，落库只可能来自 FlushAll

	layer := NewCacheLayer(kv, WithLayerConfig(cfg))
	cacheA, err := OpenCache[string, testVal](layer, "a",
		cachepkg.WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	cacheB, err := OpenCache[string, testVal](layer, "b",
		cachepkg.WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 写两条缓存：Redis 同步可见，但数据库一条都还没收到（周期 flush 被 1 小时挡住）。
	if err := cacheA.Write(ctx, "k1", &testVal{N: 1}); err != nil {
		t.Fatalf("cacheA.Write k1: %v", err)
	}
	if err := cacheB.Write(ctx, "k2", &testVal{N: 2}); err != nil {
		t.Fatalf("cacheB.Write k2: %v", err)
	}
	if got := sn.count(); got != 0 {
		t.Fatalf("FlushAll 前不应有周期落库: got=%d", got)
	}

	if err := FlushAll(ctx, layer); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	// 两个 Cache 的脏键都必须一次性落库。
	sn.mu.Lock()
	defer sn.mu.Unlock()
	if len(sn.saved) != 2 {
		t.Fatalf("FlushAll 后脏数据未全部落库: saved=%d want=2", len(sn.saved))
	}
	if got := sn.saved["k1"]; got != 1 {
		t.Fatalf("k1 落库值: %d want=1", got)
	}
	if got := sn.saved["k2"]; got != 2 {
		t.Fatalf("k2 落库值: %d want=2", got)
	}
}

// TestFlushAllNilLayer 验证 nil Layer 是空操作（缓存层未启用时不报错）。
func TestFlushAllNilLayer(t *testing.T) {
	ctx := context.Background()
	if err := FlushAll(ctx, nil); err != nil {
		t.Fatalf("FlushAll(nil) 应返回 nil: %v", err)
	}
}

// TestFlushAllIdempotent 验证重复 FlushAll 幂等：无脏数据时返回 nil 且不重复落库。
func TestFlushAllIdempotent(t *testing.T) {
	kv := &memKV{m: map[string]string{}}
	sn := &recSaver{saved: map[string]int{}}

	cfg := DefaultCacheConfig()
	cfg.TTL = time.Hour
	cfg.FlushInterval = time.Hour

	layer := NewCacheLayer(kv, WithLayerConfig(cfg))
	cache, err := OpenCache[string, testVal](layer, "a",
		cachepkg.WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := cache.Write(ctx, "k1", &testVal{N: 7}); err != nil {
		t.Fatal(err)
	}
	if err := FlushAll(ctx, layer); err != nil {
		t.Fatalf("第一次 FlushAll: %v", err)
	}
	// 第二次 FlushAll：缓冲已空，应返回 nil 且不新增落库。
	if err := FlushAll(ctx, layer); err != nil {
		t.Fatalf("第二次 FlushAll 应幂等: %v", err)
	}
	if got := sn.count(); got != 1 {
		t.Fatalf("重复 FlushAll 不应重复落库: saved=%d want=1", got)
	}
}
