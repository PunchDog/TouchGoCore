package localtimer

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/list"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
)

// timerTask 是调度通道中传递的定时器项。
//
// gen 记录入队瞬间的定时器代次；出队时若代次已变，说明该定时器已被移除或被
// 对象池复用给另一个业务，这条在途的旧调度项必须丢弃，否则会对同一个实例
// 重复调度、甚至操作已被别人复用的对象（即「幽灵定时器 / 串号」问题）。
//
// mgr 记录归属管理器。timerChannel 是全局共享的，若不带上归属，消费端只能
// 一律交给默认管理器执行并续期，非默认管理器的定时器首次触发后就被改嫁了。
type timerTask struct {
	timer TimerInterface
	gen   uint64
	mgr   *TimerManager
}

// isValid 判断该调度项是否仍然有效
//
// 刻意不检查 mgr.isClosed：收尾路径（cleanupWheel）正是靠关闭管理器时补跑最后一拍
// 来把定时器交回对象池，一旦在这里判废，在途实例既不被执行也无人回收，永久滞留。
// 「陈旧调度项跨 Run 复活」由全局通道消费端 timeTick 拦住（见该函数）。
func (t timerTask) isValid() bool {
	if t.timer == nil || t.mgr == nil {
		return false
	}
	parent := t.timer.GetParent()
	if parent == nil {
		return false
	}
	return parent.IsActive() && parent.gen.Load() == t.gen
}

// TimerWheel 表示时间轮结构
type TimerWheel struct {
	wheelConfig  int64          // 时间轮精度（毫秒）
	tickWheel    *list.List     // 定时器链表
	wheelLock    sync.RWMutex   // 时间轮锁（读写优化）
	addTimerChan chan timerTask // 定时器添加通道
	isRunning    atomic.Bool    // 是否运行
	timerCount   atomic.Int64   // 定时器计数（用于监控）

	// mgr 反向归属：摘链发生在 Timer 一侧（RemoveFromManager），那里只有 wheel 指针，
	// 没有它便无法把「移除」计入所属管理器的统计。
	mgr *TimerManager

	// lastBackpressureWarn 上次背压告警时间（毫秒）。按轮限频：一条全局限频线会让
	// 繁忙的毫秒轮把安静的小时轮的告警一并吞掉。
	lastBackpressureWarn atomic.Int64
}

// TimerManager 管理所有时间轮
type TimerManager struct {
	closeChan chan struct{}      // 关闭信号
	wheels    []*TimerWheel      // 时间轮数组
	isClosed  atomic.Bool        // 是否关闭
	stats     timerStatsCounters // 内部原子计数
	wheelWG   sync.WaitGroup

	// closeCtx 收尾预算：由 CloseCtx 在发出关闭信号前写入，供 cleanupWheel 逐节点
	// 判定「还能不能继续等业务回调」。用指针快照而非字段直改，避免与读取方竞态。
	closeCtx atomic.Pointer[context.Context]
}

// TimerStats 保存性能统计信息（快照值，非实时引用）
type TimerStats struct {
	TimersAdded     int64 // 已添加定时器数量
	TimersRemoved   int64 // 脱离调度的次数：暂停/移除/耗尽/关闭清场/背压丢弃（含复活前的摘链，不含派发与迁移）
	TimersExecuted  int64 // 已执行定时器数量
	TimersDropped   int64 // 因背压被丢弃的调度次数
	WheelMigrations int64 // 时间轮迁移次数

	TimersRescheduleFailed int64 // 续期重试后仍失败、定时器已被强制归还对象池的次数（>0 说明需要告警）
}

// timerStatsCounters 管理器内部的原子计数器
type timerStatsCounters struct {
	timersAdded     atomic.Int64
	timersRemoved   atomic.Int64
	timersExecuted  atomic.Int64
	timersDropped   atomic.Int64
	wheelMigrations atomic.Int64

	timersRescheduleFailed atomic.Int64
}

// wheelShouldWarn 限频判定：每个时间轮每秒最多一条告警。
//
// 各轮独立计数，否则毫秒轮常年背压会把小时轮的告警一并吞掉，
// 而后者恰恰是「某个低频业务定时器正在丢调度」的唯一线索。
func wheelShouldWarn(wheel *TimerWheel) bool {
	now := util.CurrentMS()
	last := wheel.lastBackpressureWarn.Load()
	if now-last < 1000 {
		return false
	}
	return wheel.lastBackpressureWarn.CompareAndSwap(last, now)
}

// notifyBackpressure 背压丢弃告警（确有调度项被丢下）。
//
// 必须在释放 wheelLock 与 parent.mu 之后调用——vars 的异步日志在缓冲写满时会
// 阻塞最长 5 秒（见 vars/async_channel.go 的阻塞模式分支），持锁调用会把整个
// 时间轮或这把定时器私有锁拖死。
func (m *TimerManager) notifyBackpressure(wheel *TimerWheel, wheelType TimerType, dropped int64) {
	if wheel == nil || !wheelShouldWarn(wheel) {
		return
	}
	vars.Warning("定时器背压: 轮=%s, 本次丢弃=%d, 累计丢弃=%d",
		wheelType.String(), dropped, m.stats.timersDropped.Load())
}

// notifyHighWatermark 入队通道水位告警：本次入队成功，只是队列已接近上限。
// 与丢弃告警分开措辞，否则「本次丢弃=0」的背压日志会让人误判为统计出错。
func (m *TimerManager) notifyHighWatermark(wheel *TimerWheel, wheelType TimerType, used, capacity int64) {
	if wheel == nil || !wheelShouldWarn(wheel) {
		return
	}
	vars.Warning("定时器入队通道水位过高: 轮=%s, 已用=%d/%d, 累计丢弃=%d",
		wheelType.String(), used, capacity, m.stats.timersDropped.Load())
}

// addTimerWarning 一次待发的背压告警。
//
// addTimerLocked 全程持有 parent.mu，而 vars 的异步日志在缓冲写满时最长阻塞 5 秒：
// 持锁写日志等于把这把定时器的 AddTimer/Remove/handleTimerAdd 全部排在这条日志后面。
// 因此临界区里只登记告警，真正的输出由解锁后的调用方发出。
type addTimerWarning struct {
	wheel     *TimerWheel
	wheelType TimerType
	dropped   int64 // >0 表示确有调度项被丢弃；0 表示仅水位过高
	used      int64
	capacity  int64
}

// emit 在锁外输出告警
func (m *TimerManager) emit(w *addTimerWarning) {
	if w == nil {
		return
	}
	if w.dropped > 0 {
		m.notifyBackpressure(w.wheel, w.wheelType, w.dropped)
		return
	}
	m.notifyHighWatermark(w.wheel, w.wheelType, w.used, w.capacity)
}

// AddTimer 向管理器添加定时器（业务首次注册与「先 Pause 再 AddTimer」复活都走这里；
// 已 Remove 作废的实例会被拒绝并返回 ErrTimerReleased）
func (m *TimerManager) AddTimer(timer TimerInterface) error {
	if timer == nil {
		return ErrTimerNilParent
	}
	parent := timer.GetParent()
	if parent == nil {
		return ErrTimerNilParent
	}

	// 定时器私有锁：把「清理旧调度 → 推进代次 → 恢复活跃 → 入队」整体与
	// handleTimerAdd 的「校验 → 入链」串行化。若不加锁，handleTimerAdd 校验
	// 通过后、入链执行前，本函数可完整跑完一轮并让新 task 先入链，对方随后
	// 拿着过期校验结果把同一节点二次入链，旧轮 timerCount 永久多 1（幻影 +1）。
	// 加锁顺序恒为 mu → wheelLock（handleTimerAdd 同序），不存在逆序死锁。
	parent.mu.Lock()
	err, warn := m.addTimerLocked(timer, parent, 0)
	parent.mu.Unlock()
	m.emit(warn)
	return err
}

// addTimerLocked 入队一个调度项，调用方必须已持有 parent.mu。
// 返回的告警必须在解锁后由调用方 emit，临界区内绝不写日志。
//
// renewGen == 0 表示业务注册/复活；非 0 表示到期续期，此时必须携带被派发那条
// 调度项的代次：代次纹丝未动才继续调度，否则返回 ErrTimerCanceled。少了这道
// 校验，与 Pause/Remove 竞态的续期会把「刚被业务停掉」的定时器重新置为活跃并
// 入链（rpc 重连成功后仍在不停重连）。只比代次不比 IsActive：上一次入队失败时
// 是我们自己把活跃标记回滚成 false 的，要求活跃会让重试永远作废，而业务停表对
// 一个活跃定时器必定推进代次，代次比对足以拦下复活。
func (m *TimerManager) addTimerLocked(timer TimerInterface, parent *Timer, renewGen uint64) (err error, warn *addTimerWarning) {
	if m.isClosed.Load() {
		return ErrTimerManagerClosed, nil
	}

	if renewGen != 0 {
		if parent.gen.Load() != renewGen {
			return ErrTimerCanceled, nil
		}
		// 代次没动却已进入作废流程：只能是移除时定时器本就不活跃（未推进代次）
		// 的边角场景。续期方是本协程自己，返回取消即可，不算业务复活失败。
		if parent.abandoned() {
			return ErrTimerCanceled, nil
		}
	} else if parent.abandoned() {
		// 该实例已永久归还对象池（或正排队等待 endTick 归还），契约上旧主人已放弃
		// 它，池随时可能把它发给下一个 NewTimer。实例自身分不清「刚归还的我」和
		// 「已易主的我」，因此一律拒绝复活，宁可让业务显式新建。
		timerPool.reviveRefused.Add(1)
		return ErrTimerReleased, nil
	}

	// 先选轮再改状态：类型非法时保持定时器原样返回，不必回滚活跃标记。
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType, nil
	}
	wheel := m.wheels[wheelType]

	// 没有分配时，分配唯一ID
	if parent.uid.Load() == 0 {
		parent.uid.CompareAndSwap(0, globalTimerUID.Add(1))
	}

	// 清理现有定时器：内部会推进代次，使在途的旧调度项失效，避免重复调度。
	// 此处已持有 parent.mu，直接调用无锁版本，避免重复加锁死锁。
	parent.removeFromManagerLocked(false)
	// RemoveFromManager 只在「原本是活跃」时才推进代次；定时器被业务临时置为
	// inactive（先 Remove 再 AddTimer 复活）时它会提前返回，此时在途的旧调度项
	// 仍持有同一个 gen，会被 isValid 误判为有效，导致同一实例被重复调度。
	// 因此这里无条件再推进一次，保证「每次 AddTimer 产生的调度项 gen 全局唯一」。
	// 直接取 Add 的返回值作为本代次：原子加法返回的新值天然全局唯一。若这里丢弃
	// 返回值、入队前再 Load，两个并发 AddTimer 会读到同一个更大的 gen，各自入队
	// 一份相同 gen 的 task，isValid 全部放行 → 同一节点被二次入链、计数多加，
	// 后续配对减完就出现 -1。这正是一次性修复引用计数出现 -1 的主因。
	newGen := parent.gen.Add(1)
	// RemoveFromManager 会把 isActive 置为 false，这里恢复活跃状态重新进入调度
	parent.isActive.Store(true)

	chanLen := int64(len(wheel.addTimerChan))
	chanCap := int64(cap(wheel.addTimerChan))

	// 使用上面 Add 返回的唯一代次入队，杜绝并发 AddTimer 的重复 gen
	task := timerTask{timer: timer, gen: newGen, mgr: m}

	select {
	case wheel.addTimerChan <- task:
		m.stats.timersAdded.Add(1)
		if float64(chanLen) >= float64(chanCap)*0.9 {
			return nil, &addTimerWarning{wheel: wheel, wheelType: wheelType, used: chanLen, capacity: chanCap}
		}
		return nil, nil
	default:
		// 通道满：丢弃本次调度。入队失败即意味着节点不在任何轮里，
		// 必须把 isActive 撤回去：留着 true 就是「幻影活跃」——IsActive()
		// 谎报在调度，而 processWheelTick 的失活清理分支也永远不会回收它。
		// 绝不阻塞、绝不新开协程——原先的「阻塞 100ms 后 go executeTimer」
		// 会与 executeTimer 内部的续期 AddTimer 构成递归 fork，
		// 下游 Tick 一旦变慢就会指数级堆积协程直至 OOM。
		parent.isActive.Store(false)
		m.stats.timersDropped.Add(1)
		// 这条调度项到此为止：既没入链也不会有人再执行，计入「脱离调度」
		m.stats.timersRemoved.Add(1)
		return ErrTimerChannelFull, &addTimerWarning{wheel: wheel, wheelType: wheelType, dropped: 1}
	}
}

// executeTimer 执行定时器并在需要时重新调度。
//
// 调用前提：不得持有任何 wheelLock。
// Tick 内部允许调用 Remove / Pause / AddTimer（业务最常见的写法），
// 因此本函数必须在完全无锁的上下文中运行，否则会自锁死锁。
func (m *TimerManager) executeTimer(task timerTask) {
	if !task.isValid() {
		return // 已被移除或被复用的过期调度项，直接丢弃
	}
	parent := task.timer.GetParent()

	// 在飞标记：Tick 执行期间禁止把实例归还对象池，否则新主人会和这个回调同时
	// 写同一块内存。beginTick 内部会复核代次与作废状态，校验通过后到真正打上
	// 在飞标记之间不存在可被池认领的窗口；失败说明实例已易主，直接放弃本次回调。
	// endTick 必须是本函数最后一个动作——它返回后实例随时已易主，
	// 再读一次 task.timer 就是踩别人的对象。
	if !parent.beginTick(task.gen) {
		return
	}
	defer parent.endTick()

	func() {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("定时器执行发生panic: %v", err)
			}
		}()
		task.timer.Tick()
	}()
	m.stats.timersExecuted.Add(1)

	// Tick 内部可能已经移除/复用该定时器，或系统已关闭，重新校验后再续期
	if m.isClosed.Load() || !task.isValid() {
		return
	}
	if !task.timer.HasNext() {
		// HasNext 为 false 有两种成因，必须用活跃标记区分：
		//   1) 有限次数自然耗尽——定时器仍是活跃的，本条就是它的最后一次执行，
		//      就此作废并归还对象池，否则实例永久滞留在无人管理的状态；
		//   2) 业务在 Tick 里自己 Pause()/Remove() 了——收尾已由那两次调用做完，
		//      这里再动手会把「只是暂停」的定时器送进池，把所有权交出去。
		if parent.IsActive() {
			m.retireAfterExhaustion(parent)
		}
		return
	}
	if !task.isValid() {
		return
	}
	if err := m.addTimerWithRetry(task.timer, task.gen); err != nil {
		if errors.Is(err, ErrTimerCanceled) {
			return // 续期窗口内业务已 Pause/Remove 或已自行重新注册：本次作废，不算失败
		}
		m.stats.timersRescheduleFailed.Add(1)
		m.notifyTimerLost(task.timer, parent, err)
	}
}

// notifyTimerLost 上报「续期彻底失败」的定时器，并把它强制归还对象池。
//
// 重试仍失败说明这条调度项已经永久消失：节点不在任何轮里、isActive 也已回滚，
// 业务侧表现为该定时器的 Tick 再也不触发，且从外部完全无法察觉。这里主动作废
// 实例（此刻 inTick 仍为 1，归还会由 endTick 落地），把「静默失联」变成一次
// 显式回收 + 一条可接管的回调，业务据此重建定时器而不是等一个永远不来的 Tick。
func (m *TimerManager) notifyTimerLost(timer TimerInterface, parent *Timer, cause error) {
	info := TimerLostInfo{
		Manager:   m,
		UID:       timer.GetUID(),
		Interval:  parent.GetInterval(),
		Remaining: parent.GetRemainingCount(),
		Cause:     cause,
	}
	vars.Error("定时器续期失败已强制回收: uid=%d, 间隔=%dms, 剩余次数=%d, 原因=%v",
		info.UID, info.Interval, info.Remaining, cause)

	if fn := onTimerLost.Load(); fn != nil {
		// 回调来自业务，绝不能让它把收尾（下面的 retire 与 endTick）打断
		func() {
			defer func() {
				if r := recover(); r != nil {
					vars.Error("OnTimerLost 回调发生panic: %v", r)
				}
			}()
			(*fn)(info)
		}()
	}
	m.retireAfterExhaustion(parent)
}

// retireAfterExhaustion 作废执行完最后一拍的定时器并归还对象池。
//
// 此刻 inTick 仍为 1，归还请求会被挂起、由 executeTimer 尾部的 endTick 落地，
// 因此本函数返回时实例还没有交给池，调用链上后续的状态读取都是安全的。
func (m *TimerManager) retireAfterExhaustion(parent *Timer) {
	parent.mu.Lock()
	parent.removeFromManagerLocked(true)
	parent.mu.Unlock()
	// 不在这里补 Put：结论必为 releaseDeferred（inTick 仍为 1），
	// 由 executeTimer 尾部的 endTick 独占落地，避免两头争同一次归还。
}

// addTimerWithRetry 有界重试重新入队。
//
// 这是「不丢任务」的最后一道关卡：AddTimer 失败时节点已经离开时间轮，
// 不复投就会永久消失，业务侧表现为该定时器的 Tick 再也不触发。
// 重试次数刻意很小且不 sleep —— 续期跑在 timeTick 消费协程的关键路径上，
// 任何额外等待都会放大整条链路的延迟。
func (m *TimerManager) addTimerWithRetry(timer TimerInterface, expectGen uint64) error {
	const maxRetry = 3
	parent := timer.GetParent()
	if parent == nil {
		return ErrTimerNilParent
	}

	var err error
	for i := 0; i < maxRetry; i++ {
		var warn *addTimerWarning
		err, warn = m.addTimerWithLock(timer, parent, expectGen)
		if warn != nil {
			// 续期跑在 timeTick 消费协程上：限频告警每秒最多一条，
			// 且此时 parent.mu 已释放，可以安全输出。
			m.emit(warn)
		}
		if err == nil || !errors.Is(err, ErrTimerChannelFull) {
			return err
		}
		// 上一次尝试已烧掉一个代次却没入队，重试的期望值改为「当前代次」：
		// 语义从「还是我这条被派发的调度项」放宽为「期间无人再改动该定时器」。
		expectGen = parent.gen.Load()
		runtime.Gosched()
	}
	return err
}

// addTimerWithLock 取定时器私有锁后入队（续期路径不走 AddTimer 公共入口，
// 以便把期望代次一路传进临界区）。
func (m *TimerManager) addTimerWithLock(timer TimerInterface, parent *Timer, expectGen uint64) (error, *addTimerWarning) {
	parent.mu.Lock()
	err, warn := m.addTimerLocked(timer, parent, expectGen)
	parent.mu.Unlock()
	return err, warn
}

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

// GetStats 返回性能统计信息
func (m *TimerManager) GetStats() TimerStats {
	return TimerStats{
		TimersAdded:     m.stats.timersAdded.Load(),
		TimersRemoved:   m.stats.timersRemoved.Load(),
		TimersExecuted:  m.stats.timersExecuted.Load(),
		TimersDropped:   m.stats.timersDropped.Load(),
		WheelMigrations: m.stats.wheelMigrations.Load(),

		TimersRescheduleFailed: m.stats.timersRescheduleFailed.Load(),
	}
}

// GetTimerCount 返回所有时间轮中的定时器总数
func (m *TimerManager) GetTimerCount() int64 {
	var total int64
	for _, wheel := range m.wheels {
		total += wheel.timerCount.Load()
	}
	return total
}

// timerRuntime 是一次 Run → TimeStop 生命周期的全部可变状态。
//
// 整体用 atomic.Pointer 替换，而不是把 closech / tickWG / ctx 摊成三个裸全局：
// Run 写、TimeStop 读并关闭、TimeTick 在 select 里读，逐字段访问必然竞态，
// 且 TimeStop 对 closech「先查后关」不是原子的，两个并发调用会双重 close
// 直接触发 fatal 的 "close of closed channel"。Swap 保证只有一方拿到非 nil
// 实例，closeOnce 再做一层幂等兜底。
type timerRuntime struct {
	closech   chan struct{}
	closeOnce sync.Once
	tickWG    sync.WaitGroup
	ctx       context.Context
}

// shutdown 关闭本轮生命周期，幂等且并发安全
func (rt *timerRuntime) shutdown() {
	rt.closeOnce.Do(func() { close(rt.closech) })
}

// TimerLostInfo 描述一个彻底失联（续期重试仍失败、已被强制回收）的定时器。
type TimerLostInfo struct {
	Manager   *TimerManager // 归属管理器
	UID       int64         // 定时器唯一ID
	Interval  int64         // 执行间隔（毫秒）
	Remaining int64         // 剩余执行次数，-1 表示无限
	Cause     error         // 续期失败的最终原因
}

// onTimerLost 业务注册的失联回调（copy-on-write，读写不竞争）
var onTimerLost atomic.Pointer[func(TimerLostInfo)]

// SetOnTimerLost 注册「定时器彻底失联」回调；传 nil 取消。
//
// 正常路径下该回调永不触发；一旦触发说明下游 Tick 太慢导致调度项被丢弃，
// 业务必须据此重建定时器。回调在 timeTick 消费协程上执行，禁止阻塞。
func SetOnTimerLost(fn func(TimerLostInfo)) {
	if fn == nil {
		onTimerLost.Store(nil)
		return
	}
	onTimerLost.Store(&fn)
}

// 全局状态（定时器系统单例相关）
var (
	// timerRegistryMu 冻结注册表的「批量关闭」与「单个注册」：Run/TimeStop 取待关
	// 清单的窗口内，并发 NewTimerManager 必须排在它后面。否则刚建好的管理器会
	// 落在清单外却被随后的清空抹掉，且从未 Close —— 它名下 5 个时间轮协程 +
	// 5 个 ticker 永久泄漏。清单内的管理器各自 Close 时自摘，不碰这段临界区。
	timerRegistryMu     sync.Mutex
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	globalTimerUID      atomic.Int64                            // 定时器ID：跨管理器全局唯一
	defaultTimerManager atomic.Pointer[TimerManager]
	timerRT             atomic.Pointer[timerRuntime]

	// timerChannel 刻意保持全局、跨生命周期共享：
	// NewTimerManager 可以在未 Run 的情况下独立调用，此时也得有地方派发。
	timerChannel      chan timerTask
	timerChannelMu    sync.Mutex
	timerChannelReady bool
)

// registerTimerManager 把管理器登记进注册表
func registerTimerManager(mgr *TimerManager) {
	timerRegistryMu.Lock()
	timerManagerMap.Store(mgr, true)
	timerRegistryMu.Unlock()
}

// unregisterTimerManager 从注册表摘除管理器（只由该管理器自己的 Close 调用）
func unregisterTimerManager(mgr *TimerManager) {
	timerRegistryMu.Lock()
	timerManagerMap.Delete(mgr)
	timerRegistryMu.Unlock()
}

// snapshotTimerManagers 在冻结注册表的窗口内取出当前存量的全部管理器。
//
// 返回后立刻放开锁：Close 内部会反过来申请这把锁做自摘，持锁等待退出会自锁。
func snapshotTimerManagers() []*TimerManager {
	timerRegistryMu.Lock()
	defer timerRegistryMu.Unlock()

	mgrs := make([]*TimerManager, 0, timerManagerMap.Length())
	timerManagerMap.Range(func(mgr *TimerManager, _ bool) bool {
		mgrs = append(mgrs, mgr)
		return true
	})
	return mgrs
}

// ensureTimerChannel 确保调度通道已创建（可被 Stop 后重建）
func ensureTimerChannel() {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	if !timerChannelReady || timerChannel == nil {
		timerChannel = make(chan timerTask, MaxTimerChannelNum)
		timerChannelReady = true
	}
}

// resetTimerChannel 释放调度通道。
// 刻意不 close：未被管理器纳管（或停止后仍在运行）的时间轮可能仍在发送，
// close 会直接触发 "send on closed channel" 的致命 panic。
func resetTimerChannel() {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	timerChannel = nil
	timerChannelReady = false
}

// currentTimerChannel 返回当前调度通道快照（无通道时为 nil）
func currentTimerChannel() chan timerTask {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	return timerChannel
}

// NewTimerManager 创建新的定时器管理器
func NewTimerManager() *TimerManager {
	ensureTimerChannel()

	// 时间轮配置：毫秒/秒/分钟/10分钟/小时
	wheelConfigs := []int64{
		1, // 毫秒级
		util.MILLISECONDS_OF_SECOND,
		util.MILLISECONDS_OF_MINUTE,
		util.MILLISECONDS_OF_10_MINUTE,
		util.MILLISECONDS_OF_HOUR,
	}

	mgr := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, len(wheelConfigs)),
	}

	// 初始化时间轮
	for i, config := range wheelConfigs {
		wheel := &TimerWheel{
			wheelConfig: config,
			// 时间轮只按顺序遍历与摘除节点，从不按 ID 查节点，
			// 用无索引链表省掉每次入链取号（CAS+时钟）与 map 写删。
			tickWheel:    list.NewUnindexedList(),
			addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
			mgr:          mgr,
		}
		wheel.isRunning.Store(true)
		mgr.wheels[i] = wheel

		// 启动时间轮协程
		mgr.wheelWG.Add(1)
		go func(w *TimerWheel, t TimerType) {
			defer mgr.wheelWG.Done()
			mgr.runWheel(w, t)
		}(wheel, TimerType(i))
	}

	registerTimerManager(mgr)
	return mgr
}

// runWheel 运行时间轮循环
func (m *TimerManager) runWheel(wheel *TimerWheel, wheelType TimerType) {
	ticker := time.NewTicker(time.Duration(wheel.wheelConfig) * time.Millisecond)
	defer ticker.Stop()

	vars.Info("启动时间轮: %s (精度: %dms)", wheelType.String(), wheel.wheelConfig)

	for wheel.isRunning.Load() {
		// recover 放在循环内：避免单次 panic 直接杀死时间轮协程，导致后续所有定时器失效
		func() {
			defer func() {
				if err := recover(); err != nil {
					vars.Error("时间轮运行发生panic错误: %v, 类型: %s", err, wheelType.String())
				}
			}()

			select {
			case <-m.closeChan:
				wheel.isRunning.Store(false)
				m.cleanupWheel(wheel, wheelType)
				vars.Info("时间轮停止: %s", wheelType.String())

			case task, ok := <-wheel.addTimerChan:
				// 通道永不关闭（resetTimerChannel 刻意不 close，见其注释），
				// 此分支只是防御性兜底：真收到 ok=false 说明上游被改坏了，
				// 继续循环只会对着 nil 通道空转，直接退出把这个轮暴露出来。
				if !ok {
					wheel.isRunning.Store(false)
					return
				}
				m.handleTimerAdd(wheel, task)

			case <-ticker.C:
				m.processWheelTick(wheel, wheelType)
			}
		}()
	}
}

// migratingMarker 是「节点已离开原轮、调度项正在投递途中」的占位归属。
//
// 投递必须先把归属改成该标记、再往目标通道发送：原先是「先投通道、后置 wheel=nil」，
// 目标轮的消费协程可能在本协程改写归属之前就跑完 handleTimerAdd 的校验，看到旧轮引用
// 仍在便判定「已入链」而丢弃这条调度项；本协程随后置 nil，节点于是两头无归属、
// 原轮计数已扣减、目标轮永不接收 —— 定时器永久停摆，且计数毫无漂移可查。
// handleTimerAdd 的认领条件因此放宽为「无归属或持有该标记」。
var migratingMarker = &TimerWheel{}

// ownedWheel 返回节点真正归属的时间轮；处于在途标记状态时视为「无归属」。
func (t *Timer) ownedWheel() *TimerWheel {
	if w := t.wheel.Load(); w != migratingMarker {
		return w
	}
	return nil
}

// handleTimerAdd 处理定时器添加到时间轮
func (m *TimerManager) handleTimerAdd(wheel *TimerWheel, task timerTask) {
	if !task.isValid() {
		return // 过期调度项：定时器已被移除或复用，丢弃
	}

	node, ok := task.timer.(list.INode)
	if !ok {
		vars.Error("定时器未实现 list.INode，无法加入时间轮")
		return
	}

	parent := task.timer.GetParent()
	// 先取定时器私有锁、再取时间轮锁（与 AddTimer 加锁顺序一致）：
	// 把「校验 + 入链 + 计数」与 AddTimer 的「摘链 + 推进代次 + 入队」串行化。
	// gen 唯一 ≠ 只有一份 task 会入链——不加锁时，本校验通过后、入链执行前，
	// 并发 AddTimer 可让更新的 task 先入链，本函数再把同一节点二次入链，
	// list.Add 的 detach+relink 使节点物理迁移到新链表，旧轮 timerCount 永远
	// 多 1（count 与 len 漂移：幻影 +1 / -1）。
	parent.mu.Lock()
	defer parent.mu.Unlock()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 加锁后再次校验：并发 Remove 可能刚好发生在这之前
	if !task.isValid() {
		return
	}

	// 已进入作废流程的实例绝不入链：在途调度项可能晚于 Put 才被本轮消化，
	// 此刻 isActive/gen 都可能已被下一个主人重新用起，光靠 isValid 分不出来。
	// 持锁路径禁止打日志（vars 异步缓冲满时最长阻塞 5 秒，会拖死整个时间轮）。
	if parent.abandoned() {
		return
	}

	// 节点已真实归属某个轮（不是「投递途中」的占位标记）才丢弃本次入链：能走到
	// 这里且归属非空，说明节点已带着当前代次正确在链（或已易主到别的轮），
	// 再次入链只会让计数多加。处于 migratingMarker 的正是刚从原轮摘链、投给本轮
	// 的迁移项，必须认领，否则原轮计数已扣、目标轮不接手，定时器永久停摆。
	if cur := parent.wheel.Load(); cur != nil && cur != migratingMarker {
		return
	}

	// 入链失败必须回滚归属且不加计数：list.Add 内部 recover 后返回 false 时
	// 节点并不在链表里，留着归属或计数都会形成永久幻影 +1。
	if !wheel.tickWheel.Add(node) {
		parent.wheel.Store(nil)
		return
	}
	parent.wheel.Store(wheel)
	// 计数只在「节点真正入链」处自增，与 processWheelTick / cleanupWheel 的
	// 自减严格配对。放在 AddTimer 里统计的是入队次数，迁移到达不经过那里，
	// 会让计数每次迁移净减一。
	wheel.timerCount.Add(1)
}

// processWheelTick 处理时间轮滴答事件
//
// 背压原则：通道满就把定时器留在当前轮等下一个 tick 重试，绝不新开协程。
// 原先的「go executeTimer」在毫秒轮上是 1ms 一次的无限 fork 源，
// 消费端只有单个 TimeTick 协程且同步执行业务 Tick，下游一慢就会堆积到 OOM。
//
// 本函数只做无锁的准备工作，临界区整体交给 tickWheelSection：那里的 panic
// 必须能释放 wheelLock，否则一次异常就把这个轮永久扣死（Close 随之挂死）。
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	dropped := m.tickWheelSection(wheel, wheelType)

	// 告警必须在解锁之后：vars 异步日志缓冲满时会阻塞最长 5 秒
	if dropped > 0 {
		m.stats.timersDropped.Add(dropped)
		m.notifyBackpressure(wheel, wheelType, dropped)
	}
}

// tickWheelSection 扫描时间轮，派发到期项并下沉到更精确的轮。
//
// 判定全程不持 wheelLock：List.Range 只在链表自己的读锁里取快照，回调在锁外跑，
// 于是一次长扫描不再把业务侧的 Remove 整段排在后面（detachFromWheelLocked 要
// 同一把锁）。只有真正命中的节点才回锁，且锁内只剩「复核 → 认领 → 投递 → 摘链」
// 这几步无阻塞动作（投递用非阻塞 select，满了立刻放手）。
//
// 认领必须和摘链在同一把轮锁里做：业务移除是「持锁读归属 → 摘链 → 扣计数」的
// check-then-act，若 CAS 漂在锁外，两边能各自读到同一个归属并各扣一次计数
// （不变前的整轮持锁恰好挡住了这个交错，实测会让 timerCount 打成负数）。
func (m *TimerManager) tickWheelSection(wheel *TimerWheel, wheelType TimerType) (dropped int64) {
	currentTime := util.CurrentMS()
	ch := currentTimerChannel()

	wheel.tickWheel.Range(func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil {
			return true
		}
		// 已被迁移到别的轮或正在投递途中：归目标轮处理
		if parent.ownedWheel() != wheel {
			return true
		}
		if !parent.IsActive() {
			// 业务已移除，但 RemoveFromManager 与 handleTimerAdd 竞态：
			// 节点在 wheel 引用写入前就被判为「无轮」而漏摘，随后又被入链。
			// 它既不会被调度也不会被回收，计数还永远多 1 —— 顺手清掉。
			m.unlinkStale(wheel, parent, node)
			return true
		}

		task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
		if parent.nextTime.Load() <= currentTime {
			// 时间到达：只有派发成功才摘链，失败则留在轮里下个 tick 重试
			if m.commitDispatch(wheel, parent, node, task, ch) == dispatchFull {
				dropped++
			}
			return true
		}

		// 检查是否需要迁移到更精确的时间轮
		remaining := parent.nextTime.Load() - currentTime - TimerMigrationOffset
		newType := parent.calculateType(remaining)
		if newType == wheelType || int(newType) >= len(m.wheels) {
			return true
		}
		switch m.commitDispatch(wheel, parent, node, task, m.wheels[newType].addTimerChan) {
		case dispatchSent:
			m.stats.wheelMigrations.Add(1)
		case dispatchFull:
			dropped++
		}
		return true
	})

	return dropped
}

// commitDispatch 的三种结论。
const (
	dispatchSent    int8 = iota // 已投递，节点已摘链并扣减计数
	dispatchSkipped             // 归属被别的路径拿走，本次什么都没做
	dispatchFull                // 目标通道满，归属已还原，留在本轮等下个 tick
)

// commitDispatch 在短临界区内完成复核、认领、投递与摘链扣计数。
//
// 先取 parent.mu 再取 wheelLock（与 AddTimer、handleTimerAdd、Remove 同序）：
// 目标轮的消费协程入链前也要这把 mu 锁，于是它不可能在本协程还挂着这个节点时
// 抢先 detach 走它 —— 那样本轮的摘链会错摘到目标链上，计数两头漂移。
//
// 先置「投递途中」再发送：反向顺序会让消费端在归属仍指向本轮时校验入链条件，
// 把这条调度项当重复入链丢弃。
func (m *TimerManager) commitDispatch(wheel *TimerWheel, parent *Timer, node list.INode, task timerTask, dst chan<- timerTask) int8 {
	parent.mu.Lock()
	defer parent.mu.Unlock()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 从快照判定到抢到锁的这段时间里节点可能已易主：归属仍是本轮才动手
	if parent.ownedWheel() != wheel || !parent.claimFromWheel(wheel) {
		return dispatchSkipped
	}
	select {
	case dst <- task:
		node.GetNode().Remove()
		wheel.timerCount.Add(-1)
		return dispatchSent
	default:
		// 通道满：归属必须原样还给本轮，否则本节点被 ownedWheel 判给「无轮」，
		// 后续每个 tick 都会跳过它，等价于永久停摆。
		parent.wheel.Store(wheel)
		return dispatchFull
	}
}

// unlinkStale 回锁清理「已不活跃却仍挂在轮上」的节点：摘链、扣计数、断开归属。
// 归属复核与认领同在轮锁内，理由同 commitDispatch。
func (m *TimerManager) unlinkStale(wheel *TimerWheel, parent *Timer, node list.INode) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	if parent.ownedWheel() != wheel || !parent.claimFromWheel(wheel) {
		return
	}
	node.GetNode().Remove()
	wheel.timerCount.Add(-1)
	parent.wheel.Store(nil)
	m.stats.timersRemoved.Add(1)
}

// cleanupWheel 清理时间轮
//
// 关键：业务回调（Tick）必须在完全释放 wheelLock 之后执行。
// Tick 内部调用 Remove / AddTimer 会再次申请同一把 wheelLock，
// 若持锁执行会立即自锁，导致 TimeStop 永久挂起（进程停不下来）。
//
// 收尾预算来自 CloseCtx：每个待补跑的回调执行前都复查一次 ctx，超时就放弃剩余
// 回调并把实例强制归还对象池。一个卡在下游锁上的 Tick 不该拖死整个进程退出。
func (m *TimerManager) cleanupWheel(wheel *TimerWheel, wheelType TimerType) {
	// 阶段一：持锁摘链，收集已过期的定时器（临界区独立成方法，panic 也不会扣住轮锁）
	expired := m.drainWheelLocked(wheel)

	ctx := context.Background()
	if p := m.closeCtx.Load(); p != nil {
		ctx = *p
	}

	// 阶段二：无锁执行（此时 Tick 内 Remove/AddTimer 都不会再触碰 wheelLock）
	for i, task := range expired {
		if ctx.Err() != nil {
			m.abandonExpired(wheelType, expired[i:])
			return
		}
		m.executeTimer(task)
	}
}

// abandonExpired 收尾超时时强制作废剩余定时器并归还对象池。
//
// 此刻没有回调在飞（inTick==0），作废结论为 releaseNow，因此必须在解锁后自行
// 归还——这一步不能省，否则实例既离开了时间轮又留在池外，永久占着内存。
// drainWheelLocked 已把这批定时器计入 timersRemoved，这里不重复计数。
func (m *TimerManager) abandonExpired(wheelType TimerType, tasks []timerTask) {
	for _, task := range tasks {
		if task.timer == nil {
			continue
		}
		parent := task.timer.GetParent()
		if parent == nil {
			continue
		}
		parent.mu.Lock()
		outcome := parent.removeFromManagerLocked(true)
		parent.mu.Unlock()
		if outcome == releaseNow {
			parent.releaseToPool()
		}
	}
	vars.Warning("收尾超时，强制归还定时器: 轮=%s, 数量=%d, 未执行到期回调",
		wheelType.String(), len(tasks))
	// 监控口径：这些定时器连最后一次 Tick 都没补上
	m.stats.timersDropped.Add(int64(len(tasks)))
}

// drainWheelLocked 持锁清空时间轮，返回需要补跑一次到期回调的调度项。
func (m *TimerManager) drainWheelLocked(wheel *TimerWheel) (expired []timerTask) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := util.CurrentMS()

	wheel.tickWheel.Range(func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil {
			return true
		}
		// 已投递途中的节点不归本轮收尾：它的归属已是在途标记，
		// 由接收方或续期方负责，这里再动它会把别人的调度项打断。
		if parent.ownedWheel() != wheel {
			return true
		}
		// 无论是否到期都要断开 wheel 引用：本函数随后会 Clear 整个轮并把计数归零，
		// 残留引用会让业务侧后续的 Remove 误判节点仍在轮里，把计数减成负数。
		parent.wheel.Store(nil)
		if parent.IsActive() && parent.nextTime.Load() <= currentTime {
			node.GetNode().Remove()
			expired = append(expired, timerTask{timer: timer, gen: parent.gen.Load(), mgr: m})
		}
		return true
	})

	// 清空时间轮
	removedCount := wheel.timerCount.Load()
	wheel.tickWheel.Clear()
	wheel.timerCount.Store(0)
	m.stats.timersRemoved.Add(removedCount)
	return expired
}
