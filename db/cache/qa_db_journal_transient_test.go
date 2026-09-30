package cache

import (
	"context"
	"errors"
	"testing"
)

// ==================== P2：journal 恢复须区分「值消失」与「Redis 瞬态错误」 ====================
//
// 事故形状：recoverJournal 对 c.kv.Get 的任何错误都当作「envelope 已消失」→
// JClear 永久销账。Redis 瞬断（网络/超时）会被误判为数据永久丢失，账本被清空，
// 未落库的脏数据再也无法恢复。修复：仅 ErrNoEntry 才销账 + RecoverMiss；
// 瞬态错误 → JournalErr 计数 + Warning 留账，continue（与 Save 失败分支同语义）。

func TestRecoverJournal_TransientGetErrorKeepsEntry(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	c1 := journalHarness(t, kv, clk, newRecSaver())
	if err := c1.Write(ctx, "k1", &testVal{N: 7}); err != nil {
		t.Fatal(err)
	}
	if jm := kv.journalOf(c1.jz); len(jm) != 1 {
		t.Fatalf("前置：账本应有 1 条，得 %v", jm)
	}

	// 「重启」+ Redis 瞬断：Get 报非 ErrNoEntry 的错误
	kv.getErr = errors.New("dial tcp: i/o timeout")
	sn2 := newRecSaver()
	c2 := journalHarness(t, kv, clk, sn2)
	c2.recoverJournal(ctx)

	if jm := kv.journalOf(c2.jz); len(jm) != 1 {
		t.Fatalf("瞬态错误不得销账（误销=数据永久丢失）: %v", jm)
	}
	if s, _ := sn2.counts(); s != 0 {
		t.Fatalf("读不到值不该落库, saves=%d", s)
	}
	if snap := c2.Stats().Snapshot(); snap.RecoverMiss != 0 || snap.Recovered != 0 {
		t.Fatalf("瞬态错误不算不可恢复: %+v", snap)
	}

	// Redis 恢复后同一条账可正常重放
	kv.getErr = nil
	sn3 := newRecSaver()
	c3 := journalHarness(t, kv, clk, sn3)
	c3.recoverJournal(ctx)
	if s, _ := sn3.counts(); s != 1 {
		t.Fatalf("恢复后应重放 1 条, saves=%d", s)
	}
	if v := sn3.get("k1"); v == nil || v.N != 7 {
		t.Fatalf("重放值错误: %+v", v)
	}
	if jm := kv.journalOf(c3.jz); len(jm) != 0 {
		t.Fatalf("重放成功应销账: %v", jm)
	}
}

func TestRecoverJournal_NoEntryStillClears(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	ctx := context.Background()

	c1 := journalHarness(t, kv, clk, newRecSaver())
	if err := c1.Write(ctx, "k1", &testVal{N: 7}); err != nil {
		t.Fatal(err)
	}

	// envelope 真消失（TTL 早于重启过期）：维持现状——销账止损 + RecoverMiss
	kv.getErr = ErrNoEntry
	sn2 := newRecSaver()
	c2 := journalHarness(t, kv, clk, sn2)
	c2.recoverJournal(ctx)

	if jm := kv.journalOf(c2.jz); len(jm) != 0 {
		t.Fatalf("值确已消失应销账: %v", jm)
	}
	if snap := c2.Stats().Snapshot(); snap.RecoverMiss != 1 {
		t.Fatalf("应记 RecoverMiss: %+v", snap)
	}
}
