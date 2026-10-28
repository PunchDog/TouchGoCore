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

func newAsyncPoolTestTimer(t *testing.T, mgr *TimerManager, id int, started chan<- int, release <-chan struct{}) asyncTickJob {
	t.Helper()
	timer := &asyncPoolTestTimer{id: id, started: started, release: release}
	if err := timer.Timer.Init(1, InfiniteCount, timer); err != nil {
		t.Fatalf("init timer: %v", err)
	}
	parent := timer.GetParent()
	return asyncTickJob{
		task:      timerTask{timer: timer, gen: timer.gen.Load(), mgr: mgr},
		shard:     0,
		flightKey: asyncTimerKey{parent: parent, epoch: parent.poolEpoch.Load()},
	}
}

func ackAsyncCompletion(completion asyncTickCompletion) {
	completion.parent.endTick()
	if completion.releaseFlight != nil {
		completion.releaseFlight()
	}
	if completion.pool != nil {
		completion.pool.releaseTimer(completion.flightKey)
	}
	completion.ack <- struct{}{}
}

func receiveAsyncCompletion(t *testing.T, pool *asyncTickPool) asyncTickCompletion {
	t.Helper()
	select {
	case completion := <-pool.resultsForShard(0):
		return completion
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async completion")
		return asyncTickCompletion{}
	}
}

func waitAsyncPending(t *testing.T, pool *asyncTickPool, want int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		if got := pool.pendingForShard(0); got == want {
			return
		}
		select {
		case <-pool.progressForShard(0):
		case <-timer.C:
			t.Fatalf("pending jobs = %d, want %d", pool.pendingForShard(0), want)
		}
	}
}

func TestTimerIsAsyncTickDefaultFalse(t *testing.T) {
	if (&Timer{}).IsAsyncTick() {
		t.Fatal("base Timer must retain synchronous execution by default")
	}
	var _ TimerInterface = (*asyncPoolTestTimer)(nil)
}

func TestAsyncWorkerPoolExpandsAndUsesShortestUnboundedQueue(t *testing.T) {
	mgr := &TimerManager{}
	started := make(chan int, 2)
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	pool := newAsyncTickPool(2, 1)

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
	queuedJobs := make([]asyncTickJob, 100)
	for i := range queuedJobs {
		queuedJobs[i] = newAsyncPoolTestTimer(t, mgr, i+3, nil, queuedRelease)
	}
	pool.mu.Lock()
	workerCount := len(pool.workers)
	pool.mu.Unlock()
	if workerCount != 2 {
		t.Fatalf("worker count = %d, want 2", workerCount)
	}

	var submitWG sync.WaitGroup
	submitWG.Add(len(queuedJobs))
	for _, queuedJob := range queuedJobs {
		job := queuedJob
		go func() {
			defer submitWG.Done()
			if !pool.submit(job) {
				t.Error("submit queued job")
			}
		}()
	}
	submitWG.Wait()
	pool.mu.Lock()
	queued := 0
	for _, worker := range pool.workers {
		queued += len(worker.queue)
	}
	pool.mu.Unlock()
	if queued != 100 {
		t.Fatalf("queued jobs = %d, want 100", queued)
	}

	pool.stopAccepting()
	close(release1)
	close(release2)
	close(queuedRelease)
	for range 102 {
		ackAsyncCompletion(receiveAsyncCompletion(t, pool))
	}
	waitAsyncPending(t, pool, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !pool.wait(ctx) {
		t.Fatal("workers did not exit after their queues drained")
	}
	pool.mu.Lock()
	remainingWorkers := len(pool.workers)
	pool.mu.Unlock()
	if remainingWorkers != 0 {
		t.Fatalf("worker count after drain = %d, want 0", remainingWorkers)
	}
}

func TestAsyncWorkerPoolSelectsShortestQueue(t *testing.T) {
	pool := &asyncTickPool{
		limit:   2,
		pending: []int{6},
		results: []chan asyncTickCompletion{make(chan asyncTickCompletion)},
		workers: []*asyncTickWorker{
			{queue: make([]asyncTickJob, 4)},
			{queue: make([]asyncTickJob, 1)},
		},
		busy:     make(map[asyncTimerKey]bool),
		deferred: make(map[asyncTimerKey][]asyncTickJob),
	}
	timer := &asyncPoolTestTimer{}
	if err := timer.Timer.Init(1, InfiniteCount, timer); err != nil {
		t.Fatal(err)
	}
	parent := timer.GetParent()
	if !pool.submit(asyncTickJob{
		task:      timerTask{timer: timer, gen: timer.gen.Load(), mgr: &TimerManager{}},
		shard:     0,
		flightKey: asyncTimerKey{parent: parent, epoch: parent.poolEpoch.Load()},
	}) {
		t.Fatal("submit job to saturated pool")
	}
	if got := len(pool.workers[0].queue); got != 4 {
		t.Fatalf("non-shortest queue length = %d, want 4", got)
	}
	if got := len(pool.workers[1].queue); got != 2 {
		t.Fatalf("shortest queue length = %d, want 2", got)
	}
}

func TestAsyncWorkerPoolStats(t *testing.T) {
	pool := newAsyncTickPool(3, 2)
	pool.pending[0] = 6
	pool.workers = []*asyncTickWorker{
		{queue: []asyncTickJob{{}, {}}},
		{queue: []asyncTickJob{{}}},
	}
	key := asyncTimerKey{parent: &Timer{}, epoch: 1}
	pool.busy[key] = true
	pool.deferred[key] = []asyncTickJob{{}, {}}
	stats := pool.stats()
	if stats != (AsyncWorkerStats{Limit: 3, Workers: 2, Queued: 5, InFlight: 1}) {
		t.Fatalf("stats = %+v", stats)
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

func TestAsyncWorkerPoolSerializesSameTimer(t *testing.T) {
	mgr := &TimerManager{}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	timer := &asyncSingleFlightTimer{started: started, release: release}
	if err := timer.Timer.Init(1, InfiniteCount, timer); err != nil {
		t.Fatal(err)
	}
	job := asyncTickJob{task: timerTask{timer: timer, gen: timer.gen.Load(), mgr: mgr}}
	pool := newAsyncTickPool(2, 1)
	if !pool.submit(job) || !pool.submit(job) {
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
	first := receiveAsyncCompletion(t, pool)
	ackAsyncCompletion(first)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("deferred same-timer job was lost")
	}
	second := receiveAsyncCompletion(t, pool)
	ackAsyncCompletion(second)
	waitAsyncPending(t, pool, 0)
	if got := timer.max.Load(); got != 1 {
		t.Fatalf("maximum concurrent same-timer callbacks = %d, want 1", got)
	}
	pool.stopAccepting()
	if !pool.wait(context.Background()) {
		t.Fatal("workers did not stop")
	}
}

func TestAsyncWorkerWaitsForSynchronousSameTimerTick(t *testing.T) {
	mgr := &TimerManager{}
	started := make(chan int, 1)
	release := make(chan struct{})
	job := newAsyncPoolTestTimer(t, mgr, 1, started, release)
	parent := job.task.timer.GetParent()
	if !parent.beginTick(job.task.gen) {
		t.Fatal("failed to simulate a synchronous callback")
	}
	flightRelease := acquireTimerFlight(job.flightKey)

	pool := newAsyncTickPool(1, 1)
	if !pool.submit(job) {
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
	ackAsyncCompletion(receiveAsyncCompletion(t, pool))
	waitAsyncPending(t, pool, 0)
	pool.stopAccepting()
	if !pool.wait(context.Background()) {
		t.Fatal("worker did not exit")
	}
}

func TestAsyncWorkerPoolSingleWorkerQueueHasNoLengthCap(t *testing.T) {
	mgr := &TimerManager{}
	started := make(chan int, 1)
	release := make(chan struct{})
	pool := newAsyncTickPool(1, 1)

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
	pool.mu.Lock()
	workers := len(pool.workers)
	queued := len(pool.workers[0].queue)
	pool.mu.Unlock()
	if workers != 1 || queued != 256 {
		t.Fatalf("workers=%d queued=%d, want 1 and 256", workers, queued)
	}

	pool.stopAccepting()
	close(release)
	ackAsyncCompletion(receiveAsyncCompletion(t, pool))
	close(queuedRelease)
	for range 256 {
		ackAsyncCompletion(receiveAsyncCompletion(t, pool))
	}
	waitAsyncPending(t, pool, 0)
	if !pool.wait(context.Background()) {
		t.Fatal("worker did not exit")
	}
}

func TestAsyncWorkerPoolSubmissionAndExitAreSerialized(t *testing.T) {
	pool := newAsyncTickPool(1, 1)
	mgr := &TimerManager{}
	const jobs = 100
	jobsToSubmit := make([]asyncTickJob, jobs)
	for i := range jobsToSubmit {
		jobsToSubmit[i] = newAsyncPoolTestTimer(t, mgr, i, nil, nil)
	}
	var wg sync.WaitGroup
	wg.Add(jobs)
	for i := range jobsToSubmit {
		job := jobsToSubmit[i]
		go func() {
			defer wg.Done()
			if !pool.submit(job) {
				t.Error("submit rejected before pool shutdown")
			}
		}()
	}
	wg.Wait()
	pool.stopAccepting()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for pool.pendingForShard(0) > 0 {
		select {
		case completion := <-pool.resultsForShard(0):
			ackAsyncCompletion(completion)
		case <-pool.progressForShard(0):
		case <-deadline.C:
			t.Fatalf("pending jobs after concurrent submit = %d", pool.pendingForShard(0))
		}
	}
	if !pool.wait(context.Background()) {
		t.Fatal("worker did not exit after concurrent submissions")
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
			t.Fatal("panic isolation or async completion processing stalled")
		case <-time.After(time.Millisecond):
		}
	}
}

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
	waitAsyncPending(t, rt.asyncPool, 0)
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
