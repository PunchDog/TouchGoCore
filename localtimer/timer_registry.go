package localtimer

import (
	"context"
	"sync"
	"sync/atomic"

	"touchgocore/syncmap"
	"touchgocore/vars"
)

// notifyTimerLost 上报「续期彻底失败」的定时器，并把它强制归还对象池。
//
// 重试仍失败说明这条调度项已经永久消失：节点不在任何轮里、isActive 也已回滚，
// 业务侧表现为该定时器的 Tick 再也不触发，且从外部完全无法察觉。这里主动作废
// 实例（此刻 inTick 仍为 1，归还会由 endTick 落地），把「静默失联」变成一条
// 明确的错误日志与一次回收。
func (m *TimerManager) notifyTimerLost(timer TimerInterface, parent *Timer, cause error) {
	vars.Error("定时器续期失败已强制回收: uid=%d, 间隔=%dms, 剩余次数=%d, 原因=%v",
		timer.GetUID(), parent.GetInterval(), parent.GetRemainingCount(), cause)
	parent.RemoveFromManager(true)
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
	asyncPool *asyncTickPool

	// degraded：本轮生命周期里有分片消费协程 panic 退出（S68）。置位后其余分片
	// 照常消费，但健康检查报告「未运行」——半死不活的状态不该对外报正常。
	degraded atomic.Bool
}

// shutdown 关闭本轮生命周期，幂等且并发安全
func (rt *timerRuntime) shutdown() {
	rt.closeOnce.Do(func() {
		if rt.asyncPool != nil {
			rt.asyncPool.stopAccepting()
		}
		close(rt.closech)
	})
}

type asyncTimerKey struct {
	parent *Timer
	epoch  uint64
}

// asyncTickJob 是一次异步 Tick 执行请求。
//
// 时间轮只负责判定到期并把任务投进池；worker 取到后自己跑完 Tick、续期/作废与
// endTick，不再把完成事件回传给分片消费者——异步不是第二套调度系统。
type asyncTickJob struct {
	task timerTask
	key  asyncTimerKey
}

// asyncTickPool 是有上限的异步 Tick 执行池。
//
// 队列无长度上限：派发时节点已经摘链，提交若因容量被拒，定时器就静默失联。
type asyncTickPool struct {
	mu      sync.Mutex
	limit   int
	stopped bool

	// queue 是待执行任务的全局 FIFO。
	queue []asyncTickJob
	// busy 标记某个 Timer 已有任务在执行。
	busy map[asyncTimerKey]bool
	// parked 保存执行期间到达的后续任务。每个 Timer 只留最新一条：更早的
	// 调度项 gen 必然已被新的 AddTimer 取代，执行也只会被 isValid 丢弃。
	parked map[asyncTimerKey]asyncTickJob
	// active 是当前存活的 worker 数，不超过 limit。
	active int
	// running 是正在执行回调的任务数，用于统计在飞量。
	running int

	workerWG sync.WaitGroup
}

// AsyncWorkerStats is a snapshot of the asynchronous Tick worker pool.
type AsyncWorkerStats struct {
	Limit    int // configured maximum worker count
	Workers  int // currently registered workers
	Queued   int // tasks waiting in queue or parked for their timer
	InFlight int // async tasks currently executing
}

// DefaultAsyncWorkerLimit is the default maximum number of asynchronous Tick workers.
const DefaultAsyncWorkerLimit = 512

var configuredAsyncWorkerLimit atomic.Int64

var timerFlightRegistry = struct {
	mu      sync.Mutex
	flights map[asyncTimerKey]*timerFlightGate
}{flights: make(map[asyncTimerKey]*timerFlightGate)}

type timerFlightGate struct {
	token chan struct{}
	refs  int
}

// acquireTimerFlight 阻塞取得单飞令牌。仅异步 worker 使用：worker 阻塞时，
// 其它 worker 与分片消费者仍在推进，令牌必然会被当前持有者归还。
func acquireTimerFlight(key asyncTimerKey) func() {
	gate := lookupOrCreateFlight(key)
	<-gate.token
	var once sync.Once
	return func() {
		once.Do(func() {
			gate.token <- struct{}{}
			dropFlight(key, gate)
		})
	}
}

// tryAcquireTimerFlight 尝试取得单飞令牌但不阻塞。分片消费者必须走这条路径：
// 它一卡在令牌上，整片调度就停摆，而令牌可能正被 cleanupWheel 的长回调持有。
func tryAcquireTimerFlight(key asyncTimerKey) (func(), bool) {
	gate := lookupOrCreateFlight(key)
	select {
	case <-gate.token:
		var once sync.Once
		return func() {
			once.Do(func() {
				gate.token <- struct{}{}
				dropFlight(key, gate)
			})
		}, true
	default:
		dropFlight(key, gate) // 未取到令牌：只回退登记引用，不能归还令牌
		return nil, false
	}
}

func lookupOrCreateFlight(key asyncTimerKey) *timerFlightGate {
	timerFlightRegistry.mu.Lock()
	defer timerFlightRegistry.mu.Unlock()
	gate := timerFlightRegistry.flights[key]
	if gate == nil {
		gate = &timerFlightGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		timerFlightRegistry.flights[key] = gate
	}
	gate.refs++
	return gate
}

func dropFlight(key asyncTimerKey, gate *timerFlightGate) {
	timerFlightRegistry.mu.Lock()
	gate.refs--
	if gate.refs == 0 && timerFlightRegistry.flights[key] == gate {
		delete(timerFlightRegistry.flights, key)
	}
	timerFlightRegistry.mu.Unlock()
}

// SetAsyncWorkerLimit sets the maximum number of asynchronous Tick workers for
// the next timer runtime. Values <= 0 restore DefaultAsyncWorkerLimit.
func SetAsyncWorkerLimit(limit int) int {
	if limit <= 0 {
		limit = DefaultAsyncWorkerLimit
	}
	configuredAsyncWorkerLimit.Store(int64(limit))
	return limit
}

func asyncWorkerLimit() int {
	limit := configuredAsyncWorkerLimit.Load()
	if limit <= 0 {
		return DefaultAsyncWorkerLimit
	}
	return int(limit)
}

// GetAsyncWorkerStats returns a snapshot of the current runtime's async worker pool.
func GetAsyncWorkerStats() AsyncWorkerStats {
	rt := timerRT.Load()
	if rt == nil || rt.asyncPool == nil {
		return AsyncWorkerStats{Limit: asyncWorkerLimit()}
	}
	return rt.asyncPool.stats()
}

func (p *asyncTickPool) stats() AsyncWorkerStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return AsyncWorkerStats{
		Limit:    p.limit,
		Workers:  p.active,
		Queued:   len(p.queue) + len(p.parked),
		InFlight: p.running,
	}
}

func newAsyncTickPool(limit int) *asyncTickPool {
	if limit <= 0 {
		limit = DefaultAsyncWorkerLimit
	}
	return &asyncTickPool{
		limit:  limit,
		busy:   make(map[asyncTimerKey]bool),
		parked: make(map[asyncTimerKey]asyncTickJob),
	}
}

func (p *asyncTickPool) submit(task timerTask) bool {
	if p == nil || task.timer == nil {
		return false
	}
	// 过期调度项不进入队列：它的代次已被新的 AddTimer 接管，既不该执行，
	// 也不该占掉 parked 里更新的一条任务。
	if !safeTimerTaskValid(task) {
		return true
	}
	parent := safeAsyncTimerParent(task.timer)
	if parent == nil {
		return false
	}
	job := asyncTickJob{
		task: task,
		key:  asyncTimerKey{parent: parent, epoch: parent.poolEpoch.Load()},
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return false
	}
	if p.busy[job.key] {
		// 同一 Timer 已有任务在执行：只保留最新一条。更早的调度项代次已被
		// 新的 AddTimer 取代，即使执行也会被 isValid 丢弃。
		if cur, ok := p.parked[job.key]; ok && cur.task.gen > job.task.gen {
			p.mu.Unlock()
			return true
		}
		p.parked[job.key] = job
		p.mu.Unlock()
		return true
	}
	p.busy[job.key] = true
	p.queue = append(p.queue, job)
	spawn := p.ensureWorkersLocked()
	p.mu.Unlock()
	p.startWorkers(spawn)
	return true
}

// ensureWorkersLocked 在有排队任务时补一个 worker，返回本次需要新起的数量。
//
// workerWG.Add 与 active 自增必须在同一临界区：wait 只在 stopAccepting 之后调用，
// 而 stopAccepting 与 submit 共用这把锁，锁内 Add 才能保证 wait 看不到半成品。
func (p *asyncTickPool) ensureWorkersLocked() (spawn int) {
	if p.active >= p.limit || len(p.queue) == 0 {
		return 0
	}
	p.active++
	p.workerWG.Add(1)
	return 1
}

func (p *asyncTickPool) startWorkers(n int) {
	for i := 0; i < n; i++ {
		go p.runWorker()
	}
}

func (p *asyncTickPool) runWorker() {
	defer p.workerWG.Done()
	for {
		p.mu.Lock()
		if len(p.queue) == 0 {
			p.active--
			p.mu.Unlock()
			return
		}
		job := p.queue[0]
		p.queue[0] = asyncTickJob{}
		p.queue = p.queue[1:]
		p.running++
		p.mu.Unlock()

		p.runJob(job)

		p.mu.Lock()
		p.running--
		p.mu.Unlock()
	}
}

// runJob 执行一个异步 Tick 的完整生命周期：取单飞令牌、执行 Tick、续期/作废、endTick。
//
// releaseKey 必须放在 defer 里：无论正常返回、提前返回还是 panic，都要把该 Timer 的
// 执行位交还，否则后续任务只能堆在 parked 里永不执行。
func (p *asyncTickPool) runJob(job asyncTickJob) {
	defer p.releaseKey(job.key)
	defer func() {
		if r := recover(); r != nil {
			vars.Error("异步定时器任务发生panic: %v", r)
		}
	}()

	parent := safeAsyncTimerParent(job.task.timer)
	if parent == nil || parent.poolEpoch.Load() != job.key.epoch {
		return
	}
	// 单飞令牌与同步收尾路径（cleanupWheel → executeTimer）互斥。同一 Timer 的
	// 并发异步任务已被 busy/parked 挡住，所以这里只可能等同步路径的那一次 Tick。
	releaseFlight := acquireTimerFlight(job.key)
	defer releaseFlight()
	if !safeTimerTaskValid(job.task) {
		return
	}
	if job.task.mgr == nil {
		return
	}
	if job.task.mgr.isClosed.Load() {
		job.task.mgr.discardDispatchedTask(job.task)
		return
	}
	job.task.mgr.runTimerTask(job.task, parent)
}

func safeAsyncTimerParent(timer TimerInterface) (parent *Timer) {
	defer func() {
		if recover() != nil {
			parent = nil
		}
	}()
	return timer.GetParent()
}

func safeTimerTaskValid(task timerTask) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	return task.isValid()
}

// releaseKey 交还某个 Timer 的执行位；有 parked 任务则把它移入队列尾部。
//
// 不在这里补 worker：本函数由刚结束一个 job 的 worker 调用，它回到循环后会立刻
// 看到新入队的任务。
func (p *asyncTickPool) releaseKey(key asyncTimerKey) {
	p.mu.Lock()
	next, ok := p.parked[key]
	if !ok {
		delete(p.busy, key)
		p.mu.Unlock()
		return
	}
	delete(p.parked, key)
	p.queue = append(p.queue, next)
	p.mu.Unlock()
}

func (p *asyncTickPool) stopAccepting() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

// wait must only be called after stopAccepting, which prevents further WaitGroup.Add.
func (p *asyncTickPool) wait(ctx context.Context) bool {
	if p == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		p.workerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// 全局状态（定时器系统单例相关）
var (
	// timerRegistryMu 串行化注册表写入与「取存量快照」：Run/TimeStop 在同一临界区
	// 取出待关清单，放开锁后才逐个 Close，避免并发 NewTimerManager 给快照制造
	// 「取到一半」的视图。管理器 Close 时自摘也不碰这段临界区。
	timerRegistryMu     sync.Mutex
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	globalTimerUID      atomic.Int64                            // 定时器ID：跨管理器全局唯一
	defaultTimerManager atomic.Pointer[TimerManager]
	timerRT             atomic.Pointer[timerRuntime]

	// timerChannels 刻意保持全局、跨生命周期共享：
	// NewTimerManager 可以在未 Run 的情况下独立调用，此时也得有地方派发。
	// S68 起为分片数组：下标即分片号，一条分片一个消费协程，同一 uid 恒定落同一分片。
	timerChannels  []chan timerTask
	timerChannelMu sync.Mutex
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
