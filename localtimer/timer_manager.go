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
type timerTask struct {
	timer TimerInterface
	gen   uint64
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
}

// TimerStats 保存性能统计信息（快照值，非实时引用）
type TimerStats struct {
	TimersAdded     int64 // 已添加定时器数量
	TimersRemoved   int64 // 已移除定时器数量
	TimersExecuted  int64 // 已执行定时器数量
	WheelMigrations int64 // 时间轮迁移次数
}

// timerStatsCounters 管理器内部的原子计数器
type timerStatsCounters struct {
	timersAdded     atomic.Int64
	timersRemoved   atomic.Int64
	timersExecuted  atomic.Int64
	wheelMigrations atomic.Int64
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
	parent.mgr.Store(m)

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

	// 检查通道是否接近满
	if float64(chanLen) >= float64(chanCap)*0.9 {
		vars.Warning("定时器通道背压过高: 类型=%s, len=%d, cap=%d", wheelType.String(), chanLen, chanCap)
	}

	// 代次必须在 RemoveFromManager 之后读取，保证与入队后的定时器一致
	task := timerTask{timer: timer, gen: parent.gen.Load()}

	select {
	case wheel.addTimerChan <- task:
		m.stats.timersAdded.Add(1)
		wheel.timerCount.Add(1)
		return nil
	default:
		// 通道满时，尝试阻塞发送（带超时）
		select {
		case wheel.addTimerChan <- task:
			m.stats.timersAdded.Add(1)
			wheel.timerCount.Add(1)
			return nil
		case <-time.After(time.Millisecond * 100):
			// 超时后异步执行：绝不能在持有 wheelLock 的情况下执行业务回调
			go m.executeTimer(task)
			vars.Warning("定时器通道已满，异步执行定时器")
			return nil
		}
	}
}

// executeTimer 执行定时器并在需要时重新调度。
//
// 调用前提：不得持有任何 wheelLock。
// Tick 内部允许调用 Remove / AddTimer（业务最常见的写法），
// 因此本函数必须在完全无锁的上下文中运行，否则会自锁死锁。
func (m *TimerManager) executeTimer(task timerTask) {
	if !task.isValid() {
		return // 已被移除或被复用的过期调度项，直接丢弃
	}

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
		return
	}
	if !task.isValid() {
		return
	}
	if err := m.AddTimer(task.timer); err != nil {
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
}

// GetStats 返回性能统计信息
func (m *TimerManager) GetStats() TimerStats {
	return TimerStats{
		TimersAdded:     m.stats.timersAdded.Load(),
		TimersRemoved:   m.stats.timersRemoved.Load(),
		TimersExecuted:  m.stats.timersExecuted.Load(),
		WheelMigrations: m.stats.wheelMigrations.Load(),
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

// 全局状态（定时器系统单例相关）
var (
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	defaultTimerManager atomic.Pointer[TimerManager]
	timerChannel        chan timerTask
	timerChannelMu      sync.Mutex
	timerChannelReady   bool
	closech             chan any
	tickWG              sync.WaitGroup
	timerRunCtx         atomic.Pointer[context.Context]
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
}

// processWheelTick 处理时间轮滴答事件
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := util.CurrentMS()
	ch := currentTimerChannel()

	wheel.tickWheel.Range(func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil || !parent.IsActive() {
			return true
		}
		// 已被迁移或摘除的节点不再处理
		if parent.wheel.Load() != wheel {
			return true
		}

		if parent.nextTime.Load() <= currentTime {
			// 时间到达，执行并移除
			node.GetNode().Remove()
			parent.wheel.Store(nil)
			wheel.timerCount.Add(-1)

			task := timerTask{timer: timer, gen: parent.gen.Load()}
			select {
			case ch <- task:
			default:
				// 通道已满（或已停止）：异步执行。
				// 当前持有 wheelLock，同步调用 Tick（内部可能再次 AddTimer 加锁）会死锁。
				go m.executeTimer(task)
			}
		} else {
			// 检查是否需要迁移到更精确的时间轮
			remaining := parent.nextTime.Load() - currentTime - TimerMigrationOffset
			newType := parent.calculateType(remaining)

			if newType != wheelType && int(newType) < len(m.wheels) {
				node.GetNode().Remove()
				parent.wheel.Store(nil)
				wheel.timerCount.Add(-1)
				m.stats.wheelMigrations.Add(1)

				task := timerTask{timer: timer, gen: parent.gen.Load()}
				select {
				case m.wheels[newType].addTimerChan <- task:
					// 迁移成功，handleTimerAdd 会重新设置 wheel
				default:
					// 目标时间轮通道已满，保持在当前时间轮。
					// list 会撤销本次的待删除标记，确保回加的节点不会被延迟删除误删。
					wheel.tickWheel.Add(node)
					wheel.timerCount.Add(1)
					parent.wheel.Store(wheel)
				}
			}
		}
		return true
	})
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
		if parent != nil && parent.IsActive() && parent.nextTime.Load() <= currentTime {
			node.GetNode().Remove()
			parent.wheel.Store(nil)
			expired = append(expired, timerTask{timer: timer, gen: parent.gen.Load()})
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
