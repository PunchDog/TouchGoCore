package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Write 同步写 Redis 时同批记账；Flush 成功即销账。
func TestJournal_WriteBooksFlushClears(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if h.c.jr == nil {
		t.Fatal("fakeKV 实现了 Journaler，账本应自动启用")
	}
	if err := h.c.Write(context.Background(), "a", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	jm := h.kv.journalOf(h.c.jz)
	if len(jm) != 1 {
		t.Fatalf("Write 后账本应有 1 条: %v", jm)
	}
	if _, ok := jm["u|a"]; !ok {
		t.Fatalf("账本成员应为 u|a: %v", jm)
	}
	if err := h.c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 0 {
		t.Fatalf("落库成功必须销账: %v", jm)
	}
	if v := sn.get("a"); v == nil {
		t.Fatal("前置：已落库")
	}
}

// 同键重复写账本去重（ZSET member 唯一）。
func TestJournal_DedupSameKey(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	for i := 1; i <= 10; i++ {
		if err := h.c.Write(ctx, "k", &testVal{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 1 {
		t.Fatalf("10 次同键写账本应只有 1 条: %v", jm)
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if v := sn.get("k"); v == nil || v.N != 10 {
		t.Fatalf("flush 后落库应为最新值: %v", v)
	}
}

// op 翻转：Write→Remove 后账本只剩 tombstone；Remove→Write 只剩 upsert。
func TestJournal_OpSwitch(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()))
	ctx := context.Background()
	if err := h.c.Write(ctx, "x", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Remove(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	jm := h.kv.journalOf(h.c.jz)
	if _, ok := jm["u|x"]; ok {
		t.Fatalf("Remove 后 upsert 账必须被销: %v", jm)
	}
	if _, ok := jm["d|x"]; !ok {
		t.Fatalf("Remove 应记 tombstone 账: %v", jm)
	}
	if err := h.c.Write(ctx, "x", &testVal{N: 2}); err != nil {
		t.Fatal(err)
	}
	jm = h.kv.journalOf(h.c.jz)
	if len(jm) != 1 {
		t.Fatalf("Write 复活后只剩一条: %v", jm)
	}
	if _, ok := jm["u|x"]; !ok {
		t.Fatalf("应翻回 upsert 账: %v", jm)
	}
}

// journalHarness 构造一个共享 kv 的新 Cache（模拟进程重启）
func journalHarness(t *testing.T, kv *fakeKV, clk *fakeClock, sn Saver[string, testVal]) *Cache[string, testVal] {
	t.Helper()
	cfg := testConfig()
	cfg.FlushInterval = time.Hour
	ld := &mapLoader{data: map[string]*testVal{}, notFound: map[string]bool{}}
	c, err := New[string, testVal]("t", kv,
		WithConfig[string, testVal](cfg), WithClock[string, testVal](clk.Now),
		WithLoader[string, testVal](ld), WithSaver[string, testVal](sn),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// waitFor 轮询直到 cond 成立或超时
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

// 崩溃恢复：模拟「Write 后进程被杀（缓冲丢失、账本在）→ 重启」。
// 新 Cache 的 Run 启动即扫账，把 envelope 值重放落库并销账。
func TestJournal_RecoverUpsertAfterCrash(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	c1 := journalHarness(t, kv, clk, newRecSaver())
	for i := 0; i < 5; i++ {
		if err := c1.Write(ctx, string(rune('a'+i)), &testVal{N: 100 + i}); err != nil {
			t.Fatal(err)
		}
	}
	// 「崩溃」：c1 连同写缓冲直接丢弃

	sn2 := newRecSaver()
	c2 := journalHarness(t, kv, clk, sn2)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = c2.Run(rctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool {
		s, _ := sn2.counts()
		return s == 5 && len(kv.journalOf(c2.jz)) == 0
	}, "恢复未在时限内完成")
	cancel()
	<-done

	for i := 0; i < 5; i++ {
		if v := sn2.get(string(rune('a' + i))); v == nil || v.N != 100+i {
			t.Fatalf("重放值不对 %d: %v", i, v)
		}
	}
	if snap := c2.Stats().Snapshot(); snap.Recovered != 5 {
		t.Fatalf("Recovered 计数: %d", snap.Recovered)
	}
}

// 崩溃恢复 tombstone：Remove 后被杀 → 重启重放删除。
func TestJournal_RecoverTombstone(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	c1 := journalHarness(t, kv, clk, newRecSaver())
	if err := c1.Remove(ctx, "gone"); err != nil {
		t.Fatal(err)
	}

	sn2 := newRecSaver()
	c2 := journalHarness(t, kv, clk, sn2)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = c2.Run(rctx); close(done) }()

	waitFor(t, 2*time.Second, func() bool {
		_, d := sn2.counts()
		return d == 1 && len(kv.journalOf(c2.jz)) == 0
	}, "tombstone 恢复未执行")
	cancel()
	<-done
}

// envelope 已物理过期 → 不可恢复：RecoverMiss 计数 + 销账止损。
func TestJournal_RecoverMissWhenExpired(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	c1 := journalHarness(t, kv, clk, newRecSaver())
	if err := c1.Write(ctx, "lost", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	clk.Add(2 * time.Second) // 物理 TTL(1s) 过期

	c2 := journalHarness(t, kv, clk, newRecSaver())
	c2.recoverJournal(ctx)

	if snap := c2.Stats().Snapshot(); snap.RecoverMiss != 1 || snap.Recovered != 0 {
		t.Fatalf("过期账应记 RecoverMiss: %+v", snap)
	}
	if jm := kv.journalOf(c2.jz); len(jm) != 0 {
		t.Fatalf("不可恢复必须销账止损: %v", jm)
	}
}

// 落库超限丢弃时同步销账：框架已放弃的数据不该被重启复活。
func TestJournal_DropClearsBook(t *testing.T) {
	sn := newRecSaver()
	sn.saveErr = errors.New("db down")
	h := newHarness(t, WithSaver[string, testVal](sn), WithWriteBehind[string, testVal](time.Hour, 10))
	ctx := context.Background()
	if err := h.c.Write(ctx, "z", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= h.c.cfg.MaxRetry+1; i++ {
		if err := h.c.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if snap := h.c.Stats().Snapshot(); snap.Dropped != 1 {
		t.Fatalf("应丢弃 1 条: %+v", snap)
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 0 {
		t.Fatalf("丢弃必须销账: %v", jm)
	}
}

// WithJournal(false) 关闭账本：Write 不再记账。
func TestJournal_CanDisable(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()), WithJournal[string, testVal](false))
	if h.c.jr != nil {
		t.Fatal("显式关闭后不应启用账本")
	}
	if err := h.c.Write(context.Background(), "a", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	for _, op := range h.kv.opsOf() {
		if len(op) > 5 && op[:5] == "zadd:" {
			t.Fatalf("关闭账本后不应有 zadd 操作: %v", op)
		}
	}
}

// WriteSync：同步落库，返回前库里已可见；失败则回滚 Redis 不留展示值。
func TestWriteSync(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	if err := h.c.WriteSync(ctx, "s", &testVal{N: 7}); err != nil {
		t.Fatal(err)
	}
	if v := sn.get("s"); v == nil || v.N != 7 {
		t.Fatalf("WriteSync 返回前必须已落库: %v", v)
	}
	if snap := h.c.Stats().Snapshot(); snap.Dirty != 0 {
		t.Fatalf("WriteSync 不进写缓冲: %+v", snap)
	}
	// 落库失败 → 报错 + Redis 值被失效
	sn.saveErr = errors.New("db down")
	if err := h.c.WriteSync(ctx, "s2", &testVal{N: 8}); err == nil {
		t.Fatal("落库失败应报错")
	}
	if _, err := h.c.Get(ctx, "s2"); !errors.Is(err, ErrMiss) {
		t.Fatalf("失败后不应留下 Redis 展示值: err=%v", err)
	}
}

// 账本键形态：{keyBase}:dirty#zset。
func TestJournal_ZKeyIsolated(t *testing.T) {
	h := newHarness(t, WithSaver[string, testVal](newRecSaver()))
	if h.c.jz != "tg:t"+jDirtySuffix {
		t.Fatalf("账本键异常: %s", h.c.jz)
	}
	// 正常键经 sanitize 后 ":"→"_"，永远拼不出账本后缀里的 ":dirty#zset"，
	// 故 envelope 键与账本键不冲突。
	if h.c.KeyOf("x") == h.c.jz {
		t.Fatal("账本键不得与 envelope 键冲突")
	}
}
