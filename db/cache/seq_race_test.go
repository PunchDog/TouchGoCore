package cache

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ==================== H1：写线并发乱序（seq 保序防线） ====================
//
// 事故形状：Write 的三步「seq 自增 → 同步写 Redis → 入写缓冲」并非原子。
// 同键并发时先取到较小 seq 的一方可能后落地：Redis 被旧值倒灌，写缓冲也被
// 旧条目无条件覆盖，随后 flush 把旧值写进库——而 hasNewer/dropIfOlder/
// JClearIfSeq 全都以「缓冲与账本里的 seq 就是最新值」为前提，于是三道防线
// 一起失效，最终库里留下一个比 Redis 更旧、且没有任何路径会纠正的行。
//
// 修复：Redis 那一步按 seq 条件化（SeqKV/JournalSeq 的 Lua CAS），缓冲那一步
// 取 max(seq)。本文件用「注入交错 + 不变量断言」验证——本机无 C 工具链，
// go test -race 不可用，靠 fakeKV.seqGate 确定性地把「小 seq 后落地」摆出来。

// mustEnvelope 断言 Redis 里有该键且能解出 envelope（写线保序的最终权威）
func mustEnvelope(t *testing.T, h *harness, ks string) *envelope[testVal] {
	t.Helper()
	raw, ok := h.kv.rawOf(ks)
	if !ok {
		t.Fatalf("Redis 里没有键 %s", ks)
	}
	e, err := decode[testVal](h.c.codec, raw)
	if err != nil {
		t.Fatalf("envelope 解码失败 %q: %v", raw, err)
	}
	return e
}

// waitGate 等注入钩子触发，超时即失败——钩子没挂上时宁可响亮报错，
// 也不让测试静默卡死到 go test 全局超时（那样看不出是哪一步没走到）。
func waitGate(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待「%s」超时：注入钩子未按预期触发", what)
	}
}

// bufferEntry 读写缓冲里该键的条目快照（不存在返回 nil）
func bufferEntry(h *harness, ks string) *opEntry[string, testVal] {
	return h.c.buf.get(ks)
}

// TestBufferPut_KeepsMaxSeq 缓冲那一道防线：不存在则插入，已存在时仅当新 seq
// 更大才覆盖；旧 seq 到达一律丢弃，且不得虚增 Dirty。
func TestBufferPut_KeepsMaxSeq(t *testing.T) {
	b := newBuffer[string, testVal](4)
	ks := "tg:t:x"
	v5 := &testVal{N: 5}
	v3 := &testVal{N: 3}
	v7 := &testVal{N: 7}

	added, stored := b.put(ks, &opEntry[string, testVal]{ks: ks, key: "x", val: v5, op: OpUpsert, seq: 5})
	if !added || !stored {
		t.Fatalf("空缓冲首次 put 应 added+stored: added=%v stored=%v", added, stored)
	}

	// 旧 seq 后到：必须被丢弃（旧实现会无条件覆盖，于是缓冲里滞留 N=3）
	added, stored = b.put(ks, &opEntry[string, testVal]{ks: ks, key: "x", val: v3, op: OpUpsert, seq: 3})
	if added || stored {
		t.Fatalf("旧 seq 不得覆盖也不得算新增脏键: added=%v stored=%v", added, stored)
	}
	if e := b.get(ks); e == nil || e.seq != 5 || e.val.N != 5 {
		t.Fatalf("缓冲应保留 seq=5 的值: %+v", e)
	}

	// 同 seq 重写也拒：seq 由原子自增分配，不同写操作永不重号，
	// 同 seq 只可能是同一个条目重复投递，覆盖它没有任何意义。
	added, stored = b.put(ks, &opEntry[string, testVal]{ks: ks, key: "x", val: v3, op: OpUpsert, seq: 5})
	if added || stored {
		t.Fatalf("同 seq 重复 put 应被拒: added=%v stored=%v", added, stored)
	}

	// 新 seq 覆盖：这是唯一被允许的覆盖方向
	added, stored = b.put(ks, &opEntry[string, testVal]{ks: ks, key: "x", val: v7, op: OpUpsert, seq: 7})
	if added || !stored {
		t.Fatalf("更大 seq 应覆盖但不算新增脏键: added=%v stored=%v", added, stored)
	}
	if e := b.get(ks); e == nil || e.seq != 7 || e.val.N != 7 {
		t.Fatalf("缓冲应更新为 seq=7 的值: %+v", e)
	}

	// 不同键互不干扰
	if added, stored := b.put("tg:t:y", &opEntry[string, testVal]{ks: "tg:t:y", key: "y", val: v3, op: OpUpsert, seq: 1}); !added || !stored {
		t.Fatalf("另一个键的首次 put 应 added+stored: added=%v stored=%v", added, stored)
	}
}

// TestWrite_ConcurrentSameKey_LargerSeqWins 「小 seq 后落地」的确定性交错：
// A 拿到 seq=1 后卡在 Redis 门口，B 拿到 seq=2 完整落地（Redis + 缓冲 + 账本），
// 再放 A 进来。断言 A 在两道防线上都被挡下，三处存储全部停在 seq=2 的值。
func TestWrite_ConcurrentSameKey_LargerSeqWins(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	aAtGate := make(chan struct{})
	releaseA := make(chan struct{})
	var once sync.Once
	h.kv.seqGate = func(_ string, seq int64) {
		if seq != 1 {
			return // B(seq=2) 直通
		}
		once.Do(func() { close(aAtGate) })
		<-releaseA // A 挂在 Redis 门口，直到 B 已经完整落地
	}

	aDone := make(chan error, 1)
	go func() { aDone <- h.c.Write(ctx, "x", &testVal{N: 1}) }()
	waitGate(t, aAtGate, "A 取到 seq=1 并抵达 Redis 门口")

	if err := h.c.Write(ctx, "x", &testVal{N: 2}); err != nil { // B 拿到 seq=2
		t.Fatalf("B 写入失败: %v", err)
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("A 的过期写不该报错（按 seq 线性序它本就应该输）: %v", err)
	}

	// 防线一：Redis 里必须是 seq=2 的值，A 的旧值没能倒灌
	if e := mustEnvelope(t, h, ks); e.Seq != 2 || e.Value.N != 2 {
		t.Fatalf("Redis 应停在最大 seq: seq=%d N=%d", e.Seq, e.Value.N)
	}
	// 防线二：写缓冲同样停在 seq=2——旧实现会让 A 无条件覆盖成 N=1 并随后落库
	be := bufferEntry(h, ks)
	if be == nil || be.seq != 2 || be.val.N != 2 {
		t.Fatalf("写缓冲应停在最大 seq: %+v", be)
	}
	// 账本 score 必须是 2，否则落库侧的 JClearIfSeq(score==本条 seq) 永远对不上
	if jm := h.kv.journalOf(h.c.jz); jm["u|x"] != 2 {
		t.Fatalf("账本 score 应为最大 seq: %v", jm)
	}
	// 可观测：过期写被计数，不是静默吞掉
	if got := h.c.Stats().Snapshot().Superseded; got != 1 {
		t.Fatalf("Superseded 应为 1: %d", got)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 1 {
		t.Fatalf("同键只应算一个脏键: %d", got)
	}
	// 最终落地值：flush 后库里是 N=2，且账本被正确销掉
	if err := h.c.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := sn.get("x"); got == nil || got.N != 2 {
		t.Fatalf("落库值应为最大 seq 对应的值: %+v", got)
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 0 {
		t.Fatalf("落库成功必须销账（score 对齐才销得掉）: %v", jm)
	}
}

// TestWriteSync_StaleSkipsDatabaseToo WriteSync 与普通 Write 并发：
// WriteSync 拿到较小 seq 且 CAS 未生效时，必须连 DB Save 一起跳过。
// 否则新值可能已经 flush 出缓冲，库里会永久留下一个比 Redis 更旧的行。
func TestWriteSync_StaleSkipsDatabaseToo(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	syncAtGate := make(chan struct{})
	releaseSync := make(chan struct{})
	var once sync.Once
	h.kv.seqGate = func(_ string, seq int64) {
		if seq != 1 {
			return
		}
		once.Do(func() { close(syncAtGate) })
		<-releaseSync
	}

	syncDone := make(chan error, 1)
	go func() { syncDone <- h.c.WriteSync(ctx, "x", &testVal{N: 1}) }()
	waitGate(t, syncAtGate, "WriteSync 取到 seq=1 并抵达 Redis 门口")

	if err := h.c.Write(ctx, "x", &testVal{N: 2}); err != nil { // 普通 Write 拿到 seq=2
		t.Fatalf("Write 失败: %v", err)
	}
	close(releaseSync)
	if err := <-syncDone; err != nil {
		t.Fatalf("过期 WriteSync 不该报错: %v", err)
	}

	if saves, _ := sn.counts(); saves != 0 {
		t.Fatalf("CAS 未生效时绝不能落库（库里会永久留下比 Redis 更旧的行）: saves=%d", saves)
	}
	if e := mustEnvelope(t, h, ks); e.Seq != 2 || e.Value.N != 2 {
		t.Fatalf("Redis 应停在 seq=2: seq=%d N=%d", e.Seq, e.Value.N)
	}
	if be := bufferEntry(h, ks); be == nil || be.seq != 2 || be.val.N != 2 {
		t.Fatalf("写缓冲应停在 seq=2: %+v", be)
	}
	if got := h.c.Stats().Snapshot().Superseded; got != 1 {
		t.Fatalf("Superseded 应为 1: %d", got)
	}
	// 后续 flush 落的仍是新值
	if err := h.c.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := sn.get("x"); got == nil || got.N != 2 {
		t.Fatalf("落库值应为 N=2: %+v", got)
	}
}

// TestWriteSync_LargerSeqStillSaves 反证：WriteSync 拿到更大 seq 时照常落库，
// CAS 没有把正常路径一起挡掉（否则上面那条断言只是空转）。
func TestWriteSync_LargerSeqStillSaves(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()

	if err := h.c.Write(ctx, "x", &testVal{N: 1}); err != nil { // seq=1 进缓冲
		t.Fatal(err)
	}
	if err := h.c.WriteSync(ctx, "x", &testVal{N: 9}); err != nil { // seq=2 强一致写
		t.Fatal(err)
	}
	if got := sn.get("x"); got == nil || got.N != 9 {
		t.Fatalf("更大 seq 的 WriteSync 必须落库: %+v", got)
	}
	if e := mustEnvelope(t, h, h.c.KeyOf("x")); e.Seq != 2 || e.Value.N != 9 {
		t.Fatalf("Redis 应为 seq=2/N=9: seq=%d N=%d", e.Seq, e.Value.N)
	}
	if h.c.Pending("x") {
		t.Fatal("WriteSync 成功后必须踢掉更旧的缓冲条目")
	}
	if got := h.c.Stats().Snapshot().Superseded; got != 0 {
		t.Fatalf("正常路径不该计 Superseded: %d", got)
	}
}

// TestRemove_StaleDeleteDoesNotClobberNewerWrite Remove 的 DEL 同样按 seq 条件化：
// 并发的更新 Write 已把值写进 Redis 时，过期删除不得把它抹掉
// （否则读线会回源到一个即将被新值覆盖的旧库行）。
func TestRemove_StaleDeleteDoesNotClobberNewerWrite(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	rmAtGate := make(chan struct{})
	releaseRm := make(chan struct{})
	var once sync.Once
	h.kv.seqGate = func(_ string, seq int64) {
		if seq != 2 {
			return // seq=1 的初始 Write 与 seq=3 的新 Write 直通
		}
		once.Do(func() { close(rmAtGate) })
		<-releaseRm
	}

	if err := h.c.Write(ctx, "x", &testVal{N: 1}); err != nil { // seq=1
		t.Fatal(err)
	}
	rmDone := make(chan error, 1)
	go func() { rmDone <- h.c.Remove(ctx, "x") }() // seq=2，卡在 DEL 门口
	waitGate(t, rmAtGate, "Remove 取到 seq=2 并抵达 Redis 门口")

	if err := h.c.Write(ctx, "x", &testVal{N: 3}); err != nil { // seq=3 抢先落地
		t.Fatalf("新 Write 失败: %v", err)
	}
	close(releaseRm)
	if err := <-rmDone; err != nil {
		t.Fatalf("过期 Remove 不该报错: %v", err)
	}

	e := mustEnvelope(t, h, ks)
	if e.Seq != 3 || e.Value.N != 3 {
		t.Fatalf("过期删除不得抹掉更新的值: seq=%d N=%d", e.Seq, e.Value.N)
	}
	if be := bufferEntry(h, ks); be == nil || be.op != OpUpsert || be.seq != 3 {
		t.Fatalf("缓冲应保留 seq=3 的 upsert，而不是被 tombstone 覆盖: %+v", be)
	}
	if jm := h.kv.journalOf(h.c.jz); jm["u|x"] != 3 {
		t.Fatalf("账本应为 seq=3 的 upsert 账: %v", jm)
	}
	if _, ok := h.kv.journalOf(h.c.jz)["d|x"]; ok {
		t.Fatalf("过期删除不得记 tombstone 账: %v", h.kv.journalOf(h.c.jz))
	}
	if got := h.c.Stats().Snapshot().Superseded; got != 1 {
		t.Fatalf("Superseded 应为 1: %d", got)
	}
}

// TestWrite_RenewSameSeqStillAllowed 守卫用「严格大于」而非「大于等于」的反证：
// renewIfAged 用同一个 seq 重写 Redis 刷新逻辑新鲜期，必须放行，
// 否则超龄条目的异步续期会变成永远空转。
func TestWrite_RenewSameSeqStillAllowed(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	if err := h.c.Write(ctx, "x", &testVal{N: 4}); err != nil { // seq=1
		t.Fatal(err)
	}
	e := bufferEntry(h, ks)
	if e == nil {
		t.Fatal("前置：缓冲里应有条目")
	}
	// 同 seq 重写（renewIfAged 的形状）：CAS 必须放行
	ok, err := h.c.setEnvelopeSeq(ctx, ks, e.val, time.Second, e.seq, false)
	if err != nil {
		t.Fatalf("同 seq 重写报错: %v", err)
	}
	if !ok {
		t.Fatal("同 seq 重写必须放行，否则 renewIfAged 永远空转")
	}
	// 更小 seq 仍要被拒
	if ok, err := h.c.setEnvelopeSeq(ctx, ks, &testVal{N: 99}, time.Second, e.seq-1, false); err != nil || ok {
		t.Fatalf("更小 seq 必须被 CAS 拒掉: ok=%v err=%v", ok, err)
	}
	if got := mustEnvelope(t, h, ks); got.Value.N != 4 {
		t.Fatalf("Redis 值不该被旧 seq 改写: %+v", got.Value)
	}
}

// TestWrite_ConcurrentSameKey_AllStoresAgree 无注入交错的高并发压力：
// 32 个协程同键写，最终 Redis / 写缓冲 / 库三处必须停在同一个 (seq, 值) 上，
// 且该 seq 就是分配出去的最大值——任何一处滞留旧值都算失败。
func TestWrite_ConcurrentSameKey_AllStoresAgree(t *testing.T) {
	sn := newRecSaver()
	h := newHarness(t, WithSaver[string, testVal](sn))
	ctx := context.Background()
	ks := h.c.KeyOf("x")

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := h.c.Write(ctx, "x", &testVal{N: i}); err != nil {
				t.Errorf("Write(%d): %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	// 最大 seq 的写永远不会被 CAS 拒（没有更大的 seq 存在），所以 Redis 必停在 n
	e := mustEnvelope(t, h, ks)
	if e.Seq != n {
		t.Fatalf("Redis 应停在最大 seq=%d，实际 %d", n, e.Seq)
	}
	be := bufferEntry(h, ks)
	if be == nil || be.seq != e.Seq || be.val.N != e.Value.N {
		t.Fatalf("写缓冲必须与 Redis 同 (seq,值): buf=%+v redis=(%d,%d)", be, e.Seq, e.Value.N)
	}
	if got := h.c.Stats().Snapshot().Dirty; got != 1 {
		t.Fatalf("同键只应有一个脏键: %d", got)
	}
	if err := h.c.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := sn.get("x")
	if got == nil || got.N != e.Value.N {
		t.Fatalf("库里必须落地与 Redis 相同的值: db=%+v redis=%+v", got, e.Value)
	}
	if jm := h.kv.journalOf(h.c.jz); len(jm) != 0 {
		t.Fatalf("落库成功必须销账: %v", jm)
	}
}
