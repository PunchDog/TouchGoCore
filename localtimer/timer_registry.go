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
// 实例（此刻 inTick 仍为 1，归还会由 endTick 落地），把「静默失联」变成一次
// 显式回收 + 一条可接管的回调，业务据此重建定时器而不是等一个永远不来的 Tick。
func (m *TimerManager) notifyTimerLost(timer TimerInterface, parent *Timer, cause error) {
	info := TimerLostInfo{
		Manager:   m,
		UID:       timer.GetUID(),
		Interval:  parent.GetInterval(),
		Remaining: parent.GetRemainingCount(),
		Cause:     cause,
	}
	vars.Error("定时器续期失败已强制回收: uid=%d, 间隔=%dms, 剩余次数=%d, 原因=%v",
		info.UID, info.Interval, info.Remaining, cause)

	if fn := onTimerLost.Load(); fn != nil {
		// 回调来自业务，绝不能让它把收尾（下面的 retire 与 endTick）打断
		func() {
			defer func() {
				if r := recover(); r != nil {
					vars.Error("OnTimerLost 回调发生panic: %v", r)
				}
			}()
			(*fn)(info)
		}()
	}
	m.retireAfterExhaustion(parent)
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

type asyncTickJob struct {
	task      timerTask
	shard     int
	flightKey asyncTimerKey
}

type asyncTickCompletion struct {
	pool          *asyncTickPool
	job           asyncTickJob
	parent        *Timer
	flightKey     asyncTimerKey
	releaseFlight func()
	ran           bool
	// orphan 标记本次完成事件的原分片消费者已永久退出，无处投递。
	// 收尾改为「作废归还」而不是续期：续期只会把调度项塞进无人消费的通道。
	orphan bool
	ack    chan struct{}
}

type asyncTickWorker struct {
	id    uint64
	queue []asyncTickJob
}

// asyncTickPool owns a bounded number of worker goroutines and an unbounded FIFO
// queue per worker. All worker registration and queue mutations use mu.
type asyncTickPool struct {
	mu       sync.Mutex
	limit    int
	stopped  bool
	nextID   uint64
	round    int
	workers  []*asyncTickWorker
	busy     map[asyncTimerKey]bool
	deferred map[asyncTimerKey][]asyncTickJob
	pending  []int
	results  []chan asyncTickCompletion
	progress []chan struct{}
	// gone 是「该分片消费者已永久退出」的信号，close 表示不再有人接收完成事件。
	// 没有它，worker 会永久阻塞在会合投递上，连带它的单飞令牌与 busy 标记一起滞留。
	gone     []chan struct{}
	goneOnce []sync.Once
	orphaned atomic.Int64
	workerWG sync.WaitGroup
}

// AsyncWorkerStats is a snapshot of the asynchronous Tick worker pool.
type AsyncWorkerStats struct {
	Limit    int   // configured maximum worker count
	Workers  int   // currently registered workers
	Queued   int   // tasks waiting in worker queues
	InFlight int   // async tasks accepted but not finalized by their shard
	Orphaned int64 // completions finalized locally because their shard consumer was gone
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

// acquireTimerFlight 阻塞取得单飞令牌。仅异步 worker 使用：它阻塞时消费者仍能
// 处理完成事件，令牌必然会被归还。
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

// tryAcquireTimerFlight 尝试取得单飞令牌但不阻塞。消费者协程必须走这条路径：
// 它在等待令牌时无法处理异步完成事件，而完成事件正是令牌的释放方，阻塞即死锁。
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
	stats := AsyncWorkerStats{Limit: p.limit, Workers: len(p.workers), Orphaned: p.orphaned.Load()}
	for _, pending := range p.pending {
		stats.InFlight += pending
	}
	for _, worker := range p.workers {
		stats.Queued += len(worker.queue)
	}
	for _, jobs := range p.deferred {
		stats.Queued += len(jobs)
	}
	stats.InFlight -= stats.Queued
	return stats
}

func newAsyncTickPool(limit, shards int) *asyncTickPool {
	if limit <= 0 {
		limit = DefaultAsyncWorkerLimit
	}
	if shards <= 0 {
		shards = 1
	}
	pool := &asyncTickPool{
		limit:    limit,
		busy:     make(map[asyncTimerKey]bool),
		deferred: make(map[asyncTimerKey][]asyncTickJob),
		pending:  make([]int, shards),
		results:  make([]chan asyncTickCompletion, shards),
		progress: make([]chan struct{}, shards),
		gone:     make([]chan struct{}, shards),
		goneOnce: make([]sync.Once, shards),
	}
	for i := range pool.results {
		// Rendezvous delivery prevents a second unbounded completion queue.
		pool.results[i] = make(chan asyncTickCompletion)
		pool.progress[i] = make(chan struct{}, 1)
		pool.gone[i] = make(chan struct{})
	}
	return pool
}

func (p *asyncTickPool) submit(job asyncTickJob) bool {
	if p == nil || job.shard < 0 || job.shard >= len(p.results) || job.task.timer == nil {
		return false
	}
	parent := safeAsyncTimerParent(job.task.timer)
	if parent == nil {
		return false
	}
	epoch := parent.poolEpoch.Load()
	job.flightKey = asyncTimerKey{parent: parent, epoch: epoch}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return false
	}
	p.pending[job.shard]++
	if p.busy[job.flightKey] {
		p.deferred[job.flightKey] = append(p.deferred[job.flightKey], job)
		p.mu.Unlock()
		return true
	}
	// Reserve this Timer incarnation before exposing the job to a worker. This
	// closes the submit-to-beginTick window without confusing later pool reuse.
	p.busy[job.flightKey] = true

	var start *asyncTickWorker
	if len(p.workers) < p.limit {
		p.nextID++
		start = &asyncTickWorker{id: p.nextID, queue: []asyncTickJob{job}}
		p.workers = append(p.workers, start)
		p.workerWG.Add(1)
	} else {
		idx := p.leastQueuedWorkerLocked()
		if idx < 0 {
			delete(p.busy, job.flightKey)
			p.pending[job.shard]--
			p.mu.Unlock()
			return false
		}
		p.workers[idx].queue = append(p.workers[idx].queue, job)
	}
	p.mu.Unlock()

	if start != nil {
		go p.runWorker(start)
	}
	return true
}

func (p *asyncTickPool) leastQueuedWorkerLocked() int {
	if len(p.workers) == 0 {
		return -1
	}
	start := p.round % len(p.workers)
	best := start
	for i := 1; i < len(p.workers); i++ {
		idx := (start + i) % len(p.workers)
		if len(p.workers[idx].queue) < len(p.workers[best].queue) {
			best = idx
		}
	}
	p.round = (best + 1) % len(p.workers)
	return best
}

func (p *asyncTickPool) nextJob(worker *asyncTickWorker) (asyncTickJob, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(worker.queue) == 0 {
		for i, candidate := range p.workers {
			if candidate == worker {
				p.workers = append(p.workers[:i], p.workers[i+1:]...)
				if p.round >= len(p.workers) {
					p.round = 0
				}
				break
			}
		}
		return asyncTickJob{}, false
	}
	job := worker.queue[0]
	worker.queue[0] = asyncTickJob{}
	worker.queue = worker.queue[1:]
	switch {
	case len(worker.queue) == 0:
		worker.queue = nil
	case len(worker.queue)*4 < cap(worker.queue):
		compacted := make([]asyncTickJob, len(worker.queue))
		copy(compacted, worker.queue)
		worker.queue = compacted
	}
	return job, true
}

func (p *asyncTickPool) runWorker(worker *asyncTickWorker) {
	defer p.workerWG.Done()
	for {
		job, ok := p.nextJob(worker)
		if !ok {
			return
		}
		if job.task.timer == nil {
			p.releaseTimer(job.flightKey)
			p.finishWithoutCompletion(job.shard)
			continue
		}
		parent := safeAsyncTimerParent(job.task.timer)
		if parent == nil || !safeTimerTaskValid(job.task) || parent.poolEpoch.Load() != job.flightKey.epoch {
			p.releaseTimer(job.flightKey)
			p.finishWithoutCompletion(job.shard)
			continue
		}
		releaseFlight := acquireTimerFlight(job.flightKey)
		if !safeTimerTaskValid(job.task) || !parent.beginTick(job.task.gen) {
			releaseFlight()
			p.releaseTimer(job.flightKey)
			p.finishWithoutCompletion(job.shard)
			continue
		}

		ran := false
		if job.task.mgr != nil && !job.task.mgr.isClosed.Load() {
			job.task.mgr.runTimerTick(job.task.timer)
			ran = true
		}
		completion := asyncTickCompletion{
			pool:          p,
			job:           job,
			parent:        parent,
			flightKey:     job.flightKey,
			releaseFlight: releaseFlight,
			ran:           ran,
			ack:           make(chan struct{}, 1),
		}
		p.deliverCompletion(completion, job.shard)
		p.finishWithoutCompletion(job.shard)
	}
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

// releaseTimer unblocks same-Timer jobs only after the current callback and its
// consumer-side finalization have completed.
func (p *asyncTickPool) releaseTimer(key asyncTimerKey) {
	if key.parent == nil {
		return
	}
	p.mu.Lock()
	delete(p.busy, key)
	jobs := p.deferred[key]
	if len(jobs) == 0 {
		delete(p.deferred, key)
		p.mu.Unlock()
		return
	}
	job := jobs[0]
	if !safeTimerTaskValid(job.task) || key.parent.poolEpoch.Load() != key.epoch {
		if len(jobs) == 1 {
			delete(p.deferred, key)
		} else {
			p.deferred[key] = jobs[1:]
		}
		p.mu.Unlock()
		p.finishWithoutCompletion(job.shard)
		p.releaseTimer(key)
		return
	}
	if len(jobs) == 1 {
		delete(p.deferred, key)
	} else {
		p.deferred[key] = jobs[1:]
	}
	p.busy[key] = true

	var start *asyncTickWorker
	if len(p.workers) < p.limit {
		p.nextID++
		start = &asyncTickWorker{id: p.nextID, queue: []asyncTickJob{job}}
		p.workers = append(p.workers, start)
		p.workerWG.Add(1)
	} else {
		idx := p.leastQueuedWorkerLocked()
		if idx < 0 {
			delete(p.busy, key)
			p.deferred[key] = append([]asyncTickJob{job}, p.deferred[key]...)
			p.mu.Unlock()
			return
		}
		p.workers[idx].queue = append(p.workers[idx].queue, job)
	}
	p.mu.Unlock()

	if start != nil {
		go p.runWorker(start)
	}
}

func (p *asyncTickPool) finishWithoutCompletion(shard int) {
	p.mu.Lock()
	if shard >= 0 && shard < len(p.pending) && p.pending[shard] > 0 {
		p.pending[shard]--
	}
	p.mu.Unlock()
	if shard >= 0 && shard < len(p.progress) {
		select {
		case p.progress[shard] <- struct{}{}:
		default:
		}
	}
}

func (p *asyncTickPool) pendingForShard(shard int) int {
	if p == nil || shard < 0 || shard >= len(p.pending) {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pending[shard]
}

func (p *asyncTickPool) resultsForShard(shard int) <-chan asyncTickCompletion {
	if p == nil || shard < 0 || shard >= len(p.results) {
		return nil
	}
	return p.results[shard]
}

func (p *asyncTickPool) progressForShard(shard int) <-chan struct{} {
	if p == nil || shard < 0 || shard >= len(p.progress) {
		return nil
	}
	return p.progress[shard]
}

// markShardGone 声明第 shard 号分片的消费者已永久退出，此后不再接收完成事件。
//
// 幂等：同一分片可能被排空路径与分片降级路径各声明一次。
func (p *asyncTickPool) markShardGone(shard int) {
	if p == nil || shard < 0 || shard >= len(p.gone) {
		return
	}
	p.goneOnce[shard].Do(func() { close(p.gone[shard]) })
}

func (p *asyncTickPool) goneForShard(shard int) <-chan struct{} {
	if p == nil || shard < 0 || shard >= len(p.gone) {
		return nil
	}
	return p.gone[shard]
}

// deliverCompletion 把完成事件交回原分片消费者，并等它收尾完毕。
//
// 会合投递要求「必须有接收方在场」。消费者 panic 退出且排空超时后，这个前提
// 永久不成立：继续阻塞投递会让 worker、它的单飞令牌与 busy 标记一起滞留，
// 该 Timer 之后所有调度都堆进 deferred 永不再执行，pending 也永不归零。
// 因此消费者消失时改为本地孤儿收尾——不续期，直接作废并归还实例。
func (p *asyncTickPool) deliverCompletion(completion asyncTickCompletion, shard int) {
	if p == nil {
		completion.orphan = true
		consumeAsyncTickCompletion(completion)
		return
	}
	if shard < 0 || shard >= len(p.results) {
		completion.orphan = true
		p.orphaned.Add(1)
		consumeAsyncTickCompletion(completion)
		return
	}
	select {
	case p.results[shard] <- completion:
		<-completion.ack
	case <-p.goneForShard(shard):
		completion.orphan = true
		p.orphaned.Add(1)
		consumeAsyncTickCompletion(completion)
	}
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

// TimerLostInfo 描述一个彻底失联（续期重试仍失败、已被强制回收）的定时器。
type TimerLostInfo struct {
	Manager   *TimerManager // 归属管理器
	UID       int64         // 定时器唯一ID
	Interval  int64         // 执行间隔（毫秒）
	Remaining int64         // 剩余执行次数，-1 表示无限
	Cause     error         // 续期失败的最终原因
}

// onTimerLost 业务注册的失联回调（copy-on-write，读写不竞争）
var onTimerLost atomic.Pointer[func(TimerLostInfo)]

// SetOnTimerLost 注册「定时器彻底失联」回调；传 nil 取消。
//
// 正常路径下该回调永不触发；一旦触发说明下游 Tick 太慢导致调度项被丢弃，
// 业务必须据此重建定时器。回调在 timeTick 消费协程上执行，禁止阻塞。
func SetOnTimerLost(fn func(TimerLostInfo)) {
	if fn == nil {
		onTimerLost.Store(nil)
		return
	}
	onTimerLost.Store(&fn)
}

// 全局状态（定时器系统单例相关）
var (
	// timerRegistryMu 冻结注册表的「批量关闭」与「单个注册」：Run/TimeStop 取待关
	// 清单的窗口内，并发 NewTimerManager 必须排在它后面。否则刚建好的管理器会
	// 落在清单外却被随后的清空抹掉，且从未 Close —— 它名下 5 个时间轮协程 +
	// 5 个 ticker 永久泄漏。清单内的管理器各自 Close 时自摘，不碰这段临界区。
	timerRegistryMu     sync.Mutex
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	globalTimerUID      atomic.Int64                            // 定时器ID：跨管理器全局唯一
	defaultTimerManager atomic.Pointer[TimerManager]
	timerRT             atomic.Pointer[timerRuntime]

	// timerChannels 刻意保持全局、跨生命周期共享：
	// NewTimerManager 可以在未 Run 的情况下独立调用，此时也得有地方派发。
	// S68 起为分片数组：下标即分片号，一条分片一个消费协程，同一 uid 恒定落同一分片。
	timerChannels     []chan timerTask
	timerChannelMu    sync.Mutex
	timerChannelReady bool
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
