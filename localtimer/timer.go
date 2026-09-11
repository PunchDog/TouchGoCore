package localtimer

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"

	"touchgocore/list"
	"touchgocore/syncmap"
	"touchgocore/util"
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
	pool *syncmap.Map[string, *sync.Pool] // map[reflect.Type]*sync.Pool
}

// Get 从池中获取定时器
func (p *TimerPool) Get(cls TimerInterface) TimerInterface {
	if p.pool == nil {
		p.pool = syncmap.NewMap[string, *sync.Pool]()
	}
	tpname, _ := util.GetClassName(cls)
	tp := reflect.TypeOf(cls).Elem()
	if pool, ok := p.pool.Load(tpname); ok {
		return pool.Get().(TimerInterface)
	}

	newPool := &sync.Pool{
		New: func() interface{} {
			return reflect.New(tp).Interface().(TimerInterface)
		},
	}
	p.pool.Store(tpname, newPool)
	return newPool.Get().(TimerInterface)
}

// Put 将定时器返回到池中
func (p *TimerPool) Put(cls TimerInterface) {
	if cls == nil {
		return
	}
	tpname, _ := util.GetClassName(cls)
	if pool, ok := p.pool.Load(tpname); ok {
		pool.Put(cls)
	}
}

var timerPool = &TimerPool{}

// Timer 表示基础定时器结构
type Timer struct {
	list.Node

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
	// t.uid = 0
	t.nextTime = util.CurrentMS() + interval
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

	t.Node.Remove() // 从链表中移除

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

	t.nextTime = util.CurrentMS() + t.interval
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
		interval = t.nextTime - util.CurrentMS()
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
