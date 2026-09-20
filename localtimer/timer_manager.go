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
func (t timerTask) isValid() bool {
	if t.timer == nil {
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
}

// TimerManager 管理所有时间轮
type TimerManager struct {
	closeChan   chan struct{}      // 关闭信号
	wheels      []*TimerWheel      // 时间轮数组
	maxTimerUID atomic.Int64       // 最大定时器ID
	isClosed    atomic.Bool        // 是否关闭
	stats       timerStatsCounters // 内部原子计数
	wheelWG     sync.WaitGroup

	// 上次背压告警时间（毫秒）。背压时每个时间轮每秒最多告警一条，避免刷爆日志。
	lastBackpressureWarn atomic.Int64
}

// TimerStats 保存性能统计信息（快照值，非实时引用）
type TimerStats struct {
	TimersAdded     int64 // 已添加定时器数量
	TimersRemoved   int64 // 已移除定时器数量
	TimersExecuted  int64 // 已执行定时器数量
	TimersDropped   int64 // 因背压被丢弃的调度次数
	WheelMigrations int64 // 时间轮迁移次数

	TimersRescheduleFailed int64 // 续期重试后仍失败、定时器可能丢失的次数（>0 说明需要告警）
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

// notifyBackpressure 背压限频告警：每个管理器每秒最多一条。
//
// 必须在释放 wheelLock 之后调用——vars 的异步日志在缓冲写满时会阻塞最长 5 秒
// （见 vars/async_channel.go 的阻塞模式分支），持锁调用会把整个时间轮拖死。
func (m *TimerManager) notifyBackpressure(wheelType TimerType, dropped int64) {
	now := util.CurrentMS()
	last := m.lastBackpressureWarn.Load()
	if now-last < 1000 {
		return
	}
	if !m.lastBackpressureWarn.CompareAndSwap(last, now) {
		return
	}
	vars.Warning("定时器背压: 轮=%s, 本次丢弃=%d, 累计丢弃=%d",
		wheelType.String(), dropped, m.stats.timersDropped.Load())
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
	defer parent.mu.Unlock()
	return m.addTimerLocked(timer, parent, 0)
}

// addTimerLocked 入队一个调度项，调用方必须已持有 parent.mu。
//
// renewGen == 0 表示业务注册/复活；非 0 表示到期续期，此时必须携带被派发那条
// 调度项的代次：代次纹丝未动才继续调度，否则返回 ErrTimerCanceled。少了这道
// 校验，与 Pause/Remove 竞态的续期会把「刚被业务停掉」的定时器重新置为活跃并
// 入链（rpc 重连成功后仍在不停重连）。只比代次不比 IsActive：上一次入队失败时
// 是我们自己把活跃标记回滚成 false 的，要求活跃会让重试永远作废，而业务停表对
// 一个活跃定时器必定推进代次，代次比对足以拦下复活。
func (m *TimerManager) addTimerLocked(timer TimerInterface, parent *Timer, renewGen uint64) error {
	if m.isClosed.Load() {
		return ErrTimerManagerClosed
	}

	if renewGen != 0 {
		if parent.gen.Load() != renewGen {
			return ErrTimerCanceled
		}
		// 代次没动却已进入作废流程：只能是移除时定时器本就不活跃（未推进代次）
		// 的边角场景。续期方是本协程自己，返回取消即可，不算业务复活失败。
		if parent.abandoned() {
			return ErrTimerCanceled
		}
	} else if parent.abandoned() {
		// 该实例已永久归还对象池（或正排队等待 endTick 归还），契约上旧主人已放弃
		// 它，池随时可能把它发给下一个 NewTimer。实例自身分不清「刚归还的我」和
		// 「已易主的我」，因此一律拒绝复活，宁可让业务显式新建。
		timerPool.reviveRefused.Add(1)
		return ErrTimerReleased
	}

	// 先选轮再改状态：类型非法时保持定时器原样返回，不必回滚活跃标记。
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType
	}
	wheel := m.wheels[wheelType]

	// 没有分配时，分配唯一ID
	if parent.uid.Load() == 0 {
		parent.uid.CompareAndSwap(0, m.maxTimerUID.Add(1))
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
			m.notifyBackpressure(wheelType, 0)
		}
		return nil
	default:
		// 通道满：丢弃本次调度。入队失败即意味着节点不在任何轮里，
		// 必须把 isActive 撤回去：留着 true 就是「幻影活跃」——IsActive()
		// 谎报在调度，而 processWheelTick 的失活清理分支也永远不会回收它。
		// 绝不阻塞、绝不新开协程——原先的「阻塞 100ms 后 go executeTimer」
		// 会与 executeTimer 内部的续期 AddTimer 构成递归 fork，
		// 下游 Tick 一旦变慢就会指数级堆积协程直至 OOM。
		parent.isActive.Store(false)
		m.stats.timersDropped.Add(1)
		m.notifyBackpressure(wheelType, 1)
		return ErrTimerChannelFull
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
		vars.Error("重新调度定时器失败(已重试): uid=%d, %v", task.timer.GetUID(), err)
	}
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
		err = m.addTimerWithLock(timer, parent, expectGen)
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
func (m *TimerManager) addTimerWithLock(timer TimerInterface, parent *Timer, expectGen uint64) error {
	parent.mu.Lock()
	defer parent.mu.Unlock()
	return m.addTimerLocked(timer, parent, expectGen)
}

// Close 优雅地关闭定时器管理器
func (m *TimerManager) Close() {
	if !m.isClosed.CompareAndSwap(false, true) {
		return // 已经关闭
	}

	// 发送关闭信号
	close(m.closeChan)

	m.wheelWG.Wait()

	for _, wheel := range m.wheels {
		wheel.isRunning.Store(false)
	}
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

// 全局状态（定时器系统单例相关）
var (
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	defaultTimerManager atomic.Pointer[TimerManager]
	timerRT             atomic.Pointer[timerRuntime]

	// timerChannel 刻意保持全局、跨生命周期共享：
	// NewTimerManager 可以在未 Run 的情况下独立调用，此时也得有地方派发。
	timerChannel      chan timerTask
	timerChannelMu    sync.Mutex
	timerChannelReady bool
)

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
			wheelConfig:  config,
			tickWheel:    list.NewList(),
			addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
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

	timerManagerMap.Store(mgr, true)
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
				m.cleanupWheel(wheel)
				vars.Info("时间轮停止: %s", wheelType.String())

			case task, ok := <-wheel.addTimerChan:
				if !ok {
					// 通道已关闭
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
		m.notifyBackpressure(wheelType, dropped)
	}
}

// tickWheelSection 在 wheelLock 临界区内完成派发与迁移，返回因背压丢弃的次数。
func (m *TimerManager) tickWheelSection(wheel *TimerWheel, wheelType TimerType) (dropped int64) {
	currentTime := util.CurrentMS()
	ch := currentTimerChannel()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

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
			node.GetNode().Remove()
			parent.wheel.Store(nil)
			wheel.timerCount.Add(-1)
			return true
		}

		if parent.nextTime.Load() <= currentTime {
			// 时间到达：只有派发成功才摘链，失败则留在轮里下个 tick 重试
			task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
			// 先置「投递途中」再发送：反向顺序会让消费端在归属仍指向本轮时
			// 校验入链条件，把这条调度项当重复入链丢弃。
			parent.wheel.Store(migratingMarker)
			select {
			case ch <- task:
				node.GetNode().Remove()
				wheel.timerCount.Add(-1)
			default:
				// 通道满：归属必须原样还给本轮，否则本节点被 ownedWheel 判给「无轮」，
				// 后续每个 tick 都会跳过它，等价于永久停摆。
				parent.wheel.Store(wheel)
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

		task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
		parent.wheel.Store(migratingMarker)
		select {
		case m.wheels[newType].addTimerChan <- task:
			// 迁移成功：归属保持在途标记，由目标轮 handleTimerAdd 认领；
			// 本轮只负责摘链与扣计数。
			node.GetNode().Remove()
			wheel.timerCount.Add(-1)
			m.stats.wheelMigrations.Add(1)
		default:
			// 目标时间轮通道已满，归属与链表位置一起还原，等下一个 tick
			parent.wheel.Store(wheel)
			dropped++
		}
		return true
	})

	return dropped
}

// cleanupWheel 清理时间轮
//
// 关键：业务回调（Tick）必须在完全释放 wheelLock 之后执行。
// Tick 内部调用 Remove / AddTimer 会再次申请同一把 wheelLock，
// 若持锁执行会立即自锁，导致 TimeStop 永久挂起（进程停不下来）。
func (m *TimerManager) cleanupWheel(wheel *TimerWheel) {
	// 阶段一：持锁摘链，收集已过期的定时器（临界区独立成方法，panic 也不会扣住轮锁）
	expired := m.drainWheelLocked(wheel)

	// 阶段二：无锁执行（此时 Tick 内 Remove/AddTimer 都不会再触碰 wheelLock）
	for _, task := range expired {
		m.executeTimer(task)
	}
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
