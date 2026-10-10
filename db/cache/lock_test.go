package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSharedLockCaches 构造两个共享同一 fakeKV 的 Cache（模拟两个进程实例），
// 都启用分布式锁并共享同一个 Loader——跨实例的回源次数即可被精确观测。
func newSharedLockCaches(t *testing.T, ld *mapLoader) (*Cache[string, testVal], *Cache[string, testVal], *fakeKV, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	kv := newFakeKV(clk)
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = time.Second
	cfg.LockPollInterval = 10 * time.Millisecond
	mk := func() *Cache[string, testVal] {
		c, err := New("t", kv,
			WithConfig[string, testVal](cfg),
			WithClock[string, testVal](clk.Now),
			WithLoader[string, testVal](ld),
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return c
	}
	return mk(), mk(), kv, clk
}

// 跨实例并发 miss：分布式锁保证同一时刻只有一个实例真正回源，
// 另一个实例轮询 Redis 到锁持有者回填的值后直接返回，不再打 DB。
func TestLock_ReadSerializesAcrossCaches(t *testing.T) {
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 42}}}
	cA, cB, _, _ := newSharedLockCaches(t, ld)

	var wg sync.WaitGroup
	var vA, vB *testVal
	var eA, eB error
	wg.Add(2)
	go func() { defer wg.Done(); vA, eA = cA.GetOrLoad(context.Background(), "k1") }()
	go func() { defer wg.Done(); vB, eB = cB.GetOrLoad(context.Background(), "k1") }()
	wg.Wait()

	if eA != nil || eB != nil {
		t.Fatalf("GetOrLoad 错误: A=%v B=%v", eA, eB)
	}
	if vA == nil || vA.N != 42 || vB == nil || vB.N != 42 {
		t.Fatalf("值不对: A=%+v B=%+v", vA, vB)
	}
	if got := ld.callCount(); got != 1 {
		t.Fatalf("跨实例回源 %d 次，分布式锁应合并为 1", got)
	}
}

// 禁用分布式锁（LockTTL=0）时，读线回源不经过 setnx 抢锁。
func TestLock_DisabledFallsBackToNoLock(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 7}}}
	cfg := testConfig()
	cfg.LockTTL = 0 // 显式禁用
	cA, err := New("t", kv, WithConfig[string, testVal](cfg), WithClock[string, testVal](clk.Now), WithLoader[string, testVal](ld))
	if err != nil {
		t.Fatal(err)
	}
	cB, err := New("t", kv, WithConfig[string, testVal](cfg), WithClock[string, testVal](clk.Now), WithLoader[string, testVal](ld))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cA.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatalf("A GetOrLoad: %v", err)
	}
	if _, err := cB.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatalf("B GetOrLoad: %v", err)
	}
	for _, op := range kv.opsOf() {
		if strings.HasPrefix(op, "setnx:") {
			t.Fatalf("锁禁用时不应出现 setnx 抢锁，ops=%v", kv.opsOf())
		}
	}
}

// 锁被其他进程长期持有时，读线在 LockWait 内抢不到锁返回 ErrLockTimeout。
func TestLock_ReadTimeout(t *testing.T) {
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 42}}}
	clk := newFakeClock()
	kv := newFakeKV(clk)
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = 50 * time.Millisecond
	cfg.LockPollInterval = 5 * time.Millisecond
	c, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 预占用锁：模拟另一个进程长时间持有
	lockKey := c.KeyOf("k1") + ":lock"
	if err := kv.Set(context.Background(), lockKey, "other-process", time.Hour); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrLoad(context.Background(), "k1")
		done <- err
	}()
	time.Sleep(80 * time.Millisecond) // 让 GetOrLoad 进入轮询等待
	clk.Add(100 * time.Millisecond)   // 推进假时钟，使 deadline 过期

	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("期望 ErrLockTimeout，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetOrLoad 未在锁等待超时后返回")
	}
}

// 写路径抢锁：Write 前出现 setnx(lockKey)，完成后锁被原子释放。
func TestLock_WriteAcquiresAndReleases(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	sn := newRecSaver()
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = time.Second
	c, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ks := c.KeyOf("k1")
	lockKey := ks + ":lock"
	if _, ok := kv.rawOf(lockKey); ok {
		t.Fatal("写前锁 key 不应存在")
	}
	if err := c.Write(context.Background(), "k1", &testVal{N: 1}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := kv.rawOf(lockKey); ok {
		t.Fatal("Write 完成后锁未释放（应为比较删除）")
	}
	found := false
	for _, op := range kv.opsOf() {
		if op == "setnx:"+lockKey {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Write 路径未抢分布式锁，ops=%v", kv.opsOf())
	}
}

// 锁被占时写路径同样在 LockWait 后返回 ErrLockTimeout。
func TestLock_WriteTimeout(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	sn := newRecSaver()
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = 50 * time.Millisecond
	cfg.LockPollInterval = 5 * time.Millisecond
	c, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lockKey := c.KeyOf("k1") + ":lock"
	if err := kv.Set(context.Background(), lockKey, "other-process", time.Hour); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Write(context.Background(), "k1", &testVal{N: 1})
	}()
	time.Sleep(80 * time.Millisecond)
	clk.Add(100 * time.Millisecond)

	select {
	case err := <-done:
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatalf("期望 ErrLockTimeout，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write 未在锁等待超时后返回")
	}
}