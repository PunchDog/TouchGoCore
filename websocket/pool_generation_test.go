package websocket

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// tickLoop 的 pool 代际隔离回归。
//
// 事故形状：workerPool 是包级原子指针，每次 Run 重建；tickLoop 原先直接回读它。
// Run→Stop→Run 换代窗口里，旧代 Tick 退出前仍可能消费旧 msgQueue 的存量消息，
// 此时包级指针已指向新一代的池——旧连接的消息被投进新池，与新代 Tick 混投。
// 修复：runState 增加 pool 字段，initWorkerPool 同时写包级指针与本代快照，
// tickLoop 只读 state.pool。本机无 -race，用「旧代消息只进旧池计数」直接验证。
// ============================================================================

func poolTotalMessages(p *workerPoolState) int64 {
	var n int64
	for _, s := range p.stats {
		n += s.Messages.Load()
	}
	return n
}

func stopPoolAndWait(p *workerPoolState) {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.wg.Wait()
}

func TestTickLoop_DispatchesOnlyToOwnGenerationPool(t *testing.T) {
	// 隔离包级 workerPool，测后还原
	prevPool := workerPool.Swap(nil)
	t.Cleanup(func() {
		if p := workerPool.Swap(prevPool); p != nil {
			stopPoolAndWait(p)
		}
	})

	state := newRunState(context.Background(), 8)
	go tickLoop(state)
	t.Cleanup(func() {
		close(state.closeCh)
		select {
		case <-state.tickDone:
		case <-time.After(2 * time.Second):
			t.Error("tickLoop 未在预算内退出")
		}
	})

	// 第一代：poolA 同时挂在包级指针与 state.pool
	initWorkerPool(2, false, state)
	poolA := workerPool.Load()
	if poolA == nil {
		t.Fatal("poolA 未装配")
	}
	if state.pool.Load() != poolA {
		t.Fatal("state.pool 未绑定本代池")
	}

	// 模拟换代：新一轮 Run 重建包级池，但旧代 state.pool 必须仍指 poolA
	initWorkerPool(2, false, nil)
	poolB := workerPool.Load()
	if poolB == nil || poolB == poolA {
		t.Fatalf("新代池未生效: %p vs %p", poolB, poolA)
	}
	if state.pool.Load() != poolA {
		t.Fatal("旧代 state.pool 漂到了新一代（tickLoop 会跨代投递）")
	}

	// 投进旧代 msgQueue：Tick 只能消费进 poolA（uid 无客户端，processMessage
	// 报错不影响 safeProcess 计数，计数即「进了哪个池」的证据）
	state.msgQueue <- &msgQueueType{uid: 889900, data: []byte("probe")}
	if !waitForMsg(func() bool { return poolTotalMessages(poolA) >= 1 }, 2*time.Second) {
		t.Fatal("旧代消息未被旧代池消费")
	}
	// 给误投新池一点传播时间再反证
	time.Sleep(100 * time.Millisecond)
	if n := poolTotalMessages(poolB); n != 0 {
		t.Fatalf("旧代 Tick 把消息投进了新一代的池: poolB 计数=%d", n)
	}
	if seq := poolB.dispatchSeq.Load(); seq != 0 {
		t.Fatalf("新池不应有任何派发: dispatchSeq=%d", seq)
	}

	// 收掉 poolA（Tick 的 shutdownWebsocket 只会停包级指针指向的 poolB）
	stopPoolAndWait(poolA)
	t.Log("✔ 旧代 Tick 只向本代池投递，换代隔离成立")
}

func waitForMsg(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
