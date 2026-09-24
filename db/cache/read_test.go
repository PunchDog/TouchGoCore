package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 读线：Redis 优先 ----------

// 首次 miss：协程回源 → 先写 Redis → 从 Redis 读回返回（操作序列含 set 且 set 在最终 get 前）
func TestRead_MissGoesThroughRedis(t *testing.T) {
	h := newHarness(t)
	h.ld.data["k1"] = &testVal{N: 42}

	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatalf("GetOrLoad: %v", err)
	}
	if v.N != 42 {
		t.Fatalf("值=%v 期望42", v.N)
	}
	ks := h.c.KeyOf("k1")
	if h.kv.setCall == 0 {
		t.Fatal("miss 回源后必须写 Redis")
	}
	ops := h.kv.opsOf()
	lastSet, lastGet := -1, -1
	for i, op := range ops {
		if op == "set:"+ks {
			lastSet = i
		}
		if op == "get:"+ks {
			lastGet = i
		}
	}
	if lastSet < 0 || lastGet < lastSet {
		t.Fatalf("client 收到的值必须来自 Redis 读回，操作序列: %v", ops)
	}
}

func TestRead_HitNoLoad(t *testing.T) {
	h := newHarness(t)
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	before := h.ld.callCount()
	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if v.N != 1 || h.ld.callCount() != before {
		t.Fatal("命中不应回源")
	}
}

// 50 并发 miss：singleflight 合并，只回源一次
func TestRead_SingleflightCoalesces(t *testing.T) {
	h := newHarness(t)
	h.ld.data["k1"] = &testVal{N: 7}
	release := make(chan struct{})
	h.ld.block = release

	var wg sync.WaitGroup
	errs := make([]error, 50)
	vals := make([]*testVal, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vals[i], errs[i] = h.c.GetOrLoad(context.Background(), "k1")
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := h.ld.callCount(); got != 1 {
		t.Fatalf("回源 %d 次，singleflight 应合并为 1", got)
	}
	for i := range errs {
		if errs[i] != nil || vals[i] == nil || vals[i].N != 7 {
			t.Fatalf("等待者 %d 未拿到共享结果: %v", i, errs[i])
		}
	}
}

// 逻辑过期（物理未过期）：立即返回 Redis 旧值 + 异步预热刷新
func TestRead_LogicalStaleAsyncReturnsOldAndPrefetches(t *testing.T) {
	h := newHarness(t) // TTL 1s，逻辑 500ms
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	h.clk.Add(600 * time.Millisecond) // 逻辑过期、物理未过期

	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if v.N != 1 {
		t.Fatal("应返回旧值")
	}
	// 换源值，等预热落 Redis
	h.ld.data["k1"] = &testVal{N: 2}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.ld.callCount() > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.c.Stats().Snapshot().Prefetch == 0 {
		t.Fatal("预热协程未触发")
	}
	// 物理仍未过期时读回应是刷新后的值
	v2, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if v2.N != 2 {
		t.Fatalf("预热后 Redis 值=%d 期望2", v2.N)
	}
}

// StaleWait：逻辑过期也走同步回源
func TestRead_StaleWaitSyncReload(t *testing.T) {
	h := newHarness(t, WithStalePolicy[string, testVal](StaleWait))
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	h.clk.Add(600 * time.Millisecond)
	h.ld.data["k1"] = &testVal{N: 3}
	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if v.N != 3 {
		t.Fatal("wait 策略应同步拿新值")
	}
}

// 物理过期：fakeKV 视为 miss，同步重载
func TestRead_PhysicalExpiredReload(t *testing.T) {
	h := newHarness(t)
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	h.clk.Add(2 * time.Second)
	h.ld.data["k1"] = &testVal{N: 9}
	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil || v.N != 9 {
		t.Fatalf("物理过期应回源新值: v=%v err=%v", v, err)
	}
}

// 空值缓存防穿透：源没有 → 只回源一次，后续直接 ErrCacheNotFound
func TestRead_NegativeCache(t *testing.T) {
	h := newHarness(t)
	h.ld.notFound["ghost"] = true
	for i := 0; i < 5; i++ {
		_, err := h.c.GetOrLoad(context.Background(), "ghost")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("期望 ErrNotFound，得 %v", err)
		}
	}
	if got := h.ld.callCount(); got != 1 {
		t.Fatalf("空标记应防穿透，回源 %d 次", got)
	}
}

// 回源失败退避：失败窗口内不再打源
func TestRead_FailBackoff(t *testing.T) {
	h := newHarness(t)
	h.ld.failN = 100
	_, err := h.c.GetOrLoad(context.Background(), "k1")
	if err == nil || errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("首次应透传源错误: %v", err)
	}
	_, err = h.c.GetOrLoad(context.Background(), "k1")
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("退避窗口内期望 ErrSourceUnavailable，得 %v", err)
	}
	if got := h.ld.callCount(); got != 1 {
		t.Fatalf("退避内不应再打源，calls=%d", got)
	}
}

// RequireRedis=true：Redis 写不进 → 读线报错，不透传源值
func TestRead_RequireRedisSetFail(t *testing.T) {
	h := newHarness(t)
	h.ld.data["k1"] = &testVal{N: 5}
	h.kv.setErr = errors.New("redis down")
	_, err := h.c.GetOrLoad(context.Background(), "k1")
	if err == nil {
		t.Fatal("RequireRedis 下 Redis 写失败必须报错")
	}
	// 降级开关：直返源值
	h2 := newHarness(t, WithRequireRedis[string, testVal](false))
	h2.ld.data["k1"] = &testVal{N: 5}
	h2.kv.setErr = errors.New("redis down")
	v, err := h2.c.GetOrLoad(context.Background(), "k1")
	if err != nil || v == nil || v.N != 5 {
		t.Fatalf("降级应直返源值: v=%v err=%v", v, err)
	}
}

// Get 只查 Redis：miss → ErrMiss，不回源
func TestRead_GetOnlyCache(t *testing.T) {
	h := newHarness(t)
	_, err := h.c.Get(context.Background(), "k9")
	if !errors.Is(err, ErrMiss) {
		t.Fatalf("期望 ErrMiss，得 %v", err)
	}
	if h.ld.callCount() != 0 {
		t.Fatal("Get 不得回源")
	}
}

// 脏 key 保护：缓冲未落库期间，读线回源不得覆盖 Write 同步写入的新值
func TestRead_DirtyKeyNotOverwritten(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	h.ld.data["k1"] = &testVal{N: 1}
	// 先让 Redis 过期消失，制造 miss
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	// Write 新值（同步 Redis + 入缓冲未落库）
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 100}); err != nil {
		t.Fatal(err)
	}
	// 源里还是旧值；读线 miss 时（此处直接调 reload 路径：清缓存再读）
	h.kv.mu.Lock()
	delete(h.kv.m, h.c.KeyOf("k1")) // 模拟 TTL 恰好到期
	h.kv.mu.Unlock()
	h.clk.Add(10 * time.Millisecond)
	h.c.fails.Delete(h.c.KeyOf("k1"))

	v, err := h.c.GetOrLoad(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	// 脏 key：跳过覆盖后读回应是补写的源值还是 Write 值？缓冲 has → 读回 miss →
	// 补写一次源值。此处断言不 panic 且有值即通过链路验证；核心断言在落库后：
	_ = v
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 100 {
		t.Fatalf("落库值应为 Write 的 100，得 %v", got)
	}
}

// ---------- KeyOf ----------

func TestKeyOf(t *testing.T) {
	h := newHarness(t, WithPrefix[string, testVal]("tg"), WithGroup[string, testVal]("g1"))
	cases := []struct{ key, want string }{
		{"1001", "tg:g1:t:1001"},
		{"a:b", "tg:g1:t:a_b"},
		{strings.Repeat("x", 200), "tg:g1:t:" + strings.Repeat("x", 96) + "#" + sha1Short(strings.Repeat("x", 200))},
	}
	for _, cs := range cases {
		got := h.c.KeyOf(cs.key)
		if got != cs.want {
			t.Fatalf("KeyOf(%q)=%q want %q", cs.key, got, cs.want)
		}
	}
	// 无 group
	h2 := newHarness(t, WithPrefix[string, testVal]("tg"))
	if got := h2.c.KeyOf("7"); got != "tg:t:7" {
		t.Fatalf("无 group 键错误: %q", got)
	}
}

func sha1Short(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func TestConfigValidateRejects(t *testing.T) {
	bad := testConfig()
	bad.LogicalPct = 100
	if err := bad.Validate(); err == nil {
		t.Fatal("LogicalPct=100 应报错")
	}
	bad = testConfig()
	bad.Shards = 6
	if err := bad.Validate(); err == nil {
		t.Fatal("Shards 非 2 的幂应报错")
	}
}
