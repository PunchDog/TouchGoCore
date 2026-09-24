package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ---------- 写线：同步写 Redis + 定时批量落库 ----------

// Write 必须先同步写 Redis 成功、saver 此时未被调用（落库交给定时器）
func TestWrite_SyncRedisBeforeSaver(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 10}); err != nil {
		t.Fatal(err)
	}
	if h.kv.setCall == 0 {
		t.Fatal("Write 必须同步写 Redis")
	}
	if s, _ := sn.counts(); s != 0 {
		t.Fatal("Write 不应同步落库")
	}
	v, err := h.c.Get(context.Background(), "k1")
	if err != nil || v.N != 10 {
		t.Fatalf("写后应能从 Redis 读到新值: v=%v err=%v", v, err)
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 10 {
		t.Fatalf("Flush 后应落库: %v", got)
	}
	if s, _ := sn.counts(); s != 1 {
		t.Fatalf("saver 调用 %d 次期望1", s)
	}
}

// 同 key 100 次 Write，flush 后 saver 只收到 1 条最新值（map 去重）
func TestWrite_DedupLatestWins(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	for i := 1; i <= 100; i++ {
		if err := h.c.Write(context.Background(), "k1", &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 1 {
		t.Fatalf("去重后脏键数=%d 期望1", got)
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 100 {
		t.Fatalf("应落最新值100，得 %v", got)
	}
	if s, _ := sn.counts(); s != 1 {
		t.Fatalf("同 key 去重后只落 1 条，saves=%d", s)
	}
}

// Redis 写失败 → Write 报错且不入写缓冲
func TestWrite_KVFailNotBuffered(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	h.kv.setErr = errors.New("redis down")
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 1}); err == nil {
		t.Fatal("Redis 写失败时 Write 应报错")
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("失败写入不应入缓冲，dirty=%d", got)
	}
}

// BatchSize 切批
func TestWrite_BatchSplit(t *testing.T) {
	bs := newRecBatchSaver()
	h := newHarness(t, WithSaver[string, testVal](bs), WithWriteBehind[string, testVal](time.Hour, 50))
	for i := 0; i < 120; i++ {
		k := string(rune('a'+i%26)) + string(rune('A'+i/26))
		if err := h.c.Write(context.Background(), k, &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	bs.mu.Lock()
	sizes := append([]int(nil), bs.batchSizes...)
	bs.mu.Unlock()
	if len(sizes) != 3 || sizes[0] != 50 || sizes[1] != 50 || sizes[2] != 20 {
		t.Fatalf("120 条 BatchSize=50 应切 50/50/20 三批，得 %v", sizes)
	}
}

// 落库失败回插重试，成功后新值可查
func TestWrite_RetryThenSuccess(t *testing.T) {
	sn := newRecSaver()
	sn.failFn = func(n int) error {
		if n == 1 {
			return errors.New("db busy")
		}
		return nil
	}
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 5}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 1 {
		t.Fatalf("失败应回插，dirty=%d", got)
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 5 {
		t.Fatalf("重试后应落库: %v", got)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("落库成功 dirty 应清零，得 %d", got)
	}
}

// 超过 MaxRetry：丢弃 + Del 缓存（绝不留未落库值）
func TestWrite_MaxRetryDropAndInvalidate(t *testing.T) {
	sn := newRecSaver()
	sn.saveErr = errors.New("db down forever")
	cfg := testConfig()
	cfg.MaxRetry = 1
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 5}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Flush(context.Background()); err != nil { // 第1轮失败→回插
		t.Fatal(err)
	}
	if err := h.c.Flush(context.Background()); err != nil { // 第2轮超 MaxRetry→丢弃
		t.Fatal(err)
	}
	st := h.c.Stats().Snapshot()
	if st.Dropped != 1 {
		t.Fatalf("应丢弃1条，Dropped=%d", st.Dropped)
	}
	if st.Dirty != 0 {
		t.Fatalf("丢弃后 dirty=%d 期望0", st.Dirty)
	}
	if _, err := h.c.Get(context.Background(), "k1"); !errors.Is(err, ErrMiss) {
		t.Fatalf("丢弃必须同时失效缓存，err=%v", err)
	}
}

// Remove：同步 DEL + tombstone 落库删除
func TestWrite_RemoveTombstone(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	h.ld.data["k1"] = &testVal{N: 1}
	if _, err := h.c.GetOrLoad(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Remove(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Get(context.Background(), "k1"); !errors.Is(err, ErrMiss) {
		t.Fatalf("Remove 应同步失效缓存: %v", err)
	}
	if _, d := sn.counts(); d != 0 {
		t.Fatal("落库删除应等定时器")
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, d := sn.counts(); d != 1 {
		t.Fatalf("tombstone 应落库删除，deletes=%d", d)
	}
}

// Run 定时器：ctx 取消触发 final flush（用独立 ctx 落库）
func TestWrite_RunFinalFlush(t *testing.T) {
	sn := newRecSaver()
	cfg := testConfig()
	cfg.FlushInterval = time.Hour // 定时器不会自己响，只验 final
	h := newHarness(t, WithSaver[string, testVal](sn), WithConfig[string, testVal](cfg))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.c.Run(ctx) }()

	if err := h.c.Write(context.Background(), "k1", &testVal{N: 66}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if s, _ := sn.counts(); s != 0 {
		t.Fatal("未到 FlushInterval 不应落库")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run 未在 ctx 取消后退出")
	}
	if got := sn.get("k1"); got == nil || got.N != 66 {
		t.Fatalf("final flush 应落库: %v", got)
	}
}

// Write 然后 WriteSync：Flush 不得用旧缓冲值盖掉 WriteSync 已落库的行。
func TestWrite_WriteThenWriteSyncFlushKeepsSyncValue(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	if err := h.c.Write(ctx, "k", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.WriteSync(ctx, "k", &testVal{N: 99}); err != nil {
		t.Fatal(err)
	}
	if v := sn.get("k"); v == nil || v.N != 99 {
		t.Fatalf("WriteSync 后库值应为 99: %v", v)
	}
	if snap := h.c.Stats().Snapshot(); snap.Dirty != 0 {
		t.Fatalf("WriteSync 应踢掉旧缓冲，Dirty=%d", snap.Dirty)
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 0 {
		t.Fatalf("WriteSync 应销掉该键账本: %v", jm)
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if v := sn.get("k"); v == nil || v.N != 99 {
		t.Fatalf("Flush 后仍应为 WriteSync 的 99，得 %v", v)
	}
	if s, _ := sn.counts(); s != 1 {
		t.Fatalf("Flush 不应再 Save 旧缓冲，saves=%d", s)
	}
}

// drain 后并发更高 seq 的 Write：失败回插不得覆盖新值；Dirty 正确收敛。
func TestWrite_ReinsertKeepsNewerSeq(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	if err := h.c.Write(ctx, "k", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	// 手工 drain 出旧条目，再 Write 更新值，模拟 flush 中途并发写
	old := h.c.buf.drain()
	if len(old) != 1 {
		t.Fatalf("期望 drain 1 条，得 %d", len(old))
	}
	if err := h.c.Write(ctx, "k", &testVal{N: 2}); err != nil {
		t.Fatal(err)
	}
	n := h.c.buf.reinsert(old)
	if n != 1 {
		t.Fatalf("旧条目应被丢弃，discarded=%d", n)
	}
	h.c.st.Dirty.Add(-int64(n))
	be := h.c.buf.get(h.c.KeyOf("k"))
	if be == nil || be.val == nil || be.val.N != 2 {
		t.Fatalf("缓冲应保留新值 2: %+v", be)
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if v := sn.get("k"); v == nil || v.N != 2 {
		t.Fatalf("落库应为新值 2: %v", v)
	}
}

// 停服 final flush DB 失败：保留账本与 Redis，返回错误；新 Run 可恢复落库。
func TestWrite_FinalFlushFailKeepsJournalForRecover(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	sn := newRecSaver()
	sn.saveErr = errors.New("db down")
	cfg := testConfig()
	cfg.FlushInterval = time.Hour
	cfg.MaxRetry = 0 // 正常路径会丢弃；final 不得走丢弃
	ld := &mapLoader{data: map[string]*testVal{}, notFound: map[string]bool{}}
	c1, err := New[string, testVal]("t", kv,
		WithConfig[string, testVal](cfg), WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld), WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c1.Write(ctx, "k", &testVal{N: 42}); err != nil {
		t.Fatal(err)
	}
	if err := c1.flushOnce(ctx, true); err == nil {
		t.Fatal("final flush DB 失败应返回错误")
	}
	if jm := kv.journalOf(c1.jz); len(jm) == 0 {
		t.Fatal("停服失败不得清账")
	}
	if _, err := c1.Get(ctx, "k"); err != nil {
		t.Fatalf("停服失败不得 Del Redis: %v", err)
	}
	if snap := c1.Stats().Snapshot(); snap.Dropped != 0 {
		t.Fatalf("停服失败不是放弃，Dropped=%d", snap.Dropped)
	}

	// 新进程：DB 恢复 → Run 扫账重放
	sn2 := newRecSaver()
	c2 := journalHarness(t, kv, clk, sn2)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = c2.Run(rctx); close(done) }()
	waitFor(t, 2*time.Second, func() bool {
		return sn2.get("k") != nil && sn2.get("k").N == 42
	}, "新 Run 应恢复落库")
	cancel()
	<-done
	if jm := kv.journalOf(c2.jz); len(jm) != 0 {
		t.Fatalf("恢复成功应销账: %v", jm)
	}
}

// Enabled=false 时 Remove 同步 Delete，不留永不 flush 的 tombstone。
func TestWrite_DisabledRemoveDeletes(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn), WithEnabled[string, testVal](false))
	if err := h.c.Remove(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if _, d := sn.counts(); d != 1 {
		t.Fatalf("关闭模式 Remove 应同步 Delete，deletes=%d", d)
	}
	if snap := h.c.Stats().Snapshot(); snap.Dirty != 0 {
		t.Fatalf("关闭模式不应入缓冲，Dirty=%d", snap.Dirty)
	}
}

// 无 Saver 时 Write 报错
func TestWrite_NoSaver(t *testing.T) {
	h := newHarness(t)
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 1}); err == nil {
		t.Fatal("未配置 Saver 的 Write 应报错")
	}
}

// ---------- MGet ----------

func TestRead_MGetOrLoad(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		k := string(rune('a' + i))
		h.ld.data[k] = &testVal{N: i}
	}
	keys := []string{"a", "b", "c", "d", "e", "zz"}
	got, err := h.c.MGetOrLoad(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("命中5个期望，得 %d", len(got))
	}
	if _, ok := got[h.c.KeyOf("zz")]; ok {
		t.Fatal("源不存在的键不应出现")
	}
	// 第二次全命中缓存
	before := h.ld.callCount()
	if _, err := h.c.MGetOrLoad(context.Background(), keys[:5]); err != nil {
		t.Fatal(err)
	}
	if h.ld.callCount() != before {
		t.Fatal("已回填的键不应再回源")
	}
}

// ---------- 并发压一下（无 -race，至少别炸） ----------

func TestConcurrent读写混合(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _ = h.c.GetOrLoad(context.Background(), "x")
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = h.c.Write(context.Background(), "x", &testVal{N: i})
		}(i)
	}
	wg.Wait()
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 0 {
		t.Fatalf("dirty=%d", got)
	}
}
