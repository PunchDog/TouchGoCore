package cache

import (
	"context"
	"errors"
	"testing"
)

// ==================== FlushNow：资金级「立即落库」的诚实语义 ====================
//
// 反「静默 no-op」是这组用例的全部目的：Flush 会吞掉落库失败（回插等定时器重试），
// 所以 FlushNow 必须自己断言，否则它对调用方撒「已落库」的谎。

// 只挂 Loader 不挂 Saver（资金表的正确形态）：结构上不存在异步残留 → nil
func TestFlushNow_NoSaverMeansNothingPending(t *testing.T) {
	h := newHarness(t) // 无 WithSaver
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatalf("无写缓冲应为 nil: %v", err)
	}
	if h.c.Pending("k1") {
		t.Fatal("无缓冲不可能脏")
	}
}

func TestFlushNow_NilReceiver(t *testing.T) {
	var c *Cache[string, testVal]
	if err := c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if c.Pending("k1") {
		t.Fatal("nil 接收者不得 panic 或报脏")
	}
}

// 键当前不脏 → 快路返回 nil，绝不触发整表 flush（零成本）
func TestFlushNow_CleanKeySkipsFlush(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := sn.counts(); s != 0 {
		t.Fatalf("无脏数据时不该落库: saves=%d", s)
	}
}

func TestFlushNow_DrainsDirtyKey(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 42}); err != nil {
		t.Fatal(err)
	}
	if !h.c.Pending("k1") {
		t.Fatal("Write 后必须脏")
	}
	if s, _ := sn.counts(); s != 0 {
		t.Fatalf("Write 不该同步落库: saves=%d", s)
	}
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if got := sn.get("k1"); got == nil || got.N != 42 {
		t.Fatalf("FlushNow 后库里必须有值: %v", got)
	}
	if h.c.Pending("k1") {
		t.Fatal("落库成功后不得仍报脏")
	}
}

// 文档化的当前粒度：整 Cache flush，顺带把别的键也落了（多写库，不写错）
func TestFlushNow_FlushesWholeCacheAtCurrentGranularity(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	for _, k := range []string{"a", "b"} {
		if err := h.c.Write(context.Background(), k, &testVal{N: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.c.FlushNow(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if sn.get("a") == nil || sn.get("b") == nil {
		t.Fatalf("当前实现是整表粒度，两键都该落库: a=%v b=%v", sn.get("a"), sn.get("b"))
	}
}

// 核心：落库失败必须报错（而不是像 Flush 那样回插后返回 nil）
func TestFlushNow_HonestOnSaveFailure(t *testing.T) {
	sn := newRecSaver()
	sn.saveErr = errors.New("db 不可用")
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 1}); err != nil {
		t.Fatal(err)
	}
	err := h.c.FlushNow(context.Background(), "k1")
	if err == nil {
		t.Fatal("落库失败时 FlushNow 绝不能返回 nil")
	}
	if !errors.Is(err, ErrFlushNotDurable) {
		t.Fatalf("错误必须可判定为 ErrFlushNotDurable: %v", err)
	}
	if !h.c.Pending("k1") {
		t.Fatal("未落库的脏数据必须留在缓冲等重试（final 语义不丢弃）")
	}
	// final 语义保留 Redis：值仍在，账本也仍在，等下一次 flush 或重启恢复
	if _, getErr := h.c.Get(context.Background(), "k1"); getErr != nil {
		t.Fatalf("final flush 失败应保留 Redis 值: %v", getErr)
	}
	// 库恢复后可清账
	sn.saveErr = nil
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatalf("DB 恢复后应能清账: %v", err)
	}
	if got := sn.get("k1"); got == nil || got.N != 1 {
		t.Fatalf("清账后库里要有值: %v", got)
	}
}

// Enabled=false 的降级轨道：Write 已是同步 write-through，缓冲恒空 → nil 且确实落了库
func TestFlushNow_DisabledCacheWriteThrough(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn), WithEnabled[string, testVal](false))
	if err := h.c.Write(context.Background(), "k1", &testVal{N: 5}); err != nil {
		t.Fatal(err)
	}
	if s, _ := sn.counts(); s != 1 {
		t.Fatalf("降级路径 Write 必须同步落库: saves=%d", s)
	}
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if sn.get("k1") == nil {
		t.Fatal("值应已在库里")
	}
}

// tombstone（Remove）也归 FlushNow 清：删除同样不能只活在 Redis 里
func TestFlushNow_DrainsTombstone(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	if err := h.c.Remove(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if !h.c.Pending("k1") {
		t.Fatal("Remove 后应有 tombstone")
	}
	if err := h.c.FlushNow(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	if _, d := sn.counts(); d != 1 {
		t.Fatalf("tombstone 应落库删除: deletes=%d", d)
	}
}
