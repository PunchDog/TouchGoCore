package localtimer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type asyncPoolTestTimer struct {
	Timer
	id      int
	started chan<- int
	release <-chan struct{}
}

func (t *asyncPoolTestTimer) Tick() {
	if t.started != nil {
		t.started <- t.id
	}
	if t.release != nil {
		<-t.release
	}
}

func (t *asyncPoolTestTimer) IsAsyncTick() bool { return true }

// newAsyncPoolTestTimer 构造一条异步任务。mgr 用离线管理器：worker 在 Tick 后
// 走正常续期路径，把任务放进无人消费的轮通道，测试只观察执行调度本身。
func newAsyncPoolTestTimer(t *testing.T, mgr *TimerManager, id int, started chan<- int, release <-chan struct{}) timerTask {
	t.Helper()
	timer := &asyncPoolTestTimer{id: id, started: started, release: release}
	if err := timer.Timer.Init(1, InfiniteCount, timer); err != nil {
		t.Fatalf("init timer: %v", err)
	}
	return timerTask{timer: timer, gen: timer.gen.Load(), mgr: mgr}
}

func TestTimerIsAsyncTickDefaultFalse(t *testing.T) {
	if (&Timer{}).IsAsyncTick() {
		t.Fatal("base Timer must retain synchronous execution by default")
	}
	var _ TimerInterface = (*asyncPoolTestTimer)(nil)
}

// TestAsyncWorkerPoolExpandsToLimitAndQueueIsUnbounded 校验 worker 上限与无界队列：
// 两个回调用各自的 release 钉住 worker，后续任务只能排队，且不得被容量拒绝。
func TestAsyncWorkerPoolExpandsToLimitAndQueueIsUnbounded(t *testing.T) {
	mgr := newRawWheelManager()
	started := make(chan int, 2)
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	pool := newAsyncTickPool(2)

	if !pool.submit(newAsyncPoolTestTimer(t, mgr, 1, started, release1)) {
		t.Fatal("submit first job")
	}
	if got := <-started; got != 1 {
		t.Fatalf("first started job = %d, want 1", got)
	}
	if !pool.submit(newAsyncPoolTestTimer(t, mgr, 2, started, release2)) {
		t.Fatal("submit second job")
	}
	if got := <-started; got != 2 {
		t.Fatalf("second started job = %d, want 2", got)
	}

	queuedRelease := make(chan struct{})
	for i := 0; i < 100; i++ {
		if !pool.submit(newAsyncPoolTestTimer(t, mgr, i+3, nil, queuedRelease)) {
			t.Fatal("unbounded queue rejected a job")
		}
	}
	stats := pool.stats()
	if stats.Workers != 2 || stats.Queued != 100 || stats.InFlight != 2 {
		t.Fatalf("stats = %+v, want workers=2 queued=100 inFlight=2", stats)
	}

	pool.stopAccepting()
	close(release1)
	close(release2)
	close(queuedRelease)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("workers did not exit after drain: %+v", pool.stats())
	}
	if stats := pool.stats(); stats != (AsyncWorkerStats{Limit: 2}) {
		t.Fatalf("stats after drain = %+v", stats)
	}
}

// TestAsyncWorkerPoolSingleWorkerQueueHasNoLengthCap 单 worker 下 256 条任务
// 必须全部排队而不是被拒绝。
func TestAsyncWorkerPoolSingleWorkerQueueHasNoLengthCap(t *testing.T) {
	mgr := newRawWheelManager()
	started := make(chan int, 1)
	release := make(chan struct{})
	pool := newAsyncTickPool(1)

	if !pool.submit(newAsyncPoolTestTimer(t, mgr, 1, started, release)) {
		t.Fatal("submit running job")
	}
	<-started
	queuedRelease := make(chan struct{})
	for id := 0; id < 256; id++ {
		if !pool.submit(newAsyncPoolTestTimer(t, mgr, id+2, nil, queuedRelease)) {
			t.Fatal("unbounded queue rejected a job")
		}
	}
	stats := pool.stats()
	if stats.Workers != 1 || stats.Queued != 256 || stats.InFlight != 1 {
		t.Fatalf("stats = %+v, want workers=1 queued=256 inFlight=1", stats)
	}

	pool.stopAccepting()
	close(release)
	close(queuedRelease)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("worker did not exit: %+v", pool.stats())
	}
}

// TestAsyncWorkerPoolStats 校验执行中、parked 与空闲三种状态的口径。
func TestAsyncWorkerPoolStats(t *testing.T) {
	mgr := newRawWheelManager()
	pool := newAsyncTickPool(2)
	started := make(chan int, 1)
	release := make(chan struct{})
	task := newAsyncPoolTestTimer(t, mgr, 1, started, release)

	if !pool.submit(task) {
		t.Fatal("submit job")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async timer did not start")
	}
	if stats := pool.stats(); stats != (AsyncWorkerStats{Limit: 2, Workers: 1, Queued: 0, InFlight: 1}) {
		t.Fatalf("running stats = %+v", stats)
	}

	// 同一 Timer 的后续任务只进 parked，不占 worker。
	if !pool.submit(task) {
		t.Fatal("submit parked job")
	}
	if stats := pool.stats(); stats != (AsyncWorkerStats{Limit: 2, Workers: 1, Queued: 1, InFlight: 1}) {
		t.Fatalf("parked stats = %+v", stats)
	}

	pool.stopAccepting()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("worker did not exit: %+v", pool.stats())
	}
}

type asyncSingleFlightTimer struct {
	Timer
	active  atomic.Int32
	max     atomic.Int32
	started chan struct{}
	release <-chan struct{}
}

func (t *asyncSingleFlightTimer) Tick() {
	active := t.active.Add(1)
	for {
		max := t.max.Load()
		if active <= max || t.max.CompareAndSwap(max, active) {
			break
		}
	}
	t.started <- struct{}{}
	<-t.release
	t.active.Add(-1)
}

func (t *asyncSingleFlightTimer) IsAsyncTick() bool { return true }

// TestAsyncWorkerPoolNeverRunsSameTimerConcurrently 同一 Timer 连续两条任务
// 不得重叠执行；第二条即使因代次过期被丢弃，执行位也必须归还。
func TestAsyncWorkerPoolNeverRunsSameTimerConcurrently(t *testing.T) {
	mgr := newRawWheelManager()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	timer := &asyncSingleFlightTimer{started: started, release: release}
	if err := timer.Timer.Init(1, InfiniteCount, timer); err != nil {
		t.Fatal(err)
	}
	task := timerTask{timer: timer, gen: timer.gen.Load(), mgr: mgr}
	pool := newAsyncTickPool(2)
	if !pool.submit(task) || !pool.submit(task) {
		t.Fatal("submit repeated timer jobs")
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first same-timer callback did not start")
	}
	select {
	case <-started:
		t.Fatal("same Timer callbacks overlapped")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	pool.stopAccepting()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("workers did not exit: %+v", pool.stats())
	}
	if got := timer.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent same-timer callbacks = %d, want 1", got)
	}
}

// TestAsyncWorkerWaitsForSynchronousSameTimerTick 同步收尾路径持有令牌时，
// 异步 worker 必须等它结束，而不是并行执行同一个 Timer。
func TestAsyncWorkerWaitsForSynchronousSameTimerTick(t *testing.T) {
	mgr := newRawWheelManager()
	started := make(chan int, 1)
	release := make(chan struct{})
	task := newAsyncPoolTestTimer(t, mgr, 1, started, release)
	parent := task.timer.GetParent()
	if !parent.beginTick(task.gen) {
		t.Fatal("failed to simulate a synchronous callback")
	}
	key := asyncTimerKey{parent: parent, epoch: parent.poolEpoch.Load()}
	flightRelease := acquireTimerFlight(key)

	pool := newAsyncTickPool(1)
	if !pool.submit(task) {
		t.Fatal("submit async job")
	}
	select {
	case <-started:
		t.Fatal("async callback started before synchronous callback ended")
	case <-time.After(20 * time.Millisecond):
	}

	parent.endTick()
	flightRelease()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async callback did not start after synchronous callback ended")
	}

	close(release)
	pool.stopAccepting()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("worker did not exit: %+v", pool.stats())
	}
}

// TestAsyncWorkerPoolSubmissionAndExitAreSerialized 并发提交与 worker 退出必须
// 串行化：stopAccepting 之后不再接收，但已入队的任务要跑完。
func TestAsyncWorkerPoolSubmissionAndExitAreSerialized(t *testing.T) {
	mgr := newRawWheelManager()
	pool := newAsyncTickPool(1)
	const jobs = 100
	var wg sync.WaitGroup
	wg.Add(jobs)
	for i := 0; i < jobs; i++ {
		job := newAsyncPoolTestTimer(t, mgr, i, nil, nil)
		go func() {
			defer wg.Done()
			if !pool.submit(job) {
				t.Error("submit rejected before pool shutdown")
			}
		}()
	}
	wg.Wait()

	pool.stopAccepting()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatalf("worker did not exit after concurrent submissions: %+v", pool.stats())
	}
	if stats := pool.stats(); stats != (AsyncWorkerStats{Limit: 1}) {
		t.Fatalf("stats after drain = %+v", stats)
	}
}

type asyncRuntimeTestTimer struct {
	Timer
	started       chan<- struct{}
	release       <-chan struct{}
	panicOnReturn bool
}

func (t *asyncRuntimeTestTimer) IsAsyncTick() bool { return true }

func (t *asyncRuntimeTestTimer) Tick() {
	t.started <- struct{}{}
	<-t.release
	if t.panicOnReturn {
		panic("async timer test panic")
	}
}

func prepareAsyncRuntimeTest(t *testing.T, workerLimit int) (context.CancelFunc, func()) {
	t.Helper()
	TimeStop(context.Background())
	if got := SetScheduleShards(1); got != 1 {
		t.Fatalf("schedule shards = %d, want 1", got)
	}
	SetAsyncWorkerLimit(workerLimit)
	ctx, cancel := context.WithCancel(context.Background())
	Run(ctx)
	cleanup := func() {
		cancel()
		TimeStop(context.Background())
		SetAsyncWorkerLimit(DefaultAsyncWorkerLimit)
		SetScheduleShards(DefaultScheduleShards)
	}
	return cancel, cleanup
}

func TestAsyncTimerDoesNotBlockShardConsumer(t *testing.T) {
	cancel, cleanup := prepareAsyncRuntimeTest(t, 2)

	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce [2]sync.Once
	defer func() {
		for i := range release {
			releaseOnce[i].Do(func() { close(release[i]) })
		}
		cancel()
		cleanup()
	}()
	started := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
	timers := make([]*asyncRuntimeTestTimer, 2)
	for i := range timers {
		index := i
		timer, err := NewTimer[*asyncRuntimeTestTimer](5, 1, func(timer *asyncRuntimeTestTimer) {
			timer.started = started[index]
			timer.release = release[index]
		})
		if err != nil {
			t.Fatal(err)
		}
		timers[i] = timer
		if err := AddTimer(timer); err != nil {
			t.Fatal(err)
		}
	}

	for i := range started {
		select {
		case <-started[i]:
		case <-time.After(2 * time.Second):
			t.Fatalf("async timer %d did not start while sibling callback was blocked", i)
		}
	}

	for i := range release {
		releaseOnce[i].Do(func() { close(release[i]) })
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		mgr := GetDefaultManager()
		if mgr != nil && mgr.GetTimerCount() == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("async timers were not finalized after their callbacks returned")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAsyncTimerRemoveDefersPoolReturn(t *testing.T) {
	cancel, cleanup := prepareAsyncRuntimeTest(t, 1)

	release := make(chan struct{})
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		cleanup()
	}()
	started := make(chan struct{}, 1)
	timer, err := NewTimer[*asyncRuntimeTestTimer](5, InfiniteCount, func(timer *asyncRuntimeTestTimer) {
		timer.started = started
		timer.release = release
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := timer.GetParent()
	if err := AddTimer(timer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async timer did not start")
	}

	before := GetTimerPoolStats().Puts
	timer.Remove()
	if got := GetTimerPoolStats().Puts; got != before {
		t.Fatalf("timer returned to pool while Tick was running: puts %d -> %d", before, got)
	}
	if got := parent.inTick.Load(); got != 1 {
		t.Fatalf("inTick = %d while callback blocked, want 1", got)
	}

	releaseOnce.Do(func() { close(release) })
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for GetTimerPoolStats().Puts == before {
		select {
		case <-deadline.C:
			t.Fatal("deferred timer was not returned to the pool")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestAsyncTickPanicDoesNotStopWorkerOrConsumer(t *testing.T) {
	_, cleanup := prepareAsyncRuntimeTest(t, 2)
	defer cleanup()

	started := []chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)}
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var releaseOnce [2]sync.Once
	for i := range release {
		index := i
		t.Cleanup(func() { releaseOnce[index].Do(func() { close(release[index]) }) })
	}
	for i := range started {
		index := i
		timer, err := NewTimer[*asyncRuntimeTestTimer](5, 1, func(timer *asyncRuntimeTestTimer) {
			timer.started = started[index]
			timer.release = release[index]
			timer.panicOnReturn = index == 0
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(timer); err != nil {
			t.Fatal(err)
		}
	}
	for i := range started {
		select {
		case <-started[i]:
		case <-time.After(2 * time.Second):
			t.Fatalf("timer %d did not start", i)
		}
	}
	for i := range release {
		releaseOnce[i].Do(func() { close(release[i]) })
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		mgr := GetDefaultManager()
		if mgr != nil && mgr.GetTimerCount() == 0 && mgr.GetStats().TimersExecuted >= 2 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("panic isolation or async finalization stalled")
		case <-time.After(time.Millisecond):
		}
	}
}

// TestAsyncTickDrainsAfterTimeStopBudgetExpires 停机预算耗尽时不得强杀在飞回调；
// 回调结束后必须由 worker 自己完成作废与归还。
func TestAsyncTickDrainsAfterTimeStopBudgetExpires(t *testing.T) {
	_, cleanup := prepareAsyncRuntimeTest(t, 1)
	defer cleanup()

	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{}, 1)
	timer, err := NewTimer[*asyncRuntimeTestTimer](5, InfiniteCount, func(timer *asyncRuntimeTestTimer) {
		timer.started = started
		timer.release = release
	})
	if err != nil {
		t.Fatal(err)
	}
	parent := timer.GetParent()
	if err := AddTimer(timer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("async timer did not start")
	}

	before := GetTimerPoolStats().Puts
	rt := timerRT.Load()
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	TimeStop(stopCtx)
	cancelStop()
	if got := parent.inTick.Load(); got != 1 {
		t.Fatalf("inTick after timed-out stop = %d, want 1", got)
	}
	if got := GetTimerPoolStats().Puts; got != before {
		t.Fatalf("timer returned to pool before its Tick finished: puts %d -> %d", before, got)
	}

	releaseOnce.Do(func() { close(release) })
	if rt == nil || rt.asyncPool == nil {
		t.Fatal("runtime async pool was not initialized")
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelWait()
	if !rt.asyncPool.wait(waitCtx) {
		t.Fatal("worker did not drain after callback was released")
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for GetTimerPoolStats().Puts == before {
		select {
		case <-deadline.C:
			t.Fatal("closed manager did not release the completed timer")
		case <-time.After(time.Millisecond):
		}
	}
}

// asyncReconnectTimer 在第一次 Tick 内重新注册自己，模拟业务在异步回调里重连。
type asyncReconnectTimer struct {
	Timer
	n      atomic.Int32
	active atomic.Int32
	max    atomic.Int32
}

func (t *asyncReconnectTimer) IsAsyncTick() bool { return true }

func (t *asyncReconnectTimer) Tick() {
	active := t.active.Add(1)
	for {
		max := t.max.Load()
		if active <= max || t.max.CompareAndSwap(max, active) {
			break
		}
	}
	n := t.n.Add(1)
	if n == 1 {
		// 新调度项的代次与当前任务不同：本次收尾会作废，后续任务进 parked 等待执行。
		_ = AddTimer(t)
	}
	t.active.Add(-1)
}

// TestAsyncTickReconnectKeepsScheduling 异步 Tick 内 AddTimer 复活后，后续任务
// 必须被 parked 接住并继续执行，不能因为当前收尾作废而永久失联。
func TestAsyncTickReconnectKeepsScheduling(t *testing.T) {
	_, cleanup := prepareAsyncRuntimeTest(t, 1)
	defer cleanup()

	timer, err := NewTimer[*asyncReconnectTimer](5, InfiniteCount, func(t *asyncReconnectTimer) {
		// 池复用时宿主字段会原样带过来，必须在这里清回初值。
		t.n.Store(0)
		t.active.Store(0)
		t.max.Store(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(timer); err != nil {
		t.Fatal(err)
	}
	defer timer.Remove()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for timer.n.Load() < 3 {
		select {
		case <-deadline.C:
			t.Fatalf("async reconnect lost ticks: n=%d", timer.n.Load())
		case <-time.After(time.Millisecond):
		}
	}
	if got := timer.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent async callbacks = %d, want 1", got)
	}
}
