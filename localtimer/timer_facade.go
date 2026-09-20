package localtimer

import (
	"context"
	"time"

	"touchgocore/vars"
)

// ==================== 定时器系统全局门面 ====================

// DefaultCloseBudget 关闭单个定时器管理器的默认收尾预算。
//
// 收尾要在无锁状态下补跑每个到期定时器的业务回调，一个卡在下游锁上的 Tick
// 就能让进程停不下来。5 秒足够正常回调跑完，又不至于拖长停机。
const DefaultCloseBudget = 5 * time.Second

// closeAllManagers 逐个关闭注册表内的管理器，返回是否有管理器收尾超时
func closeAllManagers(parent context.Context) (timedOut bool) {
	if parent == nil {
		parent = context.Background()
	}
	for _, mgr := range snapshotTimerManagers() {
		vars.Info("关闭定时器管理器，当前定时器数量: %d", mgr.GetTimerCount())
		budget, cancel := context.WithTimeout(parent, DefaultCloseBudget)
		if mgr.CloseCtx(budget) {
			timedOut = true
		}
		cancel()
	}
	return timedOut
}

// Run 启动定时器系统。ctx 取消时 TimeTick 退出。
//
// Run 与 TimeStop 不建议并发调用。重复 Run 会先完整收尾上一轮生命周期
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
	// 待关清单在冻结注册表的窗口内取出（snapshotTimerManagers），放开锁后才逐个 Close：
	// Close 反过来要申请同一把锁做自摘，持锁等待退出会自锁。窗口外并发的
	// NewTimerManager 只能排在本函数之后登记，不会再出现「已注册却从未 Close 就被抹掉」。
	// 预算刻意不继承本次 ctx：重启往往正是因为旧 ctx 已取消。
	closeAllManagers(context.Background())
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

	// 关闭所有定时器管理器：预算取 ctx 与 DefaultCloseBudget 的较小者，
	// 避免调用方传了无截止的 Background 时被一个卡死的业务 Tick 永久挂住。
	if closeAllManagers(ctx) {
		vars.Warning("定时器管理器收尾超时，剩余定时器已被强制回收")
	}
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

// AddTimer 向默认管理器添加定时器（支持一次传入多个）
//
// 全部成功才返回 nil：中途失败时把本次已注册成功的项逐个撤下，
// 否则调用方拿到错误却发现前半批定时器已经在跑，既无法回滚也无人负责回收。
func AddTimer(timer ...TimerInterface) error {
	mgr := defaultTimerManager.Load()
	if mgr == nil {
		return ErrTimerSystemNotReady
	}

	for i, v := range timer {
		if v == nil || v.GetParent() == nil {
			rollbackTimers(timer[:i])
			return ErrTimerNilParent
		}
		if err := mgr.AddTimer(v); err != nil {
			rollbackTimers(timer[:i])
			return err
		}
	}

	return nil
}

// rollbackTimers 撤销本批已注册成功的定时器：只 Pause 不 Remove。
//
// Remove 会把实例所有权交给对象池，而调用方手里还攥着这些指针；
// Pause 停掉调度并把所有权留在调用方，稍后仍可显式 AddTimer 复活或 Remove 作废。
func rollbackTimers(added []TimerInterface) {
	for _, v := range added {
		if v == nil {
			continue
		}
		v.Pause()
	}
}

// channelSnapshotInterval 调度通道快照重取周期。TimeStop 会把全局调度通道置
// nil、下一次 Run 再重建，只在启动时读一次快照的消费协程会永远守着一个已被
// 丢弃的旧通道。
const channelSnapshotInterval = 10 * time.Millisecond

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

	ctxDone := rt.ctx.Done()
	recheck := time.NewTicker(channelSnapshotInterval)
	defer recheck.Stop()

	for {
		ch := currentTimerChannel()
		if ch == nil {
			// 尚无调度通道（未 Run 或已 Stop）：等生命周期切换，不自毁
			select {
			case <-rt.closech:
				rt.retire("本轮生命周期已结束")
				return
			case <-ctxDone:
				rt.retire("上下文已取消")
				return
			case <-recheck.C:
				continue
			}
		}

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
			//
			// 管理器已关闭说明这条是上一轮遗留（或在停机途中刚入队）的陈旧调度项：
			// 既不能执行也不能续期，否则跨 Run 把旧定时器复活。这道闸门放在消费端
			// 而非 isValid 里，因为收尾路径 cleanupWheel 正是靠关闭后补跑最后一拍
			// 把实例交回对象池。
			if task.mgr == nil || task.mgr.isClosed.Load() {
				continue
			}
			task.mgr.executeTimer(task)

		case <-rt.closech:
			rt.retire("本轮生命周期已结束")
			return
		case <-ctxDone:
			rt.retire("上下文已取消")
			return
		case <-recheck.C:
			// 定时重取通道快照：通道可能已被 TimeStop 释放或由 Run 重建
		}
	}
}

// retire 消费协程退出时把自身从全局 runtime 摘除。
//
// ctx 被取消却无人调用 TimeStop 是最常见的一种：全局仍指向本轮，
// IsSystemRunning 于是继续谎报「系统正常」，而已经没有任何协程在消费调度通道，
// 业务 AddTimer 全部堆在通道里。摘除后状态诚实，同时留一条告警便于定位。
func (rt *timerRuntime) retire(reason string) {
	if timerRT.CompareAndSwap(rt, nil) {
		vars.Warning("定时器滴答协程已退出(%s)，计时器系统不再消费调度项", reason)
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
//
// 必须同时满足「本轮生命周期协程还在」与「默认管理器没关」：ctx 被取消后
// timeTick 已经退出（并把自己从 timerRT 摘除），只看管理器会长期谎报正常，
// 业务照旧 AddTimer 却没有任何消费端，调度项默默堆到通道满。
func IsSystemRunning() bool {
	if timerRT.Load() == nil {
		return false
	}
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
