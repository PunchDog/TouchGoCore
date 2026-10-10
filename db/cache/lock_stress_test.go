package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 多进程（多 Cache 实例共享同一 Redis）并发 miss：分布式锁保证只有一个实例真正回源，
// 其余实例轮询到值后返回——互斥成立且无人卡死。
func TestLock_ManyProcesses_ReadMutualExclusion(t *testing.T) {
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 42}}}
	clk := newFakeClock()
	kv := newFakeKV(clk)
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = 2 * time.Second
	cfg.LockPollInterval = 2 * time.Millisecond

	const n = 8
	caches := make([]*Cache[string, testVal], n)
	for i := range caches {
		c, err := New("t", kv,
			WithConfig[string, testVal](cfg),
			WithClock[string, testVal](clk.Now),
			WithLoader[string, testVal](ld),
		)
		if err != nil {
			t.Fatalf("New[%d]: %v", i, err)
		}
		caches[i] = c
	}

	start := make(chan struct{})
	errs := make([]error, n)
	vals := make([]*testVal, n)
	var wg sync.WaitGroup
	for i := range caches {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			vals[i], errs[i] = caches[i].GetOrLoad(context.Background(), "k1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("cache[%d] GetOrLoad 错误: %v", i, errs[i])
		}
		if vals[i] == nil || vals[i].N != 42 {
			t.Fatalf("cache[%d] 值不对: %+v", i, vals[i])
		}
	}
	if got := ld.callCount(); got != 1 {
		t.Fatalf("%d 个进程并发 miss 回源 %d 次，分布式锁应合并为 1", n, got)
	}
}

// 持有者崩溃（只留锁不回填）：锁 TTL 到期后等待进程必须能接管，不得卡死。
func TestLock_TTLExpiry_RecoversFromCrashedHolder(t *testing.T) {
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 9}}}
	clk := newFakeClock()
	kv := newFakeKV(clk)
	cfg := testConfig()
	cfg.LockTTL = 100 * time.Millisecond
	cfg.LockWait = 500 * time.Millisecond
	cfg.LockPollInterval = 5 * time.Millisecond
	c, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// 模拟另一个进程抢到锁后崩溃：锁在，数据永远不回填。
	lockKey := c.KeyOf("k1") + ":lock"
	if err := kv.Set(context.Background(), lockKey, "crashed-process", cfg.LockTTL); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	var got *testVal
	go func() {
		var err error
		got, err = c.GetOrLoad(context.Background(), "k1")
		done <- err
	}()

	// 等它进入轮询等待，再推进假时钟使崩溃进程的锁过期。
	time.Sleep(60 * time.Millisecond)
	clk.Add(200 * time.Millisecond) // 锁 TTL=100ms 已过

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("锁过期后应能接管并回源，得到错误 %v", err)
		}
		if got == nil || got.N != 9 {
			t.Fatalf("接管后值不对: %+v", got)
		}
		if ld.callCount() != 1 {
			t.Fatalf("回源次数=%d，期望 1", ld.callCount())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("崩溃持有者的锁过期后，等待进程仍卡死（未接管）")
	}
}

// 回源出错时锁必须释放（defer 兜底），后续进程能正常接管，不因残留锁卡死。
func TestLock_ReleasedOnReadError(t *testing.T) {
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 5}}, err: errors.New("source down"), failN: 1}
	clk := newFakeClock()
	kv := newFakeKV(clk)
	cfg := testConfig()
	cfg.LockTTL = time.Second
	cfg.LockWait = time.Second
	cA, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
	)
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	cB, err := New("t", kv,
		WithConfig[string, testVal](cfg),
		WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld),
	)
	if err != nil {
		t.Fatalf("New B: %v", err)
	}

	// A 回源失败：应返回错误，且锁被释放。
	if _, err := cA.GetOrLoad(context.Background(), "k1"); err == nil {
		t.Fatal("A 首次回源应失败")
	}
	lockKey := cA.KeyOf("k1") + ":lock"
	if _, ok := kv.rawOf(lockKey); ok {
		t.Fatal("回源失败后锁未释放（残留锁会卡死后续进程）")
	}

	// B 随后能正常接管并成功。
	if _, err := cB.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatalf("B 应能接管成功: %v", err)
	}
}

// 写路径互斥：锁被占用时 Write 必须等待，锁释放后继续完成，绝不提前返回或卡死。
func TestLock_WriteWaitsWhileHeld(t *testing.T) {
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

	lockKey := c.KeyOf("k1") + ":lock"
	if err := kv.Set(context.Background(), lockKey, "other-process", time.Hour); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- c.Write(context.Background(), "k1", &testVal{N: 1}) }()

	// 短延迟内 Write 不应完成（被锁挡住 → 说明写路径确实在抢锁串行）。
	select {
	case err := <-done:
		t.Fatalf("锁被占用时 Write 不应立即完成: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	// 释放锁后 Write 应继续并完成。
	if err := kv.Del(context.Background(), lockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("释放锁后 Write 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("释放锁后 Write 仍卡住（写路径未从等待中恢复）")
	}
}

// 混合读写压力：多个进程反复读写同一批键，所有操作必须在有界时间内完成，
// 任何进程都不允许因抢锁卡死；结束后锁全部释放。
func TestLock_MixedReadWriteStress_NoHang(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ld := &mapLoader{data: map[string]*testVal{"k1": {N: 1}}}
	sn := newRecSaver()
	cfg := testConfig()
	cfg.LockTTL = 500 * time.Millisecond
	cfg.LockWait = 300 * time.Millisecond
	cfg.LockPollInterval = 2 * time.Millisecond

	const n = 6
	caches := make([]*Cache[string, testVal], n)
	for i := range caches {
		c, err := New("t", kv,
			WithConfig[string, testVal](cfg),
			WithClock[string, testVal](clk.Now),
			WithLoader[string, testVal](ld),
			WithSaver[string, testVal](sn),
		)
		if err != nil {
			t.Fatalf("New[%d]: %v", i, err)
		}
		caches[i] = c
	}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := range caches {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				for j := 0; j < 3; j++ {
					_, _ = caches[i].GetOrLoad(context.Background(), "k1")
					_ = caches[i].Write(context.Background(), "k2", &testVal{N: i})
					_ = caches[i].Remove(context.Background(), "k3")
				}
			}(i)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 全部完成：没有进程因抢锁卡死。
	case <-time.After(8 * time.Second):
		t.Fatal("混合读写压力下仍有进程因抢锁卡死（8s 未完成）")
	}

	// 结束后所有锁都应已释放（原子比较删除），不残留。
	for _, key := range []string{"k1", "k2", "k3"} {
		lockKey := caches[0].KeyOf(key) + ":lock"
		if _, ok := kv.rawOf(lockKey); ok {
			t.Fatalf("压力结束后锁未释放: %s", lockKey)
		}
	}
}