package cache

import (
	"context"
	"sync"
	"testing"
)

// ==================== 旧写残影修复（Write/Remove 的 !stored 分支） ====================
//
// 事故形状：A(seq=1) 取号后卡在 Redis 门口，B(seq=2) 完整落地；就在 A 抵达前
// 键恰好物理过期 → seqGuardLua 见不到现有值，放行了这个本该输的旧写。
// 缓冲那一侧 max-seq 正确地挡住了 A（stored=false），但 Redis 里已经留下
// A 的旧值并续上了新 TTL——不修的话这个旧值会一直展示到逻辑过期。
//
// 修复：!stored 分支用缓冲里的更新条目当场 CAS 纠正 Redis。

// TestWrite_StaleAdmittedAfterExpiry_RepairedToBuffered 旧写因键过期被 CAS 放行后，
// Redis 必须被立刻纠正回缓冲里的新值。
func TestWrite_StaleAdmittedAfterExpiry_RepairedToBuffered(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	aAtGate := make(chan struct{})
	releaseA := make(chan struct{})
	var once sync.Once
	h.kv.seqGate = func(_ string, seq int64) {
		if seq != 1 {
			return
		}
		once.Do(func() { close(aAtGate) })
		<-releaseA
	}

	aDone := make(chan error, 1)
	go func() { aDone <- h.c.Write(ctx, "x", &testVal{N: 1}) }()
	waitGate(t, aAtGate, "A 取到 seq=1 并抵达 Redis 门口")

	if err := h.c.Write(ctx, "x", &testVal{N: 2}); err != nil { // B(seq=2) 完整落地
		t.Fatalf("B 写入失败: %v", err)
	}
	// 制造交错：键物理过期，于是 A 的 CAS 见不到 seq=2 而放行
	h.kv.hardDel(ks)

	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("A 不该报错: %v", err)
	}

	// 断言修复：Redis 必须停回 seq=2 的值（修复前这里是 seq=1/N=1 的残影）
	e := mustEnvelope(t, h, ks)
	if e.Seq != 2 || e.Value.N != 2 {
		t.Fatalf("旧写残影未被纠正: Redis seq=%d N=%d", e.Seq, e.Value.N)
	}
	if be := bufferEntry(h, ks); be == nil || be.seq != 2 || be.val.N != 2 {
		t.Fatalf("写缓冲应仍停在 seq=2: %+v", be)
	}
	if got := h.c.Stats().Snapshot().Superseded; got != 1 {
		t.Fatalf("Superseded 应为 1: %d", got)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 1 {
		t.Fatalf("同键只应算一个脏键: %d", got)
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := sn.get("x"); got == nil || got.N != 2 {
		t.Fatalf("落库值应为 N=2: %+v", got)
	}
}

// TestRemove_StaleAdmittedAfterExpiry_RepairedToBuffered 对称形状：过期 Remove 的
// DEL 因键不存在被放行，缓冲里更新的 upsert 值必须被补回 Redis，
// 而不是留一个空洞等到逻辑过期才自愈。
func TestRemove_StaleAdmittedAfterExpiry_RepairedToBuffered(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	if err := h.c.Write(ctx, "x", &testVal{N: 1}); err != nil { // seq=1
		t.Fatal(err)
	}

	rmAtGate := make(chan struct{})
	releaseRm := make(chan struct{})
	var once sync.Once
	h.kv.seqGate = func(_ string, seq int64) {
		if seq != 2 {
			return
		}
		once.Do(func() { close(rmAtGate) })
		<-releaseRm
	}

	rmDone := make(chan error, 1)
	go func() { rmDone <- h.c.Remove(ctx, "x") }() // seq=2，卡在 DEL 门口
	waitGate(t, rmAtGate, "Remove 取到 seq=2 并抵达 Redis 门口")

	if err := h.c.Write(ctx, "x", &testVal{N: 3}); err != nil { // seq=3 抢先落地
		t.Fatalf("新 Write 失败: %v", err)
	}
	h.kv.hardDel(ks) // 制造交错：DEL 抵达时键已不在，CAS 放行这次过期删除

	close(releaseRm)
	if err := <-rmDone; err != nil {
		t.Fatalf("过期 Remove 不该报错: %v", err)
	}

	e := mustEnvelope(t, h, ks)
	if e.Seq != 3 || e.Value.N != 3 {
		t.Fatalf("过期删除造成的空洞应被补回 seq=3 的值: seq=%d N=%d", e.Seq, e.Value.N)
	}
	if be := bufferEntry(h, ks); be == nil || be.op != OpUpsert || be.seq != 3 {
		t.Fatalf("缓冲应保留 seq=3 的 upsert: %+v", be)
	}
}
