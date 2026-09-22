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
	m.drainPendingAdds()
	return timedOut
}

// drainPendingAdds 排空各时间轮残留的入队调度项并计入统计。
//
// 轮协程已退出，这些 task 永远不会被 handleTimerAdd 消化：对应定时器既不在链上，
// 也不会有人再执行它。留着不管就是「幻影活跃」——IsActive() 仍报 true，
// 实例却永远回不了对象池，而且监控上完全看不出这笔损失。
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
						// 撤销 addTimerLocked 置上的活跃标记；管理器已关闭，
						// 并发的 AddTimer 会在入口直接返回 ErrTimerManagerClosed，不会重写。
						parent.isActive.Store(false)
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
