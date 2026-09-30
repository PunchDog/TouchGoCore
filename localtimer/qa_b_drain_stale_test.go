package localtimer

// ============================================================================
// B-F2（槽位B）：drainPendingAdds 不得用陈旧 task 杀死已在别处复活的实例。
//
// 缺陷形态：实例经 Pause(gen++) 后被另一个管理器 AddTimer 复活，旧管理器收尾时
// 从 addTimerChan 捞出的是易主前的陈旧 task，旧实现裸写 parent.isActive.Store(false)，
// 把归属另一管理器的活跃实例静默杀死（新管理器侧只会走 unlinkStale 摘链或
// handleTimerAdd 丢弃，救不回活跃标记）。
// 修复后：Store(false) 前按包内 isValid 口径（活跃 + 代次一致，持 parent.mu）甄别，
// 非本代 task 只计数不动状态。
// ============================================================================

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// qaBGateTimer armed 之后 GetParent 阻塞在 gate 上，用于把 mgr1 的轮协程
// 卡死在 handleTimerAdd 的 isValid 里（该处会调用业务实现的 GetParent）。
type qaBGateTimer struct {
	Timer
	gate  chan struct{}
	armed atomic.Bool
}

func (g *qaBGateTimer) Tick() {}

func (g *qaBGateTimer) GetParent() *Timer {
	if g.armed.Load() {
		<-g.gate
	}
	return &g.Timer
}

// TestQaBDrainPendingAddsSkipsForeignGen mgr1 入队 task → Pause → mgr2 AddTimer
// 复活 → mgr1 CloseCtx：实例必须仍活跃且在 mgr2 正常触发。
func TestQaBDrainPendingAddsSkipsForeignGen(t *testing.T) {
	mgr1 := NewTimerManager()
	mgr2 := NewTimerManager()
	defer mgr2.Close()

	// 1) 把 mgr1 的毫秒轮协程卡在 handleTimerAdd 里：此后投入该轮通道的 task
	//    永远无人消化，CloseCtx 超时后由 drainPendingAdds 收尾——正是要观测的路径。
	gate := &qaBGateTimer{gate: make(chan struct{})}
	if err := gate.Init(1000, InfiniteCount, gate); err != nil {
		t.Fatalf("✘ gate.Init 失败: %v", err)
	}
	gate.armed.Store(true)
	msWheel := mgr1.wheels[TimerTypeMillisecond]
	msWheel.addTimerChan <- timerTask{timer: gate, gen: gate.gen.Load(), mgr: mgr1}
	qaBWaitFor(t, 2*time.Second, "前置条件：阻塞 task 未被 mgr1 轮协程取走", func() bool {
		return len(msWheel.addTimerChan) == 0
	})
	defer close(gate.gate) // 收尾放行，让 mgr1 的轮协程退出

	// 2) 受害定时器入队 mgr1：task 滞留在 addTimerChan 里
	victim, err := NewTimer[*qaBBeatTimer](50, InfiniteCount, func(p *qaBBeatTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	if err := mgr1.AddTimer(victim); err != nil {
		t.Fatalf("✘ mgr1.AddTimer 失败: %v", err)
	}
	if n := len(msWheel.addTimerChan); n != 1 {
		t.Fatalf("✘ 前置条件：受害定时器的调度项未滞留（len=%d），本用例无从观测", n)
	}

	// 3) Pause（gen++）后由 mgr2 复活（gen 再 ++）：实例归属已易主
	victim.Pause()
	if err := mgr2.AddTimer(victim); err != nil {
		t.Fatalf("✘ mgr2.AddTimer 失败: %v", err)
	}

	// 4) mgr1 带短预算收尾：轮协程被卡死，必然超时并走到 drainPendingAdds
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if !mgr1.CloseCtx(ctx) {
		t.Error("✘ mgr1 收尾未按预期超时（轮协程应被 GetParent 卡住）")
	}
	if s := mgr1.GetStats(); s.TimersDropped == 0 {
		t.Fatal("✘ 前置条件：drainPendingAdds 没有捞出任何调度项，本用例无从观测")
	}

	// 5) 实例必须仍活跃，且在 mgr2 正常触发
	if !victim.IsActive() {
		t.Fatal("✘ drainPendingAdds 用陈旧 task 杀死了已在 mgr2 复活的实例（无代次校验的裸 Store(false)）")
	}
	// 系统未 Run，没有消费协程：手动从全局调度通道消费受害定时器的到期派发
	channels := currentTimerChannels()
	deadline := time.Now().Add(3 * time.Second)
	ticked := false
	for time.Now().Before(deadline) && !ticked {
		for _, ch := range channels {
			select {
			case task := <-ch:
				if task.timer == TimerInterface(victim) && task.isValid() {
					task.mgr.executeTimer(task)
					ticked = true
				}
			default:
			}
		}
		if !ticked {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !ticked {
		t.Fatal("✘ 受害定时器在 mgr2 上未被派发（活跃标记或调度链被收尾路径破坏）")
	}
	if victim.n.Load() == 0 {
		t.Fatal("✘ 受害定时器的 Tick 一次都没执行")
	}
	victim.Pause()
	t.Logf("✔ 陈旧 task 被 drainPendingAdds 计数丢弃，实例在 mgr2 正常触发 %d 次", victim.n.Load())
}
