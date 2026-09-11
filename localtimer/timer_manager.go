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

// TimerWheel 表示时间轮结构
type TimerWheel struct {
	wheelConfig  int64               // 时间轮精度（毫秒）
	tickWheel    *list.List          // 定时器链表
	wheelLock    sync.RWMutex        // 时间轮锁（读写优化）
	addTimerChan chan TimerInterface // 定时器添加通道
	isRunning    atomic.Bool         // 是否运行
	timerCount   atomic.Int64        // 定时器计数（用于监控）
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
	if parent.uid == 0 {
		parent.uid = m.maxTimerUID.Add(1)
	}
	parent.mgr = m

	// 清理现有定时器
	timer.RemoveFromManager(false)
	// 重新进入调度，恢复活跃状态：
	// RemoveFromManager 内部会 CAS 将 isActive 置为 false，
	// 若不恢复，handleTimerAdd 会因 !IsActive() 直接丢弃定时器，导致 Tick 永远不被调用
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

	select {
	case wheel.addTimerChan <- timer:
		m.stats.timersAdded.Add(1)
		wheel.timerCount.Add(1)
		return nil
	default:
		// 通道满时，尝试阻塞发送（带超时）
		select {
		case wheel.addTimerChan <- timer:
			m.stats.timersAdded.Add(1)
			wheel.timerCount.Add(1)
			return nil
		case <-time.After(time.Millisecond * 100):
			// 超时后尝试直接执行定时器
			go func() {
				if m.isClosed.Load() {
					return
				}
				timer.Tick()
				if timer.HasNext() && !m.isClosed.Load() {
					_ = m.AddTimer(timer)
				}
			}()
			vars.Warning("定时器通道已满，异步执行定时器")
			return nil
		}
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
	timerManagerMap     *syncmap.Map[*TimerManager, bool] // map[*TimerManager]bool
	defaultTimerManager *TimerManager
	timerChannel        chan TimerInterface
	managerInitOnce     sync.Once
	closech             chan any
	tickWG              sync.WaitGroup
	timerRunCtx         = context.Background()
)

// NewTimerManager 创建新的定时器管理器
func NewTimerManager() *TimerManager {
	managerInitOnce.Do(func() {
		timerChannel = make(chan TimerInterface, MaxTimerChannelNum)
	})

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
			addTimerChan: make(chan TimerInterface, MaxAddTimerChannelNum),
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

			case timer, ok := <-wheel.addTimerChan:
				if !ok {
					// 通道已关闭
					return
				}
				m.handleTimerAdd(wheel, timer)

			case <-ticker.C:
				m.processWheelTick(wheel, wheelType)
			}
		}()
	}
}

// handleTimerAdd 处理定时器添加到时间轮
func (m *TimerManager) handleTimerAdd(wheel *TimerWheel, timer TimerInterface) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	parent := timer.GetParent()
	if parent == nil || !parent.IsActive() {
		return
	}

	parent.wheel = wheel
	wheel.tickWheel.Add(timer.(list.INode))
}

// processWheelTick 处理时间轮滴答事件
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := util.CurrentMS()

	wheel.tickWheel.Range(func(node list.INode) bool {
		timer := node.(TimerInterface)
		parent := timer.GetParent()

		if parent == nil || !parent.IsActive() {
			return true
		}

		if parent.nextTime <= currentTime {
			// 时间到达，执行并移除
			node.GetNode().Remove()
			wheel.timerCount.Add(-1)
			m.stats.timersExecuted.Add(1)

			select {
			case timerChannel <- timer:
			default:
				// 通道已满，异步执行：当前持有 wheelLock，
				// 同步调用 Tick（内部可能再次 AddTimer 加锁）会导致死锁
				go func() {
					if m.isClosed.Load() {
						return
					}
					timer.Tick()
					if timer.HasNext() && !m.isClosed.Load() {
						if err := m.AddTimer(timer); err != nil {
							vars.Error("重新调度定时器失败: %v", err)
						}
					}
				}()
			}
		} else {
			// 检查是否需要迁移到更精确的时间轮
			remaining := parent.nextTime - currentTime - TimerMigrationOffset
			newType := parent.calculateType(remaining)

			if newType != wheelType && int(newType) < len(m.wheels) {
				node.GetNode().Remove()
				wheel.timerCount.Add(-1)
				parent.wheel = nil
				m.stats.wheelMigrations.Add(1)

				select {
				case m.wheels[newType].addTimerChan <- timer:
				default:
					// 如果目标时间轮通道已满，保持在当前时间轮
					wheel.tickWheel.Add(node)
					wheel.timerCount.Add(1)
				}
			}
		}
		return true
	})
}

// cleanupWheel 清理时间轮
func (m *TimerManager) cleanupWheel(wheel *TimerWheel) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := util.CurrentMS()

	// 执行所有已过期的定时器
	wheel.tickWheel.Range(func(node list.INode) bool {
		timer := node.(TimerInterface)
		parent := timer.GetParent()
		if parent != nil && parent.nextTime <= currentTime {
			timer.Tick()
			m.stats.timersExecuted.Add(1)
		}
		return true
	})

	// 清空时间轮
	removedCount := wheel.timerCount.Load()
	wheel.tickWheel.Clear()
	wheel.timerCount.Store(0)
	m.stats.timersRemoved.Add(removedCount)
}
