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
	once sync.Once
	pool *syncmap.Map[string, *sync.Pool] // map[reflect.Type]*sync.Pool
}

// Get 从池中获取定时器
func (p *TimerPool) Get(cls TimerInterface) TimerInterface {
	// once 保证并发首次调用只会初始化一次（原来的裸 nil 判断存在数据竞争）
	p.once.Do(func() {
		if p.pool == nil {
			p.pool = syncmap.NewMap[string, *sync.Pool]()
		}
	})
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
	// LoadOrStore 而非 Store：并发首次调用时只会保留一个池。
	// 各自 Store 会后写覆盖前写，被覆盖那个池里已归还的对象就此孤儿。
	pool, _ := p.pool.LoadOrStore(tpname, newPool)
	return pool.Get().(TimerInterface)
}

// Put 将定时器返回到池中
func (p *TimerPool) Put(cls TimerInterface) {
	if cls == nil || p.pool == nil {
		return
	}
	tpname, _ := util.GetClassName(cls)
	if pool, ok := p.pool.Load(tpname); ok {
		pool.Put(cls)
	}
}

var timerPool = &TimerPool{pool: syncmap.NewMap[string, *sync.Pool]()}

// Timer 表示基础定时器结构
// 说明：定时器会被多个协程同时访问（业务协程 Remove/AddTimer、时间轮协程调度、
// TimeTick 协程执行 Tick），因此所有状态字段一律使用原子类型，禁止裸读写。
type Timer struct {
	list.Node

	// 定时器元数据
	uid      atomic.Int64 // 唯一标识符
	nextTime atomic.Int64 // 下次执行时间（毫秒）
	interval atomic.Int64 // 执行间隔（毫秒）
	count    atomic.Int64 // 剩余执行次数

	// 管理组件
	wheel atomic.Pointer[TimerWheel] // 父时间轮
	self  atomic.Pointer[TimerInterface]

	// 状态标志
	isActive atomic.Bool   // 是否活跃
	gen      atomic.Uint64 // 代次：每次失效/复用后自增，用于丢弃在途的过期调度项
}

// Init 初始化定时器
func (t *Timer) Init(interval, count int64, self TimerInterface) error {
	if interval <= 0 {
		return ErrTimerInvalidInterval
	}
	t.nextTime.Store(util.CurrentMS() + interval)
	t.interval.Store(interval)
	if count == InfiniteCount {
		count = CountCorrectionValue
	}
	t.count.Store(count)
	if self != nil {
		t.self.Store(&self)
	}
	t.isActive.Store(true)

	return nil
}

// SetSelf 设置自身引用
func (t *Timer) SetSelf(self TimerInterface) {
	if self == nil {
		return
	}
	t.self.Store(&self)
}

// getSelf 返回自身接口引用
func (t *Timer) getSelf() TimerInterface {
	p := t.self.Load()
	if p == nil {
		return nil
	}
	return *p
}

// nextGen 使当前代次失效并自增：所有在途（通道/队列中）的旧调度项都会因代次
// 不匹配而被丢弃，避免定时器被回收复用后旧调度项继续操作同一个实例。
func (t *Timer) nextGen() uint64 {
	return t.gen.Add(1)
}

// RemoveFromManager 从管理器中移除
func (t *Timer) RemoveFromManager(cleanPool bool) {
	if !t.isActive.CompareAndSwap(true, false) {
		return // 已经移除
	}

	// 令在途的旧调度项失效（可能还躺在 timerChannel / addTimerChan 中）
	t.nextGen()

	// 只在摘链这一小段持锁，绝不在持锁状态下调用业务回调（Put 同理）
	if wheel := t.wheel.Load(); wheel != nil {
		wheel.wheelLock.Lock()
		// 持锁后复核归属：processWheelTick 可能已在它的临界区里摘链、扣减计数
		// 并把 wheel 置 nil，此时再减一次就会把计数打成负数。
		stillOurs := t.wheel.Load() == wheel
		t.Node.Remove()
		wheel.wheelLock.Unlock()
		if stillOurs {
			wheel.timerCount.Add(-1)
		}
	} else {
		t.Node.Remove()
	}
	t.wheel.Store(nil)

	if cleanPool {
		if self := t.getSelf(); self != nil {
			timerPool.Put(self)
		}
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

	if t.count.Load() != CountCorrectionValue {
		if t.count.Add(-1) <= 0 {
			return false
		}
	}

	t.nextTime.Store(util.CurrentMS() + t.interval.Load())
	return true
}

// GetUID 返回唯一标识符
func (t *Timer) GetUID() int64 {
	return t.uid.Load()
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
		interval = t.nextTime.Load() - util.CurrentMS()
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
	if count == InfiniteCount {
		count = CountCorrectionValue
	}
	t.count.Store(count)
}

// GetInterval 返回定时器间隔
func (t *Timer) GetInterval() int64 {
	return t.interval.Load()
}

// GetRemainingCount 返回剩余执行次数
func (t *Timer) GetRemainingCount() int64 {
	if c := t.count.Load(); c == CountCorrectionValue {
		return InfiniteCount
	} else {
		return c
	}
}

// NewTimer 创建新的定时器实例
func NewTimer(interval, count int64, cls TimerInterface) (TimerInterface, error) {
	if cls == nil {
		return nil, ErrTimerInvalidType
	}

	// timerPool.Get 内部用 reflect.TypeOf(cls).Elem() 取元素类型，
	// 传入非指针会直接 panic。用值接收者自行实现 TimerInterface 即可触发。
	if reflect.TypeOf(cls).Kind() != reflect.Pointer {
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

	// 对象池复用：实例可以是旧的，但「身份」必须是新的。
	// 重置 UID 让管理器重新分配，并推进代次使该实例残留的旧调度项全部失效。
	parent.uid.Store(0)
	parent.nextGen()

	return timer, nil
}
