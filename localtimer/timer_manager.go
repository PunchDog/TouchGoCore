package localtimer

import (
	"context"
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

	// 多线程执行池。懒创建：只有真的出现 MultiThread()==true 的定时器时才建 worker，
	// 未使用该能力时不新增任何协程（既有测试对 goroutine 数有断言）。
	// executorWorkers <= 0 表示禁用执行池，此时 MultiThread 定时器退回内联串行。
	executorMu      sync.Mutex
	executor        *timerExecutor
	executorWorkers int

	// 上次「执行池不可用 / 队列满」告警时间（毫秒），限频同上。
	lastExecutorWarn atomic.Int64
}

// TimerStats 保存性能统计信息（快照值，非实时引用）
type TimerStats struct {
	TimersAdded     int64 // 已添加定时器数量
	TimersRemoved   int64 // 已移除定时器数量
	TimersExecuted  int64 // 已执行定时器数量
	TimersDropped   int64 // 因背压被丢弃的调度次数
	WheelMigrations int64 // 时间轮迁移次数

	TimersSkipped        int64 // MultiThread 定时器因上一轮 Tick 未结束而被跳过的轮次（不消耗执行次数）
	TimersAsync          int64 // 成功投递到执行池并发执行的次数
	TimersInlineFallback int64 // 执行池未启用/已停止/队列满，退回内联执行的次数
}

// timerStatsCounters 管理器内部的原子计数器
type timerStatsCounters struct {
	timersAdded          atomic.Int64
	timersRemoved        atomic.Int64
	timersExecuted       atomic.Int64
	timersDropped        atomic.Int64
	wheelMigrations      atomic.Int64
	timersSkipped        atomic.Int64
	timersAsync          atomic.Int64
	timersInlineFallback atomic.Int64
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

// notifyExecutorBackpressure 多线程执行池降级告警：每个管理器每秒最多一条。
//
// 与 notifyBackpressure 同理，必须在释放 wheelLock 之后调用。
// e 为 nil 表示执行池未启用（配置性禁用，非故障）。
func (m *TimerManager) notifyExecutorBackpressure(e *timerExecutor) {
	now := util.CurrentMS()
	last := m.lastExecutorWarn.Load()
	if now-last < 1000 {
		return
	}
	if !m.lastExecutorWarn.CompareAndSwap(last, now) {
		return
	}

	if e == nil {
		vars.Warning("定时器执行池未启用(workers=%d)，MultiThread 定时器退回内联串行执行, 累计退回=%d",
			m.executorWorkers, m.stats.timersInlineFallback.Load())
		return
	}
	vars.Warning("定时器执行池背压: 队列已满(%d/%d)，本次退回内联执行, 累计退回=%d",
		e.Len(), e.Cap(), m.stats.timersInlineFallback.Load())
}

// executorOrCreate 懒创建并返回执行池。
//
// 只有真的出现 MultiThread()==true 的定时器才会创建 worker，未使用该能力时
// 零新增协程。创建前后都要检查 isClosed：否则 Close 之后到达的任务会重建一个
// 永远不会被销毁的池，泄漏 worker 协程。
func (m *TimerManager) executorOrCreate() *timerExecutor {
	if m.executorWorkers <= 0 {
		return nil
	}

	m.executorMu.Lock()
	defer m.executorMu.Unlock()

	if m.isClosed.Load() {
		return nil
	}
	if m.executor == nil {
		m.executor = newTimerExecutor(m, m.executorWorkers, MaxExecutorQueueNum)
		if m.executor != nil {
			vars.Info("定时器执行池已启用: workers=%d, queue=%d", m.executorWorkers, MaxExecutorQueueNum)
		}
	}
	return m.executor
}

// AddTimer 向管理器添加定时器
func (m *TimerManager) AddTimer(timer TimerInterface) error {
	if m.isClosed.Load() {
		return ErrTimerManagerClosed
	}

	parent := timer.GetParent()
	if parent == nil {
		return ErrTimerNilParent
	}

	// 没有分配时，分配唯一ID
	if parent.uid.Load() == 0 {
		parent.uid.CompareAndSwap(0, m.maxTimerUID.Add(1))
	}

	// 清理现有定时器：内部会推进代次，使在途的旧调度项失效，避免重复调度
	timer.RemoveFromManager(false)
	// RemoveFromManager 会把 isActive 置为 false，这里恢复活跃状态重新进入调度
	parent.isActive.Store(true)

	// 选择合适的时间轮
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType
	}

	wheel := m.wheels[wheelType]
	chanLen := int64(len(wheel.addTimerChan))
	chanCap := int64(cap(wheel.addTimerChan))

	// 代次必须在 RemoveFromManager 之后读取，保证与入队后的定时器一致
	task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}

	select {
	case wheel.addTimerChan <- task:
		m.stats.timersAdded.Add(1)
		if float64(chanLen) >= float64(chanCap)*0.9 {
			m.notifyBackpressure(wheelType, 0)
		}
		return nil
	default:
		// 通道满：丢弃本次调度。
		// 绝不阻塞、绝不新开协程——原先的「阻塞 100ms 后 go executeTimer」
		// 会与 executeTimer 内部的续期 AddTimer 构成递归 fork，
		// 下游 Tick 一旦变慢就会指数级堆积协程直至 OOM。
		m.stats.timersDropped.Add(1)
		m.notifyBackpressure(wheelType, 1)
		return ErrTimerChannelFull
	}
}

// executeTimer 执行定时器并在需要时重新调度（分流入口）。
//
// 调用前提：不得持有任何 wheelLock。
// Tick 内部允许调用 Remove / AddTimer（业务最常见的写法），
// 因此本函数必须在完全无锁的上下文中运行，否则会自锁死锁。
//
// 分流规则见 TimerInterface.MultiThread：
//   - false（默认）：完全保持原有的内联串行路径，行为与协程数与改造前一致；
//   - true：投递到有界执行池并发执行；池未启用 / 已停止 / 队列满时退回内联执行，
//     既不丢任务、也绝不无界新建协程（这正是原注释禁止裸 `go executeTimer` 的原因）。
func (m *TimerManager) executeTimer(task timerTask) {
	if !task.isValid() {
		return // 已被移除或被复用的过期调度项，直接丢弃
	}

	if task.timer.MultiThread() {
		e := m.executorOrCreate()
		if e != nil && e.Submit(task) {
			m.stats.timersAsync.Add(1)
			return // 由 worker 协程执行 runTick，续期也在 worker 内完成
		}
		// 执行池不可用（未启用/已停止）或队列已满：退回当前协程内联执行，绝不丢任务
		m.stats.timersInlineFallback.Add(1)
		m.notifyExecutorBackpressure(e)
	}

	m.runTick(task)
}

// runTick 执行业务回调并在需要时续期。
//
// 调用前提：不得持有任何 wheelLock；Tick 内的 panic 会被逐次 recover，
// 不会杀死调用方协程（内联时是 TimeTick 消费协程，并行时是 worker 协程）。
func (m *TimerManager) runTick(task timerTask) {
	timer := task.timer
	parent := timer.GetParent()
	if parent == nil {
		return
	}

	// 重叠保护只对 MultiThread 定时器生效：
	// 串行路径由单个消费协程内联执行，天然不会重叠；
	// 并行路径下若上一轮 Tick 还没跑完又到期，则跳过本轮，
	// 只推进下次执行时间、不消耗剩余执行次数，避免业务状态被并发 Tick 破坏。
	multithread := timer.MultiThread()
	if multithread && !parent.tryBeginRun() {
		m.stats.timersSkipped.Add(1)
		m.reschedule(task, true)
		return
	}

	func() {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("定时器执行发生panic: %v", err)
			}
		}()
		timer.Tick()
	}()
	// Tick 返回（或 panic 被吞掉）后立即释放运行标记，不占用后续续期窗口
	if multithread {
		parent.endRun()
	}
	m.stats.timersExecuted.Add(1)

	m.reschedule(task, false)
}

// reschedule 把定时器重新放回时间轮。
//
// 派发成功时节点已从时间轮摘链，因此无论本轮是否真正执行都必须回轮，
// 否则定时器会永久丢失。skipped=true 表示本轮因重叠被跳过：
// 只推进 nextTime，不消耗执行次数（跳过不是一次真正的执行）。
func (m *TimerManager) reschedule(task timerTask, skipped bool) {
	// Tick 内部可能已经移除/复用该定时器，或系统已关闭，重新校验后再续期
	if m.isClosed.Load() || !task.isValid() {
		return
	}

	timer := task.timer
	if skipped {
		parent := timer.GetParent()
		if parent == nil {
			return
		}
		parent.advanceNextTime()
	} else {
		if !timer.HasNext() {
			return
		}
		if !task.isValid() {
			return
		}
	}

	if err := m.AddTimer(timer); err != nil {
		vars.Error("重新调度定时器失败: %v", err)
	}
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

	// 执行池必须在 wheelWG.Wait() 之后停止：cleanupWheel 阶段二仍会调用
	// executeTimer，此时 MultiThread 定时器可能还要投池。这里先在同一把锁下
	// 摘走池句柄（并阻止后续再创建），再于锁外做有界排空。
	m.executorMu.Lock()
	e := m.executor
	m.executor = nil
	m.executorMu.Unlock()

	if e != nil {
		e.Stop(DefaultExecutorDrainTimeout)
	}
}

// GetStats 返回性能统计信息
func (m *TimerManager) GetStats() TimerStats {
	return TimerStats{
		TimersAdded:          m.stats.timersAdded.Load(),
		TimersRemoved:        m.stats.timersRemoved.Load(),
		TimersExecuted:       m.stats.timersExecuted.Load(),
		TimersDropped:        m.stats.timersDropped.Load(),
		WheelMigrations:      m.stats.wheelMigrations.Load(),
		TimersSkipped:        m.stats.timersSkipped.Load(),
		TimersAsync:          m.stats.timersAsync.Load(),
		TimersInlineFallback: m.stats.timersInlineFallback.Load(),
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

// TimerManagerOption 用于定制 TimerManager 的行为（目前仅执行池 worker 数）。
type TimerManagerOption func(*TimerManager)

// WithExecutorWorkers 设置该管理器的执行池 worker 数。
//
// n <= 0 表示禁用执行池：MultiThread 定时器会退回内联串行执行（不丢任务），
// 适合需要严格控制协程数的场景。超过 MaxConcurrentWorkers 会被收敛到上限。
func WithExecutorWorkers(n int) TimerManagerOption {
	return func(m *TimerManager) {
		if n < 0 {
			n = 0
		}
		if n > MaxConcurrentWorkers {
			n = MaxConcurrentWorkers
		}
		m.executorWorkers = n
	}
}

// NewTimerManager 创建新的定时器管理器
func NewTimerManager() *TimerManager {
	return NewTimerManagerWithOptions()
}

// NewTimerManagerWithOptions 创建定时器管理器并应用选项。
//
// 默认 worker 数取门面配置（SetDefaultExecutorWorkers，初始为 DefaultConcurrentWorkers），
// 执行池仍然是懒创建的：没有 MultiThread 定时器就不会新增任何协程。
func NewTimerManagerWithOptions(opts ...TimerManagerOption) *TimerManager {
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
		closeChan:       make(chan struct{}),
		wheels:          make([]*TimerWheel, len(wheelConfigs)),
		executorWorkers: currentDefaultExecutorWorkers(),
	}

	for _, opt := range opts {
		if opt != nil {
			opt(mgr)
		}
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

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 加锁后再次校验：并发 Remove 可能刚好发生在这之前
	if !task.isValid() {
		return
	}

	task.timer.GetParent().wheel.Store(wheel)
	wheel.tickWheel.Add(node)
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
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	currentTime := util.CurrentMS()
	ch := currentTimerChannel()
	var dropped int64

	wheel.wheelLock.Lock()
	wheel.tickWheel.Range(func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil {
			return true
		}
		// 已被迁移到别的轮：归目标轮处理
		if parent.wheel.Load() != wheel {
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
			select {
			case ch <- task:
				node.GetNode().Remove()
				parent.wheel.Store(nil)
				wheel.timerCount.Add(-1)
			default:
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
		select {
		case m.wheels[newType].addTimerChan <- task:
			// 迁移成功，handleTimerAdd 会重新设置 wheel 并自增目标轮计数
			node.GetNode().Remove()
			parent.wheel.Store(nil)
			wheel.timerCount.Add(-1)
			m.stats.wheelMigrations.Add(1)
		default:
			// 目标时间轮通道已满，保持在当前时间轮等下一个 tick
			dropped++
		}
		return true
	})
	wheel.wheelLock.Unlock()

	// 告警必须在解锁之后：vars 异步日志缓冲满时会阻塞最长 5 秒
	if dropped > 0 {
		m.stats.timersDropped.Add(dropped)
		m.notifyBackpressure(wheelType, dropped)
	}
}

// cleanupWheel 清理时间轮
//
// 关键：业务回调（Tick）必须在完全释放 wheelLock 之后执行。
// Tick 内部调用 Remove / AddTimer 会再次申请同一把 wheelLock，
// 若持锁执行会立即自锁，导致 TimeStop 永久挂起（进程停不下来）。
func (m *TimerManager) cleanupWheel(wheel *TimerWheel) {
	// 阶段一：持锁摘链，收集已过期的定时器
	var expired []timerTask

	wheel.wheelLock.Lock()
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
	wheel.wheelLock.Unlock()

	// 阶段二：无锁执行（此时 Tick 内 Remove/AddTimer 都不会再触碰 wheelLock）
	for _, task := range expired {
		m.executeTimer(task)
	}
}
