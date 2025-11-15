package util

import (
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/vars"
)

// 错误定义
var (
	ErrTimerInvalidInterval = errors.New("invalid timer interval")
	ErrTimerInvalidType     = errors.New("invalid timer type")
	ErrTimerManagerClosed   = errors.New("timer manager is closed")
	ErrTimerChannelFull     = errors.New("timer channel is full")
)

// 常量定义
const (
	MaxTimerChannelNum    int64 = 100000
	MaxAddTimerChannelNum int64 = 10000
	InfiniteCount         int64 = -1
	CountCorrectionValue  int64 = -999999
)

// TimerType 定时器类型枚举
type TimerType int8

const (
	TimerTypeMillisecond TimerType = iota // 毫秒级
	TimerTypeSecond                       // 秒级
	TimerTypeMinute                       // 分级
	TimerTypeTenMinute                    // 10分钟级
	TimerTypeHour                         // 小时级
)

// TimerInterface 定时器接口
type TimerInterface interface {
	// Tick 执行定时器任务
	Tick()
	// Remove 从管理器中移除定时器
	Remove()
	// GetUID 获取唯一标识
	GetUID() int64
	// GetParent 获取父类指针
	GetParent() *Timer
	// hasNext 检查是否还有下一次执行
	hasNext() bool
	// setSelf 设置自身引用
	setSelf(TimerInterface)
	// removeFromManager 从管理器中移除
	removeFromManager(cleanPool bool)
}

// TimerPool 定时器对象池
// 使用泛型技术提供类型安全的对象池管理
type TimerPool struct {
	pool sync.Map // map[reflect.Type]*sync.Pool
}

// Get 从对象池获取定时器
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

// Put 将定时器放回对象池
func (p *TimerPool) Put(cls TimerInterface) {
	tp := reflect.TypeOf(cls).Elem()
	if pool, ok := p.pool.Load(tp); ok {
		pool.(*sync.Pool).Put(cls)
	}
}

var timerPool = &TimerPool{}

// Timer 定时器基础结构体
type Timer struct {
	ListNode

	// 定时器元数据
	uid      int64 // 唯一标识
	nextTime int64 // 下次执行时间(毫秒)
	interval int64 // 执行间隔(毫秒)
	count    int64 // 剩余执行次数

	// 管理组件
	mgr   *TimerManager  // 所属管理器
	wheel *TimerWheel    // 所属时间轮
	self  TimerInterface // 自身接口引用

	// 状态标记
	isActive atomic.Bool // 是否活跃
}

// init 初始化定时器
func (t *Timer) init(interval, count int64, self TimerInterface) error {
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

// setSelf 设置自身引用
func (t *Timer) setSelf(self TimerInterface) {
	t.self = self
}

// removeFromManager 从管理器中移除
func (t *Timer) removeFromManager(cleanPool bool) {
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

// Remove 公开的移除方法
func (t *Timer) Remove() {
	t.removeFromManager(true)
}

// hasNext 检查是否还有下一次执行
func (t *Timer) hasNext() bool {
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

// GetUID 获取唯一标识
func (t *Timer) GetUID() int64 {
	return t.uid
}

// GetParent 获取父类指针
func (t *Timer) GetParent() *Timer {
	return t
}

// GetType 获取定时器类型
func (t *Timer) GetType() TimerType {
	return t.calculateType(-CountCorrectionValue)
}

// calculateType 计算定时器类型
func (t *Timer) calculateType(interval int64) TimerType {
	if interval == -CountCorrectionValue {
		interval = t.nextTime - time.Now().UTC().UnixMilli()
	}

	switch {
	case interval < MILLISECONDS_OF_SECOND:
		return TimerTypeMillisecond
	case interval < MILLISECONDS_OF_MINUTE:
		return TimerTypeSecond
	case interval < MILLISECONDS_OF_10_MINUTE:
		return TimerTypeMinute
	case interval < MILLISECONDS_OF_HOUR:
		return TimerTypeTenMinute
	default:
		return TimerTypeHour
	}
}

// SetCount 设置执行次数
func (t *Timer) SetCount(count int64) {
	t.count = count
}

// NewTimer 创建新定时器
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
		return nil, errors.New("timer parent is nil")
	}

	if err := parent.init(interval, count, timer); err != nil {
		timerPool.Put(timer)
		return nil, err
	}

	return timer, nil
}

// TimerWheel 时间轮结构
type TimerWheel struct {
	wheelConfig  int64               // 时间轮精度(毫秒)
	tickWheel    *List               // 定时器链表
	wheelLock    sync.Mutex          // 时间轮锁
	addTimerChan chan TimerInterface // 添加定时器通道
	isRunning    atomic.Bool         // 是否运行中
}

// TimerManager 定时器管理器
type TimerManager struct {
	closeChan   chan struct{} // 关闭信号
	wheels      []*TimerWheel // 时间轮数组
	maxTimerUID atomic.Int64  // 最大定时器ID
	isClosed    atomic.Bool   // 是否已关闭
}

// AddTimer 添加定时器到管理器
func (m *TimerManager) AddTimer(timer TimerInterface) error {
	if m.isClosed.Load() {
		return ErrTimerManagerClosed
	}

	parent := timer.GetParent()
	if parent == nil {
		return errors.New("timer parent is nil")
	}

	// 分配唯一ID
	parent.uid = m.maxTimerUID.Add(1)
	parent.mgr = m

	// 清理现有定时器
	timer.removeFromManager(false)

	// 选择合适的时间轮
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType
	}

	select {
	case m.wheels[wheelType].addTimerChan <- timer:
		return nil
	default:
		return ErrTimerChannelFull
	}
}

// Close 关闭定时器管理器
func (m *TimerManager) Close() {
	if !m.isClosed.CompareAndSwap(false, true) {
		return // 已经关闭
	}

	close(m.closeChan)

	// 等待所有时间轮停止
	for _, wheel := range m.wheels {
		wheel.isRunning.Store(false)
		close(wheel.addTimerChan)
	}
}

var (
	timerManagerMap     sync.Map // map[*TimerManager]bool
	defaultTimerManager *TimerManager
	timerChannel        chan TimerInterface
	managerInitOnce     sync.Once
)

// NewTimerManager 创建新的定时器管理器
func NewTimerManager() *TimerManager {
	managerInitOnce.Do(func() {
		timerChannel = make(chan TimerInterface, MaxTimerChannelNum)
	})

	// 时间轮配置：毫秒/秒/分钟/10分钟/小时
	wheelConfigs := []int64{
		1, // 毫秒级
		MILLISECONDS_OF_SECOND,
		MILLISECONDS_OF_MINUTE,
		MILLISECONDS_OF_10_MINUTE,
		MILLISECONDS_OF_HOUR,
	}

	mgr := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, len(wheelConfigs)),
	}

	// 初始化时间轮
	for i, config := range wheelConfigs {
		wheel := &TimerWheel{
			wheelConfig:  config,
			tickWheel:    NewList(),
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

// runWheel 运行时间轮
func (m *TimerManager) runWheel(wheel *TimerWheel, wheelType TimerType) {
	ticker := time.NewTicker(time.Duration(wheel.wheelConfig) * time.Millisecond)
	defer ticker.Stop()

	for wheel.isRunning.Load() {
		select {
		case <-m.closeChan:
			wheel.isRunning.Store(false)
			m.cleanupWheel(wheel)
			return

		case timer := <-wheel.addTimerChan:
			m.handleTimerAdd(wheel, timer)

		case <-ticker.C:
			m.processWheelTick(wheel, wheelType)
		}
	}
}

// handleTimerAdd 处理定时器添加
func (m *TimerManager) handleTimerAdd(wheel *TimerWheel, timer TimerInterface) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	parent := timer.GetParent()
	if parent == nil {
		return
	}

	parent.wheel = wheel
	wheel.tickWheel.Add(timer.(INode))
}

// processWheelTick 处理时间轮tick
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := time.Now().UTC().UnixMilli()

	wheel.tickWheel.Range(func(node INode) bool {
		timer := node.(TimerInterface)
		parent := timer.GetParent()

		if parent.nextTime <= currentTime {
			// 时间到了，执行并移除
			node.GetNode().Remove()
			select {
			case timerChannel <- timer:
			default:
				// 通道满，直接执行
				timer.Tick()
				if timer.hasNext() {
					m.AddTimer(timer)
				}
			}
		} else {
			// 检查是否需要迁移到更精确的时间轮
			remaining := parent.nextTime - currentTime - 5 // 提前5ms检测
			newType := parent.calculateType(remaining)

			if newType != wheelType && int(newType) < len(m.wheels) {
				node.GetNode().Remove()
				parent.wheel = nil
				m.wheels[newType].addTimerChan <- timer
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

	// 执行所有到期的定时器
	wheel.tickWheel.Range(func(node INode) bool {
		timer := node.(TimerInterface)
		if timer.GetParent().nextTime <= currentTime {
			timer.Tick()
		}
		return true
	})

	wheel.tickWheel.Clear()
}

// TimeRun 启动定时器系统
func TimeRun() {
	vars.Info("启动计时器系统")
	defaultTimerManager = NewTimerManager()
}

// TimeStop 停止定时器系统
func TimeStop() {
	timerManagerMap.Range(func(key, value interface{}) bool {
		if mgr, ok := key.(*TimerManager); ok {
			mgr.Close()
		}
		timerManagerMap.Delete(key)
		return true
	})

	if timerChannel != nil {
		close(timerChannel)
		timerChannel = nil
	}

	defaultTimerManager = nil
	managerInitOnce = sync.Once{}
}

// AddTimer 添加定时器到默认管理器
func AddTimer(timer TimerInterface) error {
	if defaultTimerManager == nil {
		return errors.New("timer system not initialized")
	}
	return defaultTimerManager.AddTimer(timer)
}

// TimeTick 处理定时器tick
func TimeTick() chan TimerInterface {
	select {
	case timer := <-timerChannel:
		timer.Tick()
		if timer.hasNext() {
			AddTimer(timer)
		}
	default:
		// 没有定时器需要处理
	}
	return nil
}

// 时间工具函数部分保持不变（已优化命名和错误处理）

// CurrentMillisecond 返回当前毫秒时间戳
func CurrentMillisecond() int64 {
	return time.Now().UnixMilli()
}

// CurrentSecond 返回当前秒时间戳
func CurrentSecond() int64 {
	return time.Now().Unix()
}

// MillisecondToTimeString 毫秒转时间字符串
func MillisecondToTimeString(ms int64) string {
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

// SecondToTimeString 秒转时间字符串
func SecondToTimeString(sec int64) string {
	return time.Unix(sec, 0).Format("2006-01-02 15:04:05")
}

// TimeToMidnight 获取时间的午夜时间
func TimeToMidnight(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

// MillisecondToMidnight 毫秒时间戳转午夜时间
func MillisecondToMidnight(ms int64) time.Time {
	return TimeToMidnight(time.UnixMilli(ms))
}

// StringToUnixTime 字符串转时间戳
func StringToUnixTime(value string) (int64, error) {
	re := regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2}) (\d{2}):(\d{2}):(\d{2})$`)
	matches := re.FindStringSubmatch(value)
	if matches == nil || len(matches) != 7 {
		return 0, errors.New("invalid time format, expected: 2006-01-02 15:04:05")
	}

	year, _ := strconv.Atoi(matches[1])
	month, _ := strconv.Atoi(matches[2])
	day, _ := strconv.Atoi(matches[3])
	hour, _ := strconv.Atoi(matches[4])
	min, _ := strconv.Atoi(matches[5])
	sec, _ := strconv.Atoi(matches[6])

	t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
	return t.UnixMilli(), nil
}

// NextMidnight 获取下一个午夜时间
func NextMidnight(ms int64) int64 {
	return TimeToMidnight(time.UnixMilli(ms)).Add(24 * time.Hour).UnixMilli()
}

// NextHour 获取下一个整点时间
func NextHour(ms int64) int64 {
	t := time.UnixMilli(ms)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location()).UnixMilli()
}

// IsSameWeek 判断是否在同一周
func IsSameWeek(ms1, ms2 int64) bool {
	if ms1 == 0 || ms2 == 0 {
		return false
	}
	y1, w1 := time.UnixMilli(ms1).ISOWeek()
	y2, w2 := time.UnixMilli(ms2).ISOWeek()
	return y1 == y2 && w1 == w2
}

// IsSameMonth 判断是否在同一月
func IsSameMonth(ms1, ms2 int64) bool {
	if ms1 == 0 || ms2 == 0 {
		return false
	}
	y1, m1, _ := time.UnixMilli(ms1).Date()
	y2, m2, _ := time.UnixMilli(ms2).Date()
	return y1 == y2 && m1 == m2
}

// IsSameDay 判断是否在同一天
func IsSameDay(ms1, ms2 int64) bool {
	t1 := time.UnixMilli(ms1)
	t2 := time.UnixMilli(ms2)
	return t1.Year() == t2.Year() && t1.Month() == t2.Month() && t1.Day() == t2.Day()
}
