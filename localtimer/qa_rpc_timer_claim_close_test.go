package localtimer

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// ============================================================================
// 槽位 RPC 复核修复回归（T6-G）：
//   - P2-3：TimerPool.Get 认领与陈旧 tick 的 endTick 补偿分支之间的双重所有权
//     窗口——认领方在 parent.mu 下重置三个标志，endTick 锁内复核 pendingRelease；
//   - P3-2：CloseCtx 的 drainPendingAdds 纳入收尾预算，病态持锁不再无限阻塞。
// ============================================================================

// qaRpcTimer 测试专用宿主类型（独立 reflect.Type → 独立对象池，不污染其它用例）。
type qaRpcTimer struct{ Timer }

func (t *qaRpcTimer) Tick() {}

// TestQaRpcStaleEndTickNoDoublePut P2-3 定向复现：陈旧 tick 任务的 endTick 在
// 「快路径读到旧 pendingRelease=true」之后被池认领穿插，修复前锁内仲裁看到
// inPool=false + putClaimed=false 的组合会判 releaseNow，把新主人正持有的实例
// 二次 Put（双重所有权/使用后释放）。修复后 Get 的标志重置与 endTick 的锁内
// 复核同持 parent.mu：仲裁前必再读一次 pendingRelease（已被认领方清掉）→ 放弃归还。
// 红：inPool 被翻回 true 且 puts 多计一次；绿：inPool 保持 false、puts 零增量。
func TestQaRpcStaleEndTickNoDoublePut(t *testing.T) {
	inst := timerPool.Get(&qaRpcTimer{}).(*qaRpcTimer)
	parent := inst.GetParent()
	var self TimerInterface = inst
	parent.self.Store(&self) // releaseToPool 走 getSelf，需就位
	if !parent.fromPool.Load() {
		t.Fatal("✘ 前置破坏：池发放实例 fromPool 应为 true")
	}

	// 预置「上一代延迟归还已落地、实例在池等待认领」的残留标志组合：
	// inPool=true、released=true（Put 已落地）、pendingRelease=true（落地时残留的
	// 陈旧挂起标志——Get 认领后才清）、putClaimed=true（上一轮归还已抢占）。
	// 不真进 sync.Pool 存储，避免干扰池内其它条目。
	parent.inPool.Store(true)
	parent.released.Store(true)
	parent.pendingRelease.Store(true)
	parent.putClaimed.Store(true)
	parent.inTick.Store(0)

	putsBefore := timerPool.puts.Load()

	// 陈旧 tick 任务：beginTick 已 inTick+1、代次不匹配、快路径读标志后进入 endTick。
	// 测试持 parent.mu 把 endTick 挡在锁前——它此刻已完成 inTick-1 与
	// pendingRelease 快路径读取（旧值 true），正是竞态窗口的交错点。
	parent.mu.Lock()
	parent.inTick.Store(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		parent.endTick()
	}()
	for parent.inTick.Load() != 0 {
		runtime.Gosched() // 等 endTick 完成 inTick.Add(-1)
	}
	time.Sleep(100 * time.Millisecond) // 确保其快路径 pendingRelease 读取（=true）已发生并阻塞在 mu

	// 新主人认领（与 TimerPool.Get 修复后的临界区同构：CAS 后在同一把 mu 下重置三标志）
	if !parent.inPool.CompareAndSwap(true, false) {
		t.Fatal("✘ 前置破坏：实例应在池待认领")
	}
	parent.released.Store(false)
	parent.pendingRelease.Store(false)
	parent.putClaimed.Store(false)
	parent.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ endTick 未在 2s 内结束（死锁？）")
	}

	if parent.inPool.Load() {
		t.Fatalf("✘ 陈旧 endTick 把已被新主人认领的实例二次 Put（inPool 被翻回 true，puts +%d）",
			timerPool.puts.Load()-putsBefore)
	}
	if d := timerPool.puts.Load() - putsBefore; d != 0 {
		t.Fatalf("✘ 不应发生任何归还，puts 增量=%d（双重所有权）", d)
	}
	if parent.inTick.Load() != 0 {
		t.Fatalf("✘ 在飞计数应配对归零，实际 %d", parent.inTick.Load())
	}
	// 实例归还测试所有权语义：不再 Put（已预置脏状态），留给 GC。
}

// TestQaRpcClaimResetSerializedWithRequestRelease P2-3 契约面：认领后的实例
// 处于「干净持有」态，requestReleaseLocked 对新主人仍正常判 releaseNow
// （锁内复核没有把合法的归还路径一并堵死）。
func TestQaRpcClaimResetSerializedWithRequestRelease(t *testing.T) {
	inst := timerPool.Get(&qaRpcTimer{}).(*qaRpcTimer)
	parent := inst.GetParent()
	var self TimerInterface = inst
	parent.self.Store(&self)
	// 模拟陈旧残留被认领方清干净
	parent.mu.Lock()
	parent.released.Store(false)
	parent.pendingRelease.Store(false)
	parent.putClaimed.Store(false)
	parent.mu.Unlock()

	// 正常业务 Remove：inTick=0 → 应判 releaseNow（契约不变）
	outcome := parent.removeWithLock(true)
	if outcome != releaseNow {
		t.Fatalf("✘ 干净实例的 Remove 应判 releaseNow，实际 %v", outcome)
	}

	// 挂起路径：pendingRelease=true 时 endTick 仍照常落地归还
	inst2 := timerPool.Get(&qaRpcTimer{}).(*qaRpcTimer)
	p2 := inst2.GetParent()
	var self2 TimerInterface = inst2
	p2.self.Store(&self2)
	putsBefore := timerPool.puts.Load()
	p2.inTick.Store(1)
	p2.pendingRelease.Store(true)
	p2.endTick()
	if !p2.inPool.Load() {
		t.Fatal("✘ 挂起的归还请求应由 endTick 落地（inPool 应为 true）")
	}
	if d := timerPool.puts.Load() - putsBefore; d != 1 {
		t.Fatalf("✘ 应恰发生一次归还，puts 增量=%d", d)
	}
}

// TestQaRpcCloseCtxDrainWithinBudget P3-2：drainPendingAdds 要逐条取 parent.mu，
// 病态持锁（消费者卡在 handleTimerAdd 不放）时修复前的 CloseCtx 在预算 select
// 之外无限阻塞——恰复现它声称修复的挂起；修复后 drain 纳入预算，超时放弃等待。
// 红：CloseCtx 永不返回（测试超时崩溃）；绿：按预算 ~200ms 返回 timedOut=true。
func TestQaRpcCloseCtxDrainWithinBudget(t *testing.T) {
	m := &TimerManager{closeChan: make(chan struct{})}
	w := &TimerWheel{addTimerChan: make(chan timerTask, 4), mgr: m}
	w.isRunning.Store(true)
	m.wheels = []*TimerWheel{w}

	stuck := &qaRpcTimer{}
	stuck.mu.Lock() // 病态持锁方：drainPendingAdds 处理该 task 时必须取这把锁
	w.addTimerChan <- timerTask{timer: stuck, gen: stuck.gen.Load(), mgr: m}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	timedOut := m.CloseCtx(ctx)
	elapsed := time.Since(start)
	stuck.mu.Unlock()

	if !timedOut {
		t.Fatal("✘ 收尾超时应报告 timedOut=true")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("✘ CloseCtx 未在预算内放弃（耗时 %v）：drainPendingAdds 未纳入预算 select", elapsed)
	}
}
