package localtimer

import (
	"context"

	"touchgocore/vars"
)

// Close 优雅地关闭定时器管理器（不设收尾时限）
func (m *TimerManager) Close() {
	_ = m.CloseCtx(context.Background())
}

// CloseCtx 带预算地关闭定时器管理器，返回 ctx 是否为收尾超时而放弃。
//
// 收尾必须遍历时间轮并补跑最后一拍的业务回调（cleanupWheel）：一个卡在下游
// 锁上的 Tick 就能让 TimeStop 永久挂起，进程停不下来。ctx 到期后不再等待，
// 把剩余定时器强制归还对象池并告警——宁可丢一次回调，也不能丢整个进程退出。
//
// 同时把自身从管理器注册表摘除：Run 的「关闭上一轮全部管理器」与业务侧
// NewTimerManager 的注册一旦交错，新管理器会被 Clear 掉却从未 Close，
// 它名下 5 个时间轮协程 + 5 个 ticker 就永久泄漏。自摘让「离开注册表」与
// 「已经 Close」严格同义。
func (m *TimerManager) CloseCtx(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if !m.isClosed.CompareAndSwap(false, true) {
		return false // 已经关闭
	}

	// 先登记预算再发关闭信号：runWheel 的收尾分支要能立刻读到本次的 ctx
	m.closeCtx.Store(&ctx)
	unregisterTimerManager(m)

	// 发送关闭信号
	close(m.closeChan)

	timedOut := false
	done := make(chan struct{})
	go func() {
		m.wheelWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// 时间轮协程仍卡在业务回调里：标记已置位，后续 AddTimer 一律被拒，
		// 这里不能再等，否则整个进程退出流程被一个 Tick 拖死。
		timedOut = true
		vars.Error("等待时间轮退出超时，强制结束收尾: 剩余定时器=%d, %v",
			m.GetTimerCount(), ctx.Err())
	}

	for _, wheel := range m.wheels {
		wheel.isRunning.Store(false)
	}
	// drainPendingAdds 同样纳入收尾预算：它要逐条取 task.timer 的 parent.mu，
	// 病态场景（某个消费者卡在 handleTimerAdd 持锁不放）下无预算等待会无限阻塞，
	// 恰好复现本方法声称修复的「一个卡住的收尾拖死进程退出」。放到协程里跑、
	// 只等预算：超时后放弃等待（协程泄漏有界——它只可能卡在这把 mu 上），
	// 与 wheelWG 等待同一取舍口径。
	drainDone := make(chan struct{})
	go func() {
		m.drainPendingAdds()
		close(drainDone)
	}()
	select {
	case <-drainDone:
	case <-ctx.Done():
		if !timedOut {
			timedOut = true
			vars.Error("排空入队通道残留未在收尾预算内完成，放弃等待: %v", ctx.Err())
		}
	}
	return timedOut
}

// drainPendingAdds 排空各时间轮残留的入队调度项并计入统计。
//
// 轮协程已退出，这些 task 永远不会被 handleTimerAdd 消化：对应定时器既不在链上，
// 也不会有人再执行它。留着不管就是「幻影活跃」——IsActive() 仍报 true，
// 实例却永远回不了对象池，而且监控上完全看不出这笔损失。
//
// 撤活跃标记前必须按代次甄别（见循环内注释）：捞出来的 task 可能只是实例易主前
// 的陈旧残影，它对应的实例此刻可能正在别的管理器上活跃。
func (m *TimerManager) drainPendingAdds() {
	var total int64
	for _, wheel := range m.wheels {
	drain:
		for {
			select {
			case task := <-wheel.addTimerChan:
				total++
				// task.timer 理论上恒非空（入队方都带实例），零值只可能来自误用，
				// 这里只断计数不追状态，避免收尾路径反过来 panic。
				if task.timer != nil {
					if parent := task.timer.GetParent(); parent != nil {
						// 只有 task 仍属该实例的当前代次，才撤销 addTimerLocked 置上的
						// 活跃标记（口径同 timerTask.isValid：活跃 + 代次一致）。实例可能
						// 已经 Pause（gen++）后被另一个管理器 AddTimer 复活——池易主/多
						// 管理器形态下这条 task 只是陈旧残影，裸 Store(false) 会把在别处
						// 活跃的实例静默杀死。非本代 task 只计数（total），不动状态。
						// 校验与撤标记整体持 parent.mu：与 AddTimer 的「清理→推进代次→
						// 恢复活跃→入队」临界区互斥，杜绝检查与 Store 之间被复活方穿插。
						// 本管理器侧并发的 AddTimer 会在入口直接返回 ErrTimerManagerClosed，
						// 不会重写这个标记。
						parent.mu.Lock()
						if parent.IsActive() && parent.gen.Load() == task.gen {
							parent.isActive.Store(false)
						}
						parent.mu.Unlock()
					}
				}
			default:
				break drain
			}
		}
	}
	if total == 0 {
		return
	}
	m.stats.timersDropped.Add(total)
	m.stats.timersRemoved.Add(total)
	vars.Warning("关闭时丢弃入队通道残留调度项: 数量=%d, 累计丢弃=%d",
		total, m.stats.timersDropped.Load())
}
