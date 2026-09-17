package localtimer

import (
	"context"

	"touchgocore/vars"
)

// ==================== 定时器系统全局门面 ====================

// Run 启动定时器系统。ctx 取消时 TimeTick 退出。
//
// Run 与 TimeStop 不可并发调用。重复 Run 会先完整收尾上一轮生命周期
// （关闭旧 runtime、等旧 TimeTick 退出、Close 全部旧管理器），
// 否则旧管理器的时间轮协程与 ticker 会永久泄漏。
func Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	vars.Info("启动计时器系统")

	rt := &timerRuntime{closech: make(chan struct{}), ctx: ctx}

	// 先接管上一轮生命周期。等待可能阻塞在业务 Tick 里的旧 TimeTick 退出，
	// 否则新旧两个消费协程会同时抢同一个调度通道。
	if old := timerRT.Swap(rt); old != nil {
		vars.Warning("定时器系统重复启动，先收尾上一轮生命周期")
		old.shutdown()
		old.tickWG.Wait()
	}

	// 关闭上一轮全部管理器后重建。只 Clear 不 Close 会泄漏 5 个时间轮协程 + 5 个 ticker。
	timerManagerMap.Range(func(mgr *TimerManager, _ bool) bool {
		mgr.Close()
		return true
	})
	timerManagerMap.Clear()
	// NewTimerManager 内部会 ensureTimerChannel，必须先于 timeTick 协程启动
	defaultTimerManager.Store(NewTimerManager())

	rt.tickWG.Add(1)
	go func() {
		defer rt.tickWG.Done()
		// rt 以参数传入而非协程内读全局，消除「旧协程读到新 runtime」的窗口
		timeTick(rt)
	}()
	vars.Info("计时器系统启动完成")
}

// TimeStop 停止定时器系统。ctx 可用于限制等待时间。
//
// 幂等且并发安全：timerRT.Swap(nil) 保证只有一方拿到 runtime，
// 重复或并发调用都不会二次 close 通道（那会触发 fatal 的 close of closed channel）。
func TimeStop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	vars.Info("正在停止计时器系统...")

	rt := timerRT.Swap(nil)
	if rt != nil {
		rt.shutdown()
	}

	// 关闭所有定时器管理器
	timerManagerMap.Range(func(mgr *TimerManager, value bool) bool {
		vars.Info("关闭定时器管理器，当前定时器数量: %d", mgr.GetTimerCount())
		mgr.Close()
		return true
	})
	timerManagerMap.Clear()
	defaultTimerManager.Store(nil)

	if rt != nil {
		done := make(chan struct{})
		go func() {
			rt.tickWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			vars.Error("等待定时器协程退出超时: %v", ctx.Err())
		}
	}

	// 释放调度通道（不 close，避免仍在运行的协程 send on closed channel）
	resetTimerChannel()
	vars.Info("计时器系统已停止")
}

// AddTimer 向默认管理器添加定时器
func AddTimer(timer ...TimerInterface) error {
	mgr := defaultTimerManager.Load()
	if mgr == nil {
		return ErrTimerSystemNotReady
	}

	var err error
	for _, v := range timer {
		if v == nil || v.GetParent() == nil {
			return ErrTimerNilParent
		}
		err = mgr.AddTimer(v)
		if err != nil {
			return err
		}
	}

	return err
}

// timeTick 消费调度通道并执行到期定时器。
//
// rt 由调用方传入，不在协程内读全局：否则重复 Run 之后，
// 上一轮的 TimeTick 会读到新一轮的 closech，永远等不到自己的退出信号。
func timeTick(rt *timerRuntime) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("定时器滴答处理发生panic错误: %v", err)
		}
	}()

	ch := currentTimerChannel()
	if ch == nil {
		return
	}

	ctxDone := rt.ctx.Done()

	for {
		select {
		case task, ok := <-ch:
			if !ok {
				// 通道已关闭
				return
			}
			// 按归属管理器执行：timerChannel 是全局共享的，
			// 一律交给默认管理器会把非默认管理器的定时器改嫁过去。
			// 业务 Tick 的 panic 由 executeTimer 内部逐次 recover，
			// 不会杀死本消费协程导致所有定时器失效。
			if task.mgr != nil {
				go task.mgr.executeTimer(task)
			}

		case <-rt.closech:
			return
		case <-ctxDone:
			return
		}
	}
}

// TimeTick 处理定时器滴答（导出入口，绑定当前生命周期）
func TimeTick() {
	if rt := timerRT.Load(); rt != nil {
		timeTick(rt)
	}
}

// GetDefaultManager 返回默认定时器管理器
func GetDefaultManager() *TimerManager {
	return defaultTimerManager.Load()
}

// IsSystemRunning 检查定时器系统是否正在运行
func IsSystemRunning() bool {
	mgr := defaultTimerManager.Load()
	return mgr != nil && !mgr.isClosed.Load()
}

// GetSystemStats 返回定时器系统统计信息
func GetSystemStats() (totalTimers int64, stats TimerStats) {
	if mgr := defaultTimerManager.Load(); mgr != nil {
		totalTimers = mgr.GetTimerCount()
		stats = mgr.GetStats()
	}
	return
}
