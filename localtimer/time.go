package localtimer

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/list"
	"touchgocore/util"
	"touchgocore/vars"
)

// 错误定义
var (
	ErrTimerInvalidInterval = errors.New("invalid timer interval")
	ErrTimerInvalidType     = errors.New("invalid timer type")
	ErrTimerManagerClosed   = errors.New("timer manager is closed")
	ErrTimerChannelFull     = errors.New("timer channel is full")
	ErrTimerSystemNotReady  = errors.New("timer system not initialized")
	ErrTimerNilParent       = errors.New("timer parent is nil")
)

// 常量定义
const (
	MaxTimerChannelNum    int64 = 100000
	MaxAddTimerChannelNum int64 = 10000
	InfiniteCount         int64 = -1
	CountCorrectionValue  int64 = -999999
	TimerMigrationOffset  int64 = 5 // 提前检测迁移的时间偏移(毫秒)
	DefaultWheelCount     int   = 5 // 默认时间轮数量
)

// TimerType 表示定时器精度级别
type TimerType int8

const (
	TimerTypeMillisecond TimerType = iota // 毫秒精度
	TimerTypeSecond                       // 秒精度
	TimerTypeMinute                       // 分钟精度
	TimerTypeTenMinute                    // 10分钟精度
	TimerTypeHour                         // 小时精度
)

// String 返回 TimerType 的字符串表示
func (t TimerType) String() string {
	switch t {
	case TimerTypeMillisecond:
		return "millisecond"
	case TimerTypeSecond:
		return "second"
	case TimerTypeMinute:
		return "minute"
	case TimerTypeTenMinute:
		return "ten-minute"
	case TimerTypeHour:
		return "hour"
	default:
		return "unknown"
	}
}

// TimerInterface 定义定时器实现的接口
type TimerInterface interface {
	// Tick 执行定时器任务
	Tick()
	// Remove 从管理器中移除定时器
	Remove()
	// GetUID 返回唯一标识符
	GetUID() int64
	// GetParent 返回父定时器指针
	GetParent() *Timer
	// HasNext 检查是否有下一次执行
	HasNext() bool
	// SetSelf 设置自身引用
	SetSelf(TimerInterface)
	// RemoveFromManager 从管理器中移除
	RemoveFromManager(cleanPool bool)
	// IsActive 检查定时器是否活跃
	IsActive() bool
}

// TimerPool 为定时器提供类型安全的对象池管理
type TimerPool struct {
	pool sync.Map // map[reflect.Type]*sync.Pool
}

// Get 从池中获取定时器
func (p *TimerPool) Get(cls TimerInterface) TimerInterface {
	tp := reflect.TypeOf(cls).Elem()
	if pool, ok := p.pool.Load(tp); ok {
		return pool.(*sync.Pool).Get().(TimerInterface)
	}

	newPool := &sync.Pool{
		New: func() interface{} {
			return reflect.New(tp).Interface().(TimerInterface)
		},
	}
	p.pool.Store(tp, newPool)
	return newPool.Get().(TimerInterface)
}

// Put 将定时器返回到池中
func (p *TimerPool) Put(cls TimerInterface) {
	if cls == nil {
		return
	}
	tp := reflect.TypeOf(cls).Elem()
	if pool, ok := p.pool.Load(tp); ok {
		pool.(*sync.Pool).Put(cls)
	}
}

var timerPool = &TimerPool{}

// Timer 表示基础定时器结构
type Timer struct {
	list.ListNode

	// 定时器元数据
	uid      int64 // 唯一标识符
	nextTime int64 // 下次执行时间（毫秒）
	interval int64 // 执行间隔（毫秒）
	count    int64 // 剩余执行次数

	// 管理组件
	mgr   *TimerManager  // 父管理器
	wheel *TimerWheel    // 父时间轮
	self  TimerInterface // 自身接口引用

	// 状态标志
	isActive atomic.Bool // 是否活跃
}

// Init 初始化定时器
func (t *Timer) Init(interval, count int64, self TimerInterface) error {
	if interval <= 0 {
		return ErrTimerInvalidInterval
	}

	t.uid = 0
	t.nextTime = time.Now().UTC().UnixMilli() + interval
	t.interval = interval
	t.count = count
	if count == InfiniteCount {
		t.count = CountCorrectionValue
	}
	t.self = self
	t.isActive.Store(true)

	return nil
}

// SetSelf 设置自身引用
func (t *Timer) SetSelf(self TimerInterface) {
	t.self = self
}

// RemoveFromManager 从管理器中移除
func (t *Timer) RemoveFromManager(cleanPool bool) {
	if !t.isActive.CompareAndSwap(true, false) {
		return // 已经移除
	}

	if t.wheel != nil {
		t.wheel.wheelLock.Lock()
		defer t.wheel.wheelLock.Unlock()
	}

	t.ListNode.Remove() // 从链表中移除

	if cleanPool && t.self != nil {
		timerPool.Put(t.self)
	}
}

// Remove 公共移除方法
func (t *Timer) Remove() {
	t.RemoveFromManager(true)
}

// HasNext 检查是否有下一次执行
func (t *Timer) HasNext() bool {
	if !t.isActive.Load() {
		return false
	}

	if t.count != CountCorrectionValue {
		t.count--
		if t.count <= 0 {
			return false
		}
	}

	t.nextTime = time.Now().UTC().UnixMilli() + t.interval
	return true
}

// GetUID 返回唯一标识符
func (t *Timer) GetUID() int64 {
	return t.uid
}

// GetParent 返回父定时器指针
func (t *Timer) GetParent() *Timer {
	return t
}

// GetType 返回定时器类型
func (t *Timer) GetType() TimerType {
	return t.calculateType(-CountCorrectionValue)
}

// IsActive 检查定时器是否活跃
func (t *Timer) IsActive() bool {
	return t.isActive.Load()
}

// calculateType 根据间隔计算定时器类型
func (t *Timer) calculateType(interval int64) TimerType {
	if interval == -CountCorrectionValue {
		interval = t.nextTime - time.Now().UTC().UnixMilli()
	}

	switch {
	case interval < util.MILLISECONDS_OF_SECOND:
		return TimerTypeMillisecond
	case interval < util.MILLISECONDS_OF_MINUTE:
		return TimerTypeSecond
	case interval < util.MILLISECONDS_OF_10_MINUTE:
		return TimerTypeMinute
	case interval < util.MILLISECONDS_OF_HOUR:
		return TimerTypeTenMinute
	default:
		return TimerTypeHour
	}
}

// SetCount 设置执行次数
func (t *Timer) SetCount(count int64) {
	if !t.isActive.Load() {
		return
	}
	t.count = count
	if count == InfiniteCount {
		t.count = CountCorrectionValue
	}
}

// GetInterval 返回定时器间隔
func (t *Timer) GetInterval() int64 {
	return t.interval
}

// GetRemainingCount 返回剩余执行次数
func (t *Timer) GetRemainingCount() int64 {
	if t.count == CountCorrectionValue {
		return InfiniteCount
	}
	return t.count
}

// NewTimer 创建新的定时器实例
func NewTimer(interval, count int64, cls TimerInterface) (TimerInterface, error) {
	if cls == nil {
		return nil, ErrTimerInvalidType
	}

	if interval <= 0 {
		return nil, ErrTimerInvalidInterval
	}

	timer := timerPool.Get(cls)
	parent := timer.GetParent()
	if parent == nil {
		timerPool.Put(timer)
		return nil, ErrTimerNilParent
	}

	if err := parent.Init(interval, count, timer); err != nil {
		timerPool.Put(timer)
		return nil, err
	}

	return timer, nil
}

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
	closeChan   chan struct{} // 关闭信号
	wheels      []*TimerWheel // 时间轮数组
	maxTimerUID atomic.Int64  // 最大定时器ID
	isClosed    atomic.Bool   // 是否关闭
	stats       TimerStats    // 性能统计
}

// TimerStats 保存性能统计信息
type TimerStats struct {
	TimersAdded     atomic.Int64 // 已添加定时器数量
	TimersRemoved   atomic.Int64 // 已移除定时器数量
	TimersExecuted  atomic.Int64 // 已执行定时器数量
	WheelMigrations atomic.Int64 // 时间轮迁移次数
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

	// 分配唯一ID
	parent.uid = m.maxTimerUID.Add(1)
	parent.mgr = m

	// 清理现有定时器
	timer.RemoveFromManager(false)

	// 选择合适的时间轮
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType
	}

	select {
	case m.wheels[wheelType].addTimerChan <- timer:
		m.stats.TimersAdded.Add(1)
		m.wheels[wheelType].timerCount.Add(1)
		return nil
	default:
		return ErrTimerChannelFull
	}
}

// Close 优雅地关闭定时器管理器
func (m *TimerManager) Close() {
	if !m.isClosed.CompareAndSwap(false, true) {
		return // 已经关闭
	}

	// 发送关闭信号
	close(m.closeChan)

	// 停止所有时间轮
	for _, wheel := range m.wheels {
		wheel.isRunning.Store(false)
		close(wheel.addTimerChan)
	}
}

// GetStats 返回性能统计信息
func (m *TimerManager) GetStats() TimerStats {
	return m.stats
}

// GetTimerCount 返回所有时间轮中的定时器总数
func (m *TimerManager) GetTimerCount() int64 {
	var total int64
	for _, wheel := range m.wheels {
		total += wheel.timerCount.Load()
	}
	return total
}

var (
	timerManagerMap     sync.Map // map[*TimerManager]bool
	defaultTimerManager *TimerManager
	timerChannel        chan TimerInterface
	managerInitOnce     sync.Once
	closech             chan any
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
		go mgr.runWheel(wheel, TimerType(i))
	}

	timerManagerMap.Store(mgr, true)
	return mgr
}

// runWheel 运行时间轮循环
func (m *TimerManager) runWheel(wheel *TimerWheel, wheelType TimerType) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("时间轮运行发生panic错误: %v, 类型: %s", err, wheelType.String())
		}
	}()

	ticker := time.NewTicker(time.Duration(wheel.wheelConfig) * time.Millisecond)
	defer ticker.Stop()

	vars.Info("启动时间轮: %s (精度: %dms)", wheelType.String(), wheel.wheelConfig)

	for wheel.isRunning.Load() {
		select {
		case <-m.closeChan:
			wheel.isRunning.Store(false)
			m.cleanupWheel(wheel)
			vars.Info("时间轮停止: %s", wheelType.String())
			return

		case timer, ok := <-wheel.addTimerChan:
			if !ok {
				// 通道已关闭
				continue
			}
			m.handleTimerAdd(wheel, timer)

		case <-ticker.C:
			m.processWheelTick(wheel, wheelType)
		}
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

	currentTime := time.Now().UTC().UnixMilli()

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
			m.stats.TimersExecuted.Add(1)

			select {
			case timerChannel <- timer:
			default:
				// 通道已满，直接执行
				timer.Tick()
				if timer.HasNext() {
					if err := m.AddTimer(timer); err != nil {
						vars.Error("重新调度定时器失败: %v", err)
					}
				}
			}
		} else {
			// 检查是否需要迁移到更精确的时间轮
			remaining := parent.nextTime - currentTime - TimerMigrationOffset
			newType := parent.calculateType(remaining)

			if newType != wheelType && int(newType) < len(m.wheels) {
				node.GetNode().Remove()
				wheel.timerCount.Add(-1)
				parent.wheel = nil
				m.stats.WheelMigrations.Add(1)

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

	currentTime := time.Now().UTC().UnixMilli()

	// 执行所有已过期的定时器
	wheel.tickWheel.Range(func(node list.INode) bool {
		timer := node.(TimerInterface)
		parent := timer.GetParent()
		if parent != nil && parent.nextTime <= currentTime {
			timer.Tick()
			m.stats.TimersExecuted.Add(1)
		}
		return true
	})

	// 清空时间轮
	removedCount := wheel.timerCount.Load()
	wheel.tickWheel.Clear()
	wheel.timerCount.Store(0)
	m.stats.TimersRemoved.Add(removedCount)
}

// Run 启动定时器系统
func Run() {
	vars.Info("启动计时器系统")
	defaultTimerManager = NewTimerManager()
	closech = make(chan any)
	go TimeTick()
	vars.Info("计时器系统启动完成")
}

// TimeStop 停止定时器系统
func TimeStop() {
	vars.Info("正在停止计时器系统...")
	close(closech)

	// 关闭所有定时器管理器
	timerManagerMap.Range(func(key, value interface{}) bool {
		if mgr, ok := key.(*TimerManager); ok {
			vars.Info("关闭定时器管理器，当前定时器数量: %d", mgr.GetTimerCount())
			mgr.Close()
		}
		// timerManagerMap.Delete(key)
		return true
	})
	timerManagerMap.Clear()

	// 关闭定时器通道
	if timerChannel != nil {
		close(timerChannel)
		timerChannel = nil
	}

	defaultTimerManager = nil
	managerInitOnce = sync.Once{}
	vars.Info("计时器系统已停止")
}

// AddTimer 向默认管理器添加定时器
func AddTimer(timer TimerInterface) error {
	if defaultTimerManager == nil {
		return ErrTimerSystemNotReady
	}

	if timer == nil || timer.GetParent() == nil {
		return ErrTimerNilParent
	}

	return defaultTimerManager.AddTimer(timer)
}

// TimeTick 处理定时器滴答
func TimeTick() {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("定时器滴答处理发生panic错误: %v", err)
		}
	}()

	for {
		select {
		case timer, ok := <-timerChannel:
			if !ok {
				// 通道已关闭
				return
			}

			timer.Tick()
			if timer.HasNext() {
				if err := AddTimer(timer); err != nil {
					vars.Error("重新调度定时器失败: %v", err)
				}
			}
		case <-closech:
			return
		}
	}
}

// GetDefaultManager 返回默认定时器管理器
func GetDefaultManager() *TimerManager {
	return defaultTimerManager
}

// IsSystemRunning 检查定时器系统是否正在运行
func IsSystemRunning() bool {
	return defaultTimerManager != nil && !defaultTimerManager.isClosed.Load()
}

// GetSystemStats 返回定时器系统统计信息
func GetSystemStats() (totalTimers int64, stats TimerStats) {
	if defaultTimerManager != nil {
		totalTimers = defaultTimerManager.GetTimerCount()
		stats = defaultTimerManager.GetStats()
	}
	return
}

// // CreatePeriodicTimer 创建具有指定间隔的周期性定时器
// func CreatePeriodicTimer(interval time.Duration, callback func()) (TimerInterface, error) {
// 	type SimpleTimer struct {
// 		Timer
// 		callback func()
// 	}

// 	// 为 SimpleTimer 实现 Tick 方法
// 	st := &SimpleTimer{callback: callback}
// 	st.SetSelf(st)

// 	timer, err := NewTimer(interval.Milliseconds(), InfiniteCount, st)
// 	if err != nil {
// 		return nil, err
// 	}

// 	return timer, nil
// }

// // Tick 方法用于 SimpleTimer
// func (st *SimpleTimer) Tick() {
// 	if st.callback != nil {
// 		st.callback()
// 	}
// }

// // CreateOneShotTimer 创建一次性定时器
// func CreateOneShotTimer(delay time.Duration, callback func()) (TimerInterface, error) {
// 	type OneShotTimer struct {
// 		Timer
// 		callback func()
// 	}

// 	// 为 OneShotTimer 实现 Tick 方法
// 	ost := &OneShotTimer{callback: callback}
// 	ost.SetSelf(ost)

// 	timer, err := NewTimer(delay.Milliseconds(), 1, ost)
// 	if err != nil {
// 		return nil, err
// 	}

// 	return timer, nil
// }

// // Tick 方法用于 OneShotTimer
// func (ost *OneShotTimer) Tick() {
// 	if ost.callback != nil {
// 		ost.callback()
// 	}
// }
