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
	// ErrTimerReleased 实例已通过 RemoveFromManager(true) 永久归还对象池，
	// 且已被（或随时会被）下一个 NewTimer 取走，禁止再复活。
	ErrTimerReleased = errors.New("timer instance already released to pool")
	// ErrTimerCanceled 续期窗口内定时器被业务移除或被另一次 AddTimer 接管，
	// 本次续期作废。这是正常收尾，不计入调度失败统计。
	ErrTimerCanceled = errors.New("timer renewal canceled")
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
	// Remove 彻底作废本实例：停止调度并归还对象池，此后不得再引用该指针
	Remove()
	// Pause 仅停止调度，所有权留在调用方手里，稍后可用 AddTimer 复活
	Pause()
	// GetUID 返回唯一标识符
	GetUID() int64
	// GetParent 返回父定时器指针
	GetParent() *Timer
	// HasNext 检查是否有下一次执行
	HasNext() bool
	// SetSelf 设置自身引用
	SetSelf(TimerInterface)
	// RemoveFromManager 从管理器中移除；cleanPool=true 表示永久弃用该实例并归还对象池
	RemoveFromManager(cleanPool bool)
	// IsActive 检查定时器是否活跃
	IsActive() bool
}

// TimerPool 为定时器提供类型安全的对象池管理
type TimerPool struct {
	once sync.Once
	pool *syncmap.Map[string, *sync.Pool] // map[reflect.Type]*sync.Pool

	// 池行为计数。sync.Pool 没有可观测长度，「Remove 是否真的回了池」「有没有
	// 把非池实例塞进池」这类契约只能靠自己的计数器暴露出来供监控与测试断言。
	gets          atomic.Int64 // 成功从池中认领的实例数
	puts          atomic.Int64 // 成功归还进池的实例数
	putRejected   atomic.Int64 // 因缺少池发放凭证而拒绝归还的次数（>0 说明有调用方手工构造宿主对象后 Remove）
	reviveRefused atomic.Int64 // 对已作废实例调用 AddTimer 被拒的次数
}

// TimerPoolStats 是对象池统计信息快照
type TimerPoolStats struct {
	Gets          int64 // 池中认领次数
	Puts          int64 // 归还入池次数
	PutRejected   int64 // 拒绝归还次数（非池发放实例）
	ReviveRefused int64 // 拒绝复活已作废实例次数
}

// GetTimerPoolStats 返回全局对象池统计快照
func GetTimerPoolStats() TimerPoolStats {
	return TimerPoolStats{
		Gets:          timerPool.gets.Load(),
		Puts:          timerPool.puts.Load(),
		PutRejected:   timerPool.putRejected.Load(),
		ReviveRefused: timerPool.reviveRefused.Load(),
	}
}

// maxPoolGetAttempts 从池中取实例时最多丢弃多少个「所有权已被认领」的陈旧条目。
// 有界即可：超出就说明池里几乎全是活跃实例，再捞只是浪费，直接新建。
const maxPoolGetAttempts = 8

// Get 从池中获取定时器，成功后调用方即成为该实例的唯一主人。
//
// 所有权靠 Timer.inPool 的 CAS 转移：池中条目只有处于「等待认领」状态才发得出
// 去，取到不可认领的条目就丢弃再取，绝不把别人手里的实例发第二份。
func (p *TimerPool) Get(cls TimerInterface) TimerInterface {
	// once 保证并发首次调用只会初始化一次（原来的裸 nil 判断存在数据竞争）
	p.once.Do(func() {
		if p.pool == nil {
			p.pool = syncmap.NewMap[string, *sync.Pool]()
		}
	})
	tpname, _ := util.GetClassName(cls)
	pool, ok := p.pool.Load(tpname)
	if !ok {
		tp := reflect.TypeOf(cls).Elem()
		// LoadOrStore 而非 Store：并发首次调用时只会保留一个池。
		// 各自 Store 会后写覆盖前写，被覆盖那个池里已归还的对象就此孤儿。
		pool, _ = p.pool.LoadOrStore(tpname, &sync.Pool{
			New: func() interface{} {
				obj := reflect.New(tp).Interface().(TimerInterface)
				// 新造实例视作「仍在池中待认领」，与归还后的状态统一，
				// 上面的 CAS 才能一致地判定所有权。
				if parent := obj.GetParent(); parent != nil {
					parent.inPool.Store(true)
					// 池发放凭证：只有从这里诞生的实例才允许日后 Put 回池。
					// 粘性标记，永不清除。
					parent.fromPool.Store(true)
				}
				return obj
			},
		})
	}

	for i := 0; i < maxPoolGetAttempts; i++ {
		obj := pool.Get().(TimerInterface)
		parent := obj.GetParent()
		if parent == nil {
			continue
		}
		// 正常协议下在飞实例永远不会在池里（Put 只在 inTick==0 时发生），
		// 这道闸是防御：绝不把仍有回调在执行的实例发给新主人。
		if parent.inTick.Load() != 0 {
			continue
		}
		if parent.inPool.CompareAndSwap(true, false) {
			// 认领成功：粘性释放标记随所有权一并转移给新主人
			parent.released.Store(false)
			parent.pendingRelease.Store(false)
			parent.putClaimed.Store(false)
			p.gets.Add(1)
			return obj
		}
	}
	// 连续捞到不可认领的条目：现造一个，绝不返回 nil 或别人的实例。
	obj := pool.New().(TimerInterface)
	if parent := obj.GetParent(); parent != nil {
		parent.inPool.Store(false)
		parent.released.Store(false)
	}
	p.gets.Add(1)
	return obj
}

// Put 将定时器归还到池中。
//
// 只有「主人确认永远不再引用、也不再 AddTimer 复活该实例」时才允许归还：归还
// 会给实例打上 released 粘性标记，此后对它调用 AddTimer 一律返回 ErrTimerReleased。
// inPool 的 CAS 是所有权闸：重复归还会让同一个「仍被业务使用」的指针在池里
// 堆积成百上千份，NewTimer 于是把活实例再发给别人，两个逻辑定时器共用一个
// 对象，彼此的代次互相把对方的调度项判为过期，表现为定时器彻底不再 Tick。
func (p *TimerPool) Put(cls TimerInterface) {
	if cls == nil || p.pool == nil {
		return
	}
	parent := cls.GetParent()
	if parent == nil {
		return
	}
	// 入池资格闸：fromPool 只由 sync.Pool.New 置位。调用方自己 new 出来的宿主
	// 对象（type X struct{ localtimer.Timer }）没有凭证，一旦进池，池就会把整个
	// 宿主对象当成空白定时器发给下一个 NewTimer，业务字段被覆盖、内存被踩踏。
	if !parent.fromPool.Load() {
		p.putRejected.Add(1)
		return
	}
	if !parent.inPool.CompareAndSwap(false, true) {
		return // 已在池中：重复归还直接丢弃
	}
	parent.released.Store(true)
	tpname, _ := util.GetClassName(cls)
	pool, ok := p.pool.Load(tpname)
	if !ok {
		// 无池可归：撤销标记，别把实例留在「谁都不认」的中间态
		parent.released.Store(false)
		parent.pendingRelease.Store(false)
		parent.putClaimed.Store(false)
		parent.inPool.Store(false)
		return
	}
	pool.Put(cls)
	p.puts.Add(1)
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

	// mu 定时器私有锁：串行化 AddTimer 的「清理旧调度→推进代次→恢复活跃→入队」
	// 与 handleTimerAdd 的「校验→入链」。单靠 gen 唯一无法保证只有一份 task 入链
	//（校验通过后、入链前状态可被并发 AddTimer 改写），必须用锁把两段临界区互斥。
	// 加锁顺序恒为 mu → wheelLock，全库一致，不存在逆序死锁。
	mu sync.Mutex

	// 状态标志
	isActive atomic.Bool
	gen      atomic.Uint64 // 代次：每次失效/复用后自增，用于丢弃在途的过期调度项
	// inPool 对象池所有权标记：true 表示实例在池中等待认领，false 表示已被某个
	// 主人持有。TimerPool.Get/Put 靠 CAS 争夺它，保证同一实例不会同时属于池和业务。
	inPool atomic.Bool
	// released 粘性标记：一旦通过 Put 永久归还过就为 true，直到下一个主人从池中
	// 认领才清除。AddTimer 见它即拒绝复活——实例本身无法区分「刚归还的我」和
	// 「已被池发给的别人」，而契约上调用方已经宣布永久弃用它了。
	released atomic.Bool
	// fromPool 池发放凭证：只有 TimerPool.Get 里由 sync.Pool.New 造出的实例为
	// true，且永不清除。Put 以此拒绝调用方手工构造的宿主对象入池。
	fromPool atomic.Bool
	// inTick 业务回调在飞计数。Tick 执行期间实例正被消费协程使用，此时归还
	// 等于把「正在跑回调的对象」交给新主人，两边同时写同一块内存。
	inTick atomic.Int32
	// pendingRelease Tick 在飞期间收到的作废请求，由 endTick 在回调结束后补做归还。
	pendingRelease atomic.Bool
	// putClaimed 归还抢占标记：Tick 在飞期间挂起的作废请求（由 endTick 补做）与
	// 业务侧再次 Remove 可能同时判定为 releaseNow，两边都去 Put 就会争抢同一次归还。
	// Put 里的 inPool CAS 是最后一道闸，但只有在这里抢占才能保证「收尾只做一次」，
	// 也让统计口径（Puts）不被无意义的重复调用污染。实例被下一个主人认领时重置。
	putClaimed atomic.Bool
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

// releaseOutcome 表示一次移除后「实例该不该、能不能归还对象池」的仲裁结论。
type releaseOutcome int8

const (
	releaseNone      releaseOutcome = iota // 无需归还（只停调度）
	releaseNow                             // 立即归还：由调用方在释放 t.mu 之后执行 Put
	releaseDeferred                        // 业务回调在飞，由 endTick 收尾归还
	releaseAlready                         // 实例已在池中（重复作废），幂等 no-op
	releaseNotPooled                       // 无池发放凭证，禁止入池
)

// RemoveFromManager 从管理器中移除
//
// cleanPool=true 表示「永久作废」：摘链之后把实例归还对象池，调用方此后不得再
// 引用它——此后对该实例的 AddTimer 一律返回 ErrTimerReleased，无论池是否已经
// 把它发给下一个 NewTimer。只是暂停调度、稍后可能复活的场景用 Pause()。
// Remove() 即 cleanPool=true 的便捷入口。
//
// 整体持有 mu：与 handleTimerAdd 的「校验+入链」、AddTimer 的「清理+推进代次+
// 入队」互斥。若不加锁，本函数读到 wheel==nil 后、摘链前，并发 AddTimer 可让
// 节点入链（count+1），随后的 else 分支 Node.Remove 会把刚入链的节点无锁扯出
// 且不扣计数，留下 count=1 / len=0 的永久幻影。
//
// Put 一定在解锁之后：sync.Pool 会执行用户代码，且归还完成瞬间实例就可能被别的
// NewTimer 取走，持着业务私有锁做这件事等于把新主人的初始化排在旧主人的锁上。
func (t *Timer) RemoveFromManager(cleanPool bool) {
	if outcome := t.removeWithLock(cleanPool); outcome == releaseNow {
		t.releaseToPool()
	}
}

// removeWithLock 在 t.mu 保护下执行移除。
//
// 临界区独立成方法只为让 mu 通过 defer 释放：摘链路径上的任何 panic 都不会把
// 这把私有锁永久扣住，否则该实例后续的 AddTimer/Remove/handleTimerAdd 会全部挂死。
func (t *Timer) removeWithLock(cleanPool bool) releaseOutcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.removeFromManagerLocked(cleanPool)
}

// requestReleaseLocked 判定归还方式，调用方必须已持有 t.mu。
//
// 判定为「立即归还」时同步抢占 putClaimed：两条路径（endTick 补做与业务 Remove）
// 同时得出 releaseNow 时，只有抢占成功的一方真正执行 Put。
func (t *Timer) requestReleaseLocked() releaseOutcome {
	// 凭证闸在最前：手工构造的宿主对象连「池」这个概念都不该参与
	if !t.fromPool.Load() {
		// 在仲裁处留痕：Put 里的同名闸门是兜底，正常路径永远走不到那里
		timerPool.putRejected.Add(1)
		return releaseNotPooled
	}
	// 已在池中 ⇒ 本实例已被归还（或从未离开池），重复归还一律丢弃
	if t.inPool.Load() {
		return releaseAlready
	}
	// 回调在飞：此刻 Put 等于把正在执行 Tick 的对象交给新主人，两边同时写一块
	// 内存。挂起请求，endTick 里补做。pendingRelease 粘到池重新发放为止，中途
	// 不允许任何人用 AddTimer 把「已宣布作废」的实例再抢回活跃态。
	if t.inTick.Load() > 0 {
		t.pendingRelease.Store(true)
		return releaseDeferred
	}
	if !t.putClaimed.CompareAndSwap(false, true) {
		return releaseAlready // 另一方已抢占本次归还
	}
	return releaseNow
}

// abandoned 报告实例是否已进入永久作废流程（已归还，或已排队等待 endTick 归还）
func (t *Timer) abandoned() bool {
	return t.released.Load() || t.pendingRelease.Load()
}

// releaseToPool 把实例归还对象池；调用方不得再持有 t 的任何引用语义保证
func (t *Timer) releaseToPool() {
	if self := t.getSelf(); self != nil {
		timerPool.Put(self)
	}
}

// beginTick 登记一次业务回调执行，返回 false 表示本次调度项已作废、调用方必须放弃回调。
//
// isValid 校验与真正的「在飞」标记之间总有窗口：期间实例可能被 Remove 归还对象池、
// 又被下一个 NewTimer 认领并推进代次。若只增计数不复核，就会与全新主人同时写同一块
// 内存（use-after-free 到池对象）。这里先增在飞计数、再校验代次与作废标记，两次原子
// 操作构成 Dekker 配对：池的认领方也是「先改所有权标记、后读在飞计数」，
// 因此双方不可能同时认为自己是唯一持有者。
func (t *Timer) beginTick(expectGen uint64) bool {
	t.inTick.Add(1)

	if t.gen.Load() != expectGen {
		// 实例已易主（或本条是陈旧的重复调度项）：只撤销在飞标记，
		// 绝不替新主人做归还收尾。
		t.inTick.Add(-1)
		return false
	}
	if t.abandoned() || t.inPool.Load() {
		// 代次没动却已进入作废流程：走正常 endTick，让挂起的归还请求照常落地。
		t.endTick()
		return false
	}
	return true
}

// endTick 结束一次回调：把在飞期间挂起的归还请求补做掉。
//
// 必须是 executeTimer 的最后一步——本函数返回后实例随时可能已属于新主人，
// 调用方再碰一下 task.timer 就是踩别人的对象。
func (t *Timer) endTick() {
	if t.inTick.Add(-1) > 0 {
		return
	}
	if !t.pendingRelease.Load() {
		return
	}
	t.mu.Lock()
	outcome := t.requestReleaseLocked()
	t.mu.Unlock()
	if outcome == releaseNow {
		t.releaseToPool()
	}
}

// removeFromManagerLocked 执行实际移除逻辑，调用方必须已持有 t.mu。
//
// 不能因为「已经不活跃」就整体提前返回：Pause() 只是停调度，业务随后仍可能
// Remove() 正式作废它。短路会让这条路径永远走不到归还分支，实例泄漏在使用者手里。
func (t *Timer) removeFromManagerLocked(cleanPool bool) releaseOutcome {
	// 陈旧主人闸：实例已在池中 ⇒ 归还早已完成，迟到的 Remove/Pause 一律整体
	// 忽略。否则摘链会把池（或已认领它的新主人）的节点无锁扯走，计数打成负数。
	// 只护得住「作废后重复调用」，护不住「作废后新主人已重新入链」——那种悬垂
	// 引用在 Go 里无法从库内检测，契约上要求调用方作废后立刻丢弃指针。
	if t.inPool.Load() {
		if cleanPool {
			return releaseAlready
		}
		return releaseNone
	}

	// 只有「活跃 → 不活跃」这一次转换才允许推进代次和摘链扣计数：
	// 重复执行会把 timerCount 打成负数。
	if t.isActive.CompareAndSwap(true, false) {
		// 令在途的旧调度项失效（可能还躺在 timerChannel / addTimerChan 中）
		t.nextGen()

		// 只在摘链这一小段持锁，绝不在持锁状态下调用业务回调（Put 同理）
		if wheel := t.ownedWheel(); wheel != nil {
			t.detachFromWheelLocked(wheel)
		} else {
			// mu 持有下 handleTimerAdd 无法并发入链，wheel==nil ⇒ 节点必然不在任何
			// 链表中，这里的 Node.Remove 只是防御性兜底（无链可摘时为 no-op）。
			t.Node.Remove()
		}
		// 归属可能已在别处被清掉（甚至是在途标记），这里兜底再清一次为幂等操作。
		t.wheel.Store(nil)
	}

	if !cleanPool {
		return releaseNone
	}
	return t.requestReleaseLocked()
}

// detachFromWheelLocked 从 t 当前归属的时间轮上摘链并扣减计数，调用方必须已持有 t.mu。
//
// 同样独立成方法让 wheelLock 走 defer：临界区内的 panic 若漏出解锁，整个时间轮
// 会被永久扣住（该轮所有定时器停摆，Close 也永远等不到协程退出）。
func (t *Timer) detachFromWheelLocked(wheel *TimerWheel) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 持锁后复核归属：processWheelTick 可能已在它的临界区里摘链、扣减计数
	// 并把 wheel 置 nil，此时再减一次就会把计数打成负数。
	if t.wheel.Load() != wheel {
		return
	}
	// 归属未变才允许摘链扣减。归属已变更（本节点已被摘链并重新入链到别的轮）
	// 时绝不能再无条件 Node.Remove：那会把节点从「当前归属轮」的链表里无锁
	// 扯出，且归属轮的 timerCount 无人扣减，形成永久幻影 +1（count=1 而
	// len=0）。节点归属已易主，其收尾交由新归属轮的派发/清理路径，计数自然配对。
	t.Node.Remove()
	// 「摘链 → 减计数 → 清引用」三者必须在同一把 wheelLock 临界区内
	// 原子完成：若清引用延后到解锁之后，processWheelTick 的派发/清理
	// 分支可能在这段间隙里看到一个「已不在链却仍挂着 wheel 引用」的
	// 节点，或入链方在间隙入链后被本处的 Store(nil) 无痕清掉归属，
	// 造成计数与链表长度漂移（-1 / 幻影 +1）。
	wheel.timerCount.Add(-1)
	t.wheel.Store(nil)
	// 统计口径挂在「真正摘掉一条链」这一点上：暂停、移除、耗尽回收、复活前清理
	// 都会经过这里，而派发与迁移走的是 Node.Remove（不改归属为 nil 的语义），
	// 不会被误计。轮上没有归属管理器时（构造期）留空跳过。
	if wheel.mgr != nil {
		wheel.mgr.stats.timersRemoved.Add(1)
	}
}

// Remove 公共移除方法：彻底作废本实例——停止调度并归还对象池。
//
// 归还后实例可能被任意一次 NewTimer 发给别的调用方，指针仍留在手里也不代表
// 它还是你的：对它调用 AddTimer 一律返回 ErrTimerReleased。断线重连这类「先停
// 调度、稍后再跑」的场景必须改用 Pause()，否则会把仍在使用的实例交还池、再被
// 别人拿走，两个逻辑定时器共用一个 Timer 对象，彼此的代次互相把对方的调度项判
// 为过期，表现为定时器彻底不再 Tick。
func (t *Timer) Remove() {
	t.RemoveFromManager(true)
}

// Pause 暂停调度：把节点摘出时间轮、令在途调度项失效，但所有权留在调用方手里。
//
// 之后用 AddTimer 复活是受支持的操作（rpc 客户端断线重连即此用法）。
func (t *Timer) Pause() {
	t.RemoveFromManager(false)
}

// HasNext 检查是否有下一次执行
func (t *Timer) HasNext() bool {
	if !t.isActive.Load() {
		return false
	}

	if t.count.Load() != CountCorrectionValue {
		// CAS 扣减：count 已 <= 0 时直接判负返回，绝不再扣。
		// 原先无条件 Add(-1) 在 count==0 时会把 -1 存回去，GetRemainingCount
		// 从此返回 -1；这里保证 count 永不为负，且 count==1 扣到 0 后返回 false
		// 的原语义不变。
		for {
			c := t.count.Load()
			if c <= 0 {
				return false
			}
			if t.count.CompareAndSwap(c, c-1) {
				if c-1 <= 0 {
					return false // 最后一次执行，扣到 0 并结束
				}
				break
			}
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

// SetInterval 更新执行间隔（毫秒）。interval <= 0 时忽略，避免时间轮收到非正间隔。
// 供续期路径应用 NextIntervaler 重算出的间隔。
func (t *Timer) SetInterval(interval int64) {
	if interval <= 0 {
		return
	}
	t.interval.Store(interval)
}

// GetRemainingCount 返回剩余执行次数
func (t *Timer) GetRemainingCount() int64 {
	if c := t.count.Load(); c == CountCorrectionValue {
		return InfiniteCount
	} else {
		return c
	}
}

// NewTimer 创建新的定时器实例，实例来自对象池（T 必须是指针类型，如 *MyTimer）。
// 返回 T 供调用方直接断言回具体类型使用。
// initcallback 在 Init 完成后、返回前调用，入参为池中实例（含复用实例），
// 用于初始化业务字段；池复用时每次 NewTimer 都会重新执行，天然覆盖旧状态。
// 说明：返回值用 T 而非 *T —— *T（T 为类型参数）不实现 TimerInterface，
// 写 *T 会直接编译报错（pointer to type parameter / impossible type assertion）；
// 而 T 本身受约束可实现接口，断言经 any 中转即可。
func NewTimer[T TimerInterface](interval, count int64, initcallback func(t T)) (T, error) {
	var zero T
	tp := reflect.TypeOf(zero)
	// T 必须是指针类型：对象池内部依赖 reflect.TypeOf(cls).Elem() 取元素类型，
	// 值类型会 panic，必须提前拦截。
	if tp == nil || tp.Kind() != reflect.Pointer {
		return zero, ErrTimerInvalidType
	}

	if interval <= 0 {
		return zero, ErrTimerInvalidInterval
	}

	// 构造真实原型实例：不能用 zero（类型化 nil 指针）直接入池 ——
	// GetClassName 内部 reflect.Indirect 对 nil 指针取 Elem 会 panic。
	prototype, ok := reflect.New(tp.Elem()).Interface().(T)
	if !ok {
		return zero, ErrTimerInvalidType
	}

	timer := timerPool.Get(prototype)
	parent := timer.GetParent()
	if parent == nil {
		timerPool.Put(timer)
		return zero, ErrTimerNilParent
	}

	// 对象池复用：实例可以是旧的，但「身份」必须是新的。
	// 必须在 Init 重新激活之前完成：重置 UID 让管理器重新分配（AddTimer 见 0 补发），
	// 推进代次使该实例残留的旧调度项全部失效。若放在 Init 之后，复用实例在
	// isActive=true 的窗口内会被旧代次的在途调度项命中，导致过期回调被误执行。
	parent.uid.Store(0)
	parent.nextGen()

	if err := parent.Init(interval, count, timer); err != nil {
		timerPool.Put(timer)
		return zero, err
	}

	if initcallback != nil {
		initcallback(any(timer).(T))
	}
	return any(timer).(T), nil
}
