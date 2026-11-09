package localtimer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/list"
	"touchgocore/util"
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

// newTestRing 造与生产同规格的桶环：按精度在 defaultWheelConfigs 里找档序，
// 桶数由同一张精度表推导，避免测试自造一套「只够测试用」的桶数而测不到真实形状。
func newTestRing(config int64) *slotRing {
	for i, c := range defaultWheelConfigs {
		if c == config {
			return newSlotRing(config, slotCountFor(defaultWheelConfigs, i))
		}
	}
	return newSlotRing(config, 64)
}

// ============================================================================
// 引用计数 -1 缺陷修复的回归验证。
//
// 修复 1（timer_manager.go AddTimer）：原先 parent.gen.Add(1) 丢弃返回值、
//   入队前再 parent.gen.Load()，两个并发 AddTimer 可读到同一 gen，产生两份
//   相同 gen 的 task，handleTimerAdd 的 isValid 全部放行 → 同一节点二次入链、
//   wheel.timerCount 多加，后续配对减完出现 -1。修复后直接使用 Add 返回值。
//
// 修复 2（timer.go HasNext）：原先 count==0 时无条件 Add(-1) 并存回，
//   GetRemainingCount() 从此返回 -1（触发场景：Init(interval, 0, ...)、
//   SetCount(0)、count 耗尽后业务重新 AddTimer 复活）。修复后 CAS 扣减，
//   c <= 0 直接返回 false 不再扣；count==1 扣到 0 返回 false 的语义不变。
//
// 运行方式：
//   go test ./localtimer -run 'TestRegression_HasNext|TestRegression_ConcurrentAddRemove' -v
//   go test -race -count=1 ./localtimer/
// ============================================================================

// ----------------------------------------------------------------------------
// 测试 A（确定性，针对修复 2）：count==0 时 HasNext 不得把 -1 存回。
// ----------------------------------------------------------------------------

// TestRegression_HasNext_ZeroCountNeverGoesNegative 覆盖三类触发场景：
//  1. Init(interval, 0, ...) 直接以 0 次数创建；
//  2. SetCount(0) 把运行中的定时器次数清零；
//  3. count==1 扣到 0 后返回 false 的原语义保持不变。
func TestRegression_HasNext_ZeroCountNeverGoesNegative(t *testing.T) {
	// 场景 1：Init(interval, 0, ...)
	tm0, err := NewTimer[*plainTimer](100, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm0.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ Init(count=0) 后剩余次数应为 0，实际 %d", got)
	}
	if tm0.HasNext() {
		t.Fatal("✘ count=0 的定时器 HasNext 应返回 false")
	}
	if got := tm0.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ count=0 调用 HasNext 后剩余次数被扣成 %d（期望仍为 0，修复前为 -1）", got)
	}

	// 场景 2：SetCount(0) 复验
	tm1, err := NewTimer[*plainTimer](100, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm1.SetCount(0)
	if got := tm1.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ SetCount(0) 后剩余次数应为 0，实际 %d", got)
	}
	if tm1.HasNext() {
		t.Fatal("✘ SetCount(0) 后 HasNext 应返回 false")
	}
	if got := tm1.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ SetCount(0) 后调用 HasNext 剩余次数被扣成 %d（期望仍为 0）", got)
	}

	// 场景 3：count==1 扣到 0 返回 false，原语义不变
	tm2, err := NewTimer[*plainTimer](100, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tm2.HasNext() {
		t.Fatal("✘ count=1 的定时器 HasNext 应扣减至 0 并返回 false")
	}
	if got := tm2.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ count=1 扣减后剩余次数应为 0，实际 %d", got)
	}

	// 场景 4：无限次数定时器不受影响，仍返回 InfiniteCount 且 HasNext 为 true
	tm3, err := NewTimer[*plainTimer](100, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm3.GetRemainingCount(); got != InfiniteCount {
		t.Fatalf("✘ 无限次数定时器剩余次数应为 %d，实际 %d", InfiniteCount, got)
	}
	if !tm3.HasNext() {
		t.Fatal("✘ 无限次数定时器 HasNext 应返回 true")
	}

	t.Log("✔ count=0 / SetCount(0) 时 HasNext 不再产生 -1，count==1 原语义保持")
}

// TestRegression_HasNext_ConcurrentDecrement_NeverNegative
// 并发 HasNext 竞争扣减：总扣减量不超过初始 count，剩余次数永不为负。
// （修复前的无条件 Add(-1) 在耗尽后会把 -1 存回。）
func TestRegression_HasNext_ConcurrentDecrement_NeverNegative(t *testing.T) {
	const initial = 100
	tm, err := NewTimer[*plainTimer](100, initial, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = tm.HasNext()
			}
		}()
	}
	wg.Wait()

	if got := tm.GetRemainingCount(); got < 0 {
		t.Fatalf("✘ 并发 HasNext 后剩余次数为负: %d", got)
	}
	t.Logf("✔ 并发扣减后剩余次数 %d（>= 0，总扣减未越界）", tm.GetRemainingCount())
}

// ----------------------------------------------------------------------------
// 测试 B（并发压力，针对修复 1）：AddTimer / Pause 并发竞态下
// GetTimerCount 恒 >= 0 且最终收敛为 0，无 panic、无死锁。
// ----------------------------------------------------------------------------

// TestRegression_ConcurrentAddRemoveChurn_TimerCountNeverNegative
//
// 模拟「TimeTick 续期协程 + 业务协程先 Pause 再 AddTimer 复活」的竞态：
// 每轮创建一个毫秒级无限次数定时器，两个 goroutine 分别对同一实例持续
// AddTimer 与 Pause+AddTimer，持续一段时间后停表并作废，断言：
//   - 压测全程 GetTimerCount() >= 0（由看门狗协程持续采样）；
//   - 每轮结束后计数收敛归零；
//   - 无 panic（goroutine panic 会直接令测试失败）、无死锁（外层 -timeout 保护，
//     且每轮清理带 deadline 轮询）。
//
// 说明：这里必须用 Pause 而不是 Remove —— Remove 即彻底作废回池，后续 AddTimer
// 一律被拒，压测会退化成「什么都不发生」，覆盖不到入队/摘链的竞态窗口。
//
// 说明：gen 重复入链需要毫秒级时间窗对齐，直接断言「复现 -1」在修复后不可行；
// 本用例采用「长时间高频压测下计数恒非负且收敛为 0」的验证口径。
func TestRegression_ConcurrentAddRemoveChurn_TimerCountNeverNegative(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	// 计数看门狗：全程采样 GetTimerCount，记录最小值并标记是否出现过负数
	var (
		minCount  atomic.Int64
		negative  atomic.Bool
		stopWatch = make(chan struct{})
		watchDone = make(chan struct{})
	)
	minCount.Store(math.MaxInt64)
	go func() {
		defer close(watchDone)
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			c := m.GetTimerCount()
			if c < 0 {
				negative.Store(true)
			}
			for {
				old := minCount.Load()
				if c >= old || minCount.CompareAndSwap(old, c) {
					break
				}
			}
			time.Sleep(500 * time.Microsecond)
		}
	}()

	// 一轮 100ms，200 轮 ≈ 20s；-short 下压到 20 轮，仍覆盖到入队/摘链竞态窗口，
	// 只是把「长时间」换成「较短时间」，避免整条用例在快速回归里缺席。
	rounds := 200 // 轮数（>=200）
	if testing.Short() {
		rounds = 20
	}
	const (
		churn      = 100 * time.Millisecond
		cleanupMax = 3 * time.Second
	)

	for r := 0; r < rounds; r++ {
		tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		// 协程 1：模拟 TimeTick 续期路径的 AddTimer（不含 Remove）
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = AddTimer(tm) // 通道满丢弃属正常背压
				}
			}
		}()
		// 协程 2：模拟业务重启路径的 Pause + AddTimer 复活
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					tm.Pause()
					_ = AddTimer(tm)
				}
			}
		}()

		time.Sleep(churn)
		close(stop)
		wg.Wait()

		// 停表并轮询等待计数收敛归零（带 deadline，防死锁式挂起）。
		// 循环里只能 Pause：Remove 一次即把实例交还对象池，再 Remove 就是
		// 对可能已易主的实例做悬垂操作。
		deadline := time.Now().Add(cleanupMax)
		for {
			tm.Pause()
			if m.GetTimerCount() == 0 {
				tm.Remove() // 正式作废，实例回池供下一轮复用
				break
			}
			if time.Now().After(deadline) {
				close(stopWatch)
				<-watchDone
				// 诊断：逐轮 dump 计数与链表长度，定位是「多减」还是「少加」
				var detail string
				for wi, w := range m.wheels {
					detail += fmt.Sprintf("wheel[%d]=%s count=%d len=%d; ",
						wi, TimerType(wi), w.timerCount.Load(), w.tickWheel.Length())
				}
				t.Fatalf("✘ 第 %d 轮结束后定时器计数未归零: %d | stats=%+v | %s",
					r+1, m.GetTimerCount(), m.GetStats(), detail)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	close(stopWatch)
	<-watchDone

	if negative.Load() {
		t.Fatalf("✘ 压测期间 GetTimerCount 出现负数（最小值 %d）", minCount.Load())
	}
	if c := m.GetTimerCount(); c != 0 {
		t.Fatalf("✘ 测试结束后计数未归零: %d", c)
	}
	t.Logf("✔ %d 轮高频 AddTimer/Pause 竞态压测：计数全程最小值 %d（>=0），最终归零",
		rounds, minCount.Load())
}

// ============================================================================
// localtimer 宕机问题修复后的回归验证。
//
// 历史问题（修复前实测可复现，详见 git 历史）：
//   1. TimeStop → cleanupWheel 持 wheelLock 同步执行 Tick，Tick 内 Remove /
//      AddTimer 会重入同一把锁 → 进程永久挂死；
//   2. 未 Run（或停止后）直接 NewTimerManager → nil timerManagerMap panic；
//   3. Range 内「摘除 + 回加」的节点被延迟删除误删 → 定时器永久丢失；
//   4. 对象池复用同一实例 → 两个逻辑定时器 UID 相同、互相串扰；
//   5. 业务协程 Remove 与调度协程 Tick/HasNext/AddTimer 的数据竞争。
//
// 致命场景（1、2）会被 runtime 直接杀死/挂死，recover 无效，因此用
// 「子进程重入自身测试二进制」运行，依据子进程是否超时/崩溃来判定。
//
// 运行方式：
//   go test ./localtimer -run 'TestRegression_' -v
//   CGO_ENABLED=1 go test ./localtimer -run TestRace_ -race -v
//   go test ./localtimer -short     # 快跑：跳过子进程宕机回归，其余照常
// ============================================================================

const crashChildEnv = "TG_LOCALTIMER_CRASH_CHILD"

// runCrashChild 在子进程中单独运行指定测试函数，返回子进程输出与是否超时（挂死）。
//
// 门控集中在这一处：子进程要重跑一遍测试二进制、且各自带 20~25s 的等待预算，
// 快跑（go test -short）只想要功能结论时不必付这份墙钟；致命场景的验证留给完整 CI。
func runCrashChild(t *testing.T, testName string, wait time.Duration) (out string, timedOut bool) {
	t.Helper()

	if testing.Short() {
		t.Skip("短跑模式跳过子进程宕机回归（需完整 CI 运行）")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), crashChildEnv+"=1")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(wait):
		_ = cmd.Process.Kill()
		<-done
		timedOut = true
	}
	return buf.String(), timedOut
}

func isChild() bool { return os.Getenv(crashChildEnv) == "1" }

// ============================================================================
// 场景 1（回归）：退出流程不得死锁。
//
//	cleanupWheel 必须在释放 wheelLock 之后才执行业务 Tick，
//	否则 Tick 内调用 Remove / AddTimer 会重入同一把锁，TimeStop 永久挂起。
// ============================================================================

type selfRemoveTimer struct {
	Timer
	ticked atomic.Int64
}

func (s *selfRemoveTimer) Tick() {
	s.ticked.Add(1)
	// 业务最常见的写法：跑完把自己摘掉。同时覆盖「回调在飞时收到作废请求」
	// 这条路径 —— 归还必须延迟到 endTick，不能把正在执行回调的对象交给新主人。
	s.Remove()
}

func scenarioCloseNoDeadlock() {
	// 看门狗：超时后 dump 全部 goroutine 栈并退出，用于定位死锁发生的确切位置
	go func() {
		time.Sleep(5 * time.Second)
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "\n===== DEADLOCK DETECTED: goroutine dump =====\n%s\n", buf[:n])
		os.Exit(3)
	}()

	Run(context.Background())

	m := GetDefaultManager()
	tm, err := NewTimer[*selfRemoveTimer](10, 1, nil)
	if err != nil {
		return
	}

	// 放入小时轮（1 小时才 tick 一次），保证 Close 前不会被正常调度取走；
	// 同时把 nextTime 设为已过期，保证 cleanupWheel 一定会执行它的 Tick。
	p := tm.GetParent()
	p.nextTime.Store(util.CurrentMS() - 1)
	wheel := m.wheels[TimerTypeHour]
	p.wheel.Store(wheel)
	wheel.tickWheel.Add(tm)

	time.Sleep(50 * time.Millisecond)
	TimeStop(context.Background())
}

func TestRegression_CloseNoDeadlock_TickRemovesSelf(t *testing.T) {
	if isChild() {
		scenarioCloseNoDeadlock()
		return
	}

	out, timedOut := runCrashChild(t, "TestRegression_CloseNoDeadlock_TickRemovesSelf", 20*time.Second)
	if strings.Contains(out, "DEADLOCK DETECTED") {
		t.Fatalf("✘ 退出流程仍然存在死锁:\n%s", extractDeadlockStack(out))
	}
	if strings.Contains(out, "all goroutines are asleep - deadlock!") {
		t.Fatalf("✘ 退出流程死锁，进程被 runtime 杀死:\n%s", tailLines(out, 12))
	}
	if timedOut {
		t.Fatalf("✘ 退出流程未在 20s 内结束（疑似挂死），输出:\n%s", tailLines(out, 12))
	}
	t.Log("✔ TimeStop 正常返回，退出无死锁")
}

// ============================================================================
// 场景 2（回归）：未 Run / 停止后重建管理器不得 panic。
// ============================================================================

func scenarioNewManagerNoPanic() {
	// 故意不调用 Run，模拟「自建管理器 / 停止后重建管理器」的用法
	mgr := NewTimerManager()
	mgr.Close()
}

func TestRegression_NewTimerManager_NoPanic(t *testing.T) {
	if isChild() {
		scenarioNewManagerNoPanic()
		return
	}

	out, timedOut := runCrashChild(t, "TestRegression_NewTimerManager_NoPanic", 20*time.Second)
	if strings.Contains(out, "nil pointer dereference") || strings.Contains(out, "panic:") {
		t.Fatalf("✘ 仍然 panic:\n%s", tailLines(out, 20))
	}
	if timedOut {
		t.Fatalf("✘ 子进程挂死，输出:\n%s", tailLines(out, 12))
	}
	t.Log("✔ 未 Run 时创建管理器不再 panic")
}

// ============================================================================
// 场景 3（回归）：Range 内「摘除 + 回加」的节点必须保留。
//
//	processWheelTick 迁移失败时会在 Range 回调里把节点回加到当前时间轮，
//	list 必须撤销该节点的待删除标记，否则定时器会被静默丢弃。
// ============================================================================

func TestRegression_RangeRemoveThenAddKeepsTimer(t *testing.T) {
	l := list.NewList()
	n := &Timer{}
	if !l.Add(n) {
		t.Fatal("Add 失败")
	}

	l.Range(func(node list.INode) bool {
		node.GetNode().Remove() // processWheelTick：先摘
		l.Add(node)             // 迁移目标通道满，回加当前轮
		return true
	})

	if got := l.Length(); got != 1 {
		t.Fatalf("✘ 回加的节点被延迟删除误删，链表长度=%d（期望 1）", got)
	}
	t.Log("✔ Range 内回加的节点被正确保留")
}

// ============================================================================
// 场景 4（回归）：对象池允许复用实例，但每个逻辑定时器必须拿到独立 UID。
// ============================================================================

type plainTimer struct {
	Timer
	n atomic.Int64
}

func (p *plainTimer) Tick() { p.n.Add(1) }

func TestRegression_PoolReuseKeepsUniqueUID(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	a, err := NewTimer[*plainTimer](1000, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(a); err != nil {
		t.Fatal(err)
	}
	uidA := a.GetUID()

	a.Remove() // 彻底作废并归还对象池，b 很可能正好认领到这个实例

	b, err := NewTimer[*plainTimer](1000, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(b); err != nil {
		t.Fatal(err)
	}
	uidB := b.GetUID()

	if a == b {
		t.Logf("实例被对象池复用（正常），需保证身份隔离: a=b=%p", a)
	}
	if uidA == 0 || uidB == 0 || uidA == uidB {
		t.Fatalf("✘ UID 未隔离: uidA=%d uidB=%d", uidA, uidB)
	}
	t.Logf("✔ 对象池复用实例但 UID 独立: uidA=%d uidB=%d", uidA, uidB)
}

// ============================================================================
// 场景 5（回归）：已移除的定时器不得再被调度（幽灵定时器）。
//
//	代次机制必须让残留在通道中的在途调度项失效。
// ============================================================================

func TestRegression_RemovedTimerNotRescheduled(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](5, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	before := tm.n.Load()
	if before == 0 {
		t.Fatal("定时器未被执行，调度链路异常")
	}

	tm.Remove()
	time.Sleep(300 * time.Millisecond)

	if after := tm.n.Load(); after != before {
		t.Fatalf("✘ 已移除的定时器仍在执行: 移除前=%d 移除后=%d", before, after)
	}
	t.Logf("✔ 移除后不再执行（移除前执行 %d 次）", before)
}

// ============================================================================
// 场景 6（回归）：周期性定时器必须持续触发，且恢复后能重新调度。
// ============================================================================

func TestRegression_PeriodicTimerKeepsTicking(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](10, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	if n := tm.n.Load(); n < 3 {
		t.Fatalf("✘ 周期定时器执行次数过少: %d（期望 >=3）", n)
	} else {
		t.Logf("✔ 周期定时器正常执行 %d 次", n)
	}
}

// ============================================================================
// 场景 7（回归）：TimeStop 后可再次 Run，定时器恢复工作。
// ============================================================================

func TestRegression_RestartAfterTimeStop(t *testing.T) {
	Run(context.Background())
	TimeStop(context.Background())

	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](5, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := tm.n.Load(); n == 0 {
		t.Fatal("✘ 二次 Run 后定时器不再执行")
	}
	t.Log("✔ 停止后重启，定时器恢复正常")
}

// ============================================================================
// 场景 8（数据竞争，需 go test -race）：
//
//	telegram/rpc 等调用方会在「另一个 goroutine」里调用 timer.Pause()/Remove()，
//	而调度协程同时在 Tick → HasNext → AddTimer 上读写同一 Timer。
//	Timer 的状态字段已全部原子化，本用例在 -race 下不应报竞争。
//	这里用 Pause 而非 Remove：后者会把实例交还对象池，此后的 AddTimer 一律被拒，
//	覆盖不到「业务停表」与「调度续期」并发的真实窗口。
// ============================================================================

type raceTimer struct {
	Timer
	n atomic.Int64
}

func (r *raceTimer) Tick() { r.n.Add(1) }

func TestRace_RemoveFromOtherGoroutine(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*raceTimer](1, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tm.Pause()       // 业务侧并发停表
				time.Sleep(0)    // 放大竞态窗口
				_ = tm.HasNext() // 与调度协程争抢 count
				if err := AddTimer(tm); err == nil {
					_ = tm.GetParent().GetRemainingCount()
				}
			}
		}()
	}
	wg.Wait()
	tm.Remove() // 全部协程退出后才正式作废归还，避免对已归池实例做悬垂操作
}

// extractDeadlockStack 抽取 dump 中「卡在锁上」的关键栈帧，避免日志刷屏
func extractDeadlockStack(out string) string {
	var keep []string
	hit := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "DEADLOCK DETECTED") {
			hit = true
			continue
		}
		if !hit {
			continue
		}
		if strings.Contains(line, "goroutine ") ||
			strings.Contains(line, "localtimer.") ||
			strings.Contains(line, "sync.") ||
			strings.Contains(line, "list.") {
			keep = append(keep, strings.TrimSpace(line))
		}
	}
	if len(keep) == 0 {
		return tailLines(out, 12)
	}
	return strings.Join(keep, "\n")
}

// tailLines 取输出末尾若干行，避免日志刷屏
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// 场景 9（背压）：调度通道打满时不得 fork 协程，且定时器「留轮重试」而非永久丢弃。
//
//	修复前：通道满 → 每个到期定时器 go executeTimer → 与续期 AddTimer 递归 fork，
//	毫秒轮 1ms 一次即可 OOM。修复后：发送失败就留在当前轮，下个 tick 重试，零新增协程。
//
//	用 NewTimerManager 独立驱动（不 Run、无消费协程），全局通道天然保持满，
//	隔离出纯背压路径。
// ============================================================================

func TestRegression_BackpressureNoGoroutineExplosion(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	wheel := m.wheels[TimerTypeHour] // 小时轮 1h 才 tick 一次，不会自我干扰
	ch := currentTimerChannel()

	// 灌满全局调度通道，制造持续背压
fill:
	for i := int64(0); i < MaxTimerChannelNum; i++ {
		select {
		case ch <- timerTask{}:
		default:
			break fill
		}
	}

	base := runtime.NumGoroutine()

	plant := func() {
		tm, err := NewTimer[*plainTimer](1000, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := tm.GetParent()
		p.nextTime.Store(util.CurrentMS() - 1) // 已过期，必然走派发分支
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
	}

	for i := 0; i < 200; i++ {
		plant()
		m.processWheelTick(wheel, TimerTypeHour)
	}

	if g := runtime.NumGoroutine(); g-base > 5 {
		t.Fatalf("✘ 背压期间协程数暴涨: base=%d now=%d", base, g)
	}
	if d := m.GetStats().TimersDropped; d < 200 {
		t.Fatalf("✘ 丢弃计数不足: %d（期望 >=200）", d)
	}
	if l := wheel.tickWheel.Length(); l != 200 {
		t.Fatalf("✘ 定时器应全部留在轮里等待重试: 链表长度=%d（期望 200）", l)
	}
	if c := wheel.timerCount.Load(); c != 200 {
		t.Fatalf("✘ 计数与链表不一致: count=%d len=%d", c, wheel.tickWheel.Length())
	}

	// 腾出一个空位，验证「留轮重试」而非永久丢弃
	<-ch
	m.processWheelTick(wheel, TimerTypeHour)
	if l := wheel.tickWheel.Length(); l != 199 {
		t.Fatalf("✘ 通道腾空后应有一个定时器恢复派发: 链表长度=%d（期望 199）", l)
	}

	// 清理：排空灌入的占位任务，避免污染后续测试的全局通道
drain:
	for {
		select {
		case <-ch:
		default:
			break drain
		}
	}
	t.Logf("✔ 背压期间零协程增长，累计丢弃 %d 次，通道腾空后恢复派发", m.GetStats().TimersDropped)
}

// ============================================================================
// 场景 10（并发）：N 个协程并发 TimeStop 不得触发 "close of closed channel"。
//
//	修复前：TimeStop 对裸全局 closech 先查后关，非原子，两个并发调用会双重 close。
//	修复后：timerRT.Swap(nil) 只有一方拿到 runtime，closeOnce 再兜一层幂等。
// ============================================================================

func scenarioConcurrentTimeStop() {
	Run(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			TimeStop(context.Background())
		}()
	}
	wg.Wait()
}

func TestRegression_ConcurrentTimeStopNoPanic(t *testing.T) {
	if isChild() {
		scenarioConcurrentTimeStop()
		return
	}
	out, timedOut := runCrashChild(t, "TestRegression_ConcurrentTimeStopNoPanic", 25*time.Second)
	if strings.Contains(out, "close of closed channel") {
		t.Fatalf("✘ 并发 TimeStop 触发双重 close:\n%s", tailLines(out, 20))
	}
	if strings.Contains(out, "panic:") {
		t.Fatalf("✘ 并发 TimeStop panic:\n%s", tailLines(out, 20))
	}
	if timedOut {
		t.Fatalf("✘ 并发 TimeStop 挂死:\n%s", tailLines(out, 12))
	}
	t.Log("✔ 32 个协程并发 TimeStop 无 panic、无双重 close")
}

// ============================================================================
// 场景 11（生命周期）：连续多次 Run 不得泄漏管理器 / 时间轮协程。
//
//	修复前：Run 只 Clear 管理器 map 不 Close，每多一次 Run 就泄漏 5 个时间轮协程
//	+ 5 个 ticker。修复后：Run 先 Close 上一轮全部管理器并等待旧 TimeTick 退出。
// ============================================================================

func TestRegression_RepeatedRunNoManagerLeak(t *testing.T) {
	Run(context.Background())
	TimeStop(context.Background())
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		Run(context.Background())
	}

	// 确定性断言：任意时刻管理器 map 里最多一个存活管理器
	if l := timerManagerMap.Length(); l != 1 {
		t.Fatalf("✘ 重复 Run 后管理器泄漏: map 长度=%d（期望 1）", l)
	}
	if g := runtime.NumGoroutine(); g-base > 12 {
		t.Logf("⚠ 协程数增长偏多: base=%d now=%d（软提示）", base, g)
	}

	TimeStop(context.Background())
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	if g := runtime.NumGoroutine(); g > base+3 {
		t.Fatalf("✘ TimeStop 后协程未回落: base=%d now=%d", base, g)
	}
	t.Logf("✔ 连续 3 次 Run 无管理器泄漏，TimeStop 后协程回落至 %d", runtime.NumGoroutine())
}

// ============================================================================
// 场景 12（跨管理器）：非默认管理器的定时器触发后续期必须回到原管理器。
//
//	修复前：调度通道全局共享（各管理器共用同一组分片），消费端恒用默认管理器执行并续期，
//	非默认管理器的定时器首次触发后即被改嫁。修复后：timerTask 携带 mgr 归属。
// ============================================================================

func TestManagerIsolation_NoHijack(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	custom := NewTimerManager()
	defer custom.Close()

	// 注意：NewTimer 返回的是对象池里的实例，不是外部构造的那个，
	// 所以必须用返回的 tm 本身，而非外部新建的 &plainTimer{}。
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := custom.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	pt := tm

	time.Sleep(300 * time.Millisecond)

	if n := pt.n.Load(); n == 0 {
		t.Fatal("✘ 非默认管理器的定时器未被执行")
	}
	customStats := custom.GetStats()
	defaultStats := GetDefaultManager().GetStats()
	if customStats.TimersAdded < 2 {
		t.Fatalf("✘ 非默认管理器续期计数异常: TimersAdded=%d（期望 >=2）", customStats.TimersAdded)
	}
	if defaultStats.TimersAdded != 0 {
		t.Fatalf("✘ 定时器被改嫁到默认管理器: 默认 TimersAdded=%d（期望 0）", defaultStats.TimersAdded)
	}
	t.Logf("✔ 非默认管理器定时器持续在原管理器续期: custom.Added=%d default.Added=%d",
		customStats.TimersAdded, defaultStats.TimersAdded)
}

// ============================================================================
// 场景 13（计数一致性）：大量派发 / 迁移 / 移除后，timerCount 与链表长度必须一致且归零。
//
//	修复前：AddTimer 入队时 +1、迁移到达不 +1、触发/迁移各 -1 → 每次迁移净 -1，长期变负；
//	Remove 摘链不 -1 → 泄漏。修复后：计数只在「节点真正进出链表」处增减，严格配对。
// ============================================================================

func TestTimerCount_MatchesListAfterChurn(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	var timers []TimerInterface
	for i := 0; i < 40; i++ {
		// 间隔跨越时间轮边界，强制触发派发 + 迁移
		tm, err := NewTimer[*plainTimer](int64(50+i*100), InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}

	// 让派发 / 迁移充分发生，期间采样断言计数不为负
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		if c := m.GetTimerCount(); c < 0 {
			t.Fatalf("✘ 计数为负: %d", c)
		}
	}

	for _, tm := range timers {
		tm.Remove()
	}

	// 轮询等待全部摘除。失活残留由 processWheelTick 的清理分支负责摘链，
	// 这里绝不补第二轮 Remove：实例一旦归还对象池就随时可能被别人认领，
	// 陈旧主人的任何再操作都是在撕别人的调度状态。
	deadline := time.Now().Add(2 * time.Second)
	for {
		var sumCount, sumLen int64
		for _, w := range m.wheels {
			sumCount += w.timerCount.Load()
			sumLen += int64(w.tickWheel.Length())
		}
		if sumCount == 0 && sumLen == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("✘ 全部移除后计数/链表未归零: count=%d len=%d", sumCount, sumLen)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("✔ 大量派发/迁移/移除后计数与链表一致且归零")
}

// ============================================================================
// 基准：processWheelTick 的 Range 快照分配。
//
//	list.Range 改为 sync.Pool 复用快照 buffer 后，每 tick 不应再为链表长度
//	线性分配短命切片。用 -benchmem 对比 allocs/op。
// ============================================================================

func BenchmarkProcessWheelTick(b *testing.B) {
	m := NewTimerManager()
	defer m.Close()

	wheel := m.wheels[TimerTypeHour]
	for i := 0; i < 1000; i++ {
		tm, err := NewTimer[*plainTimer](3600*1000, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		p := tm.GetParent()
		// 剩余时间需 >= 1h，calculateType 才判为小时轮，既不派发也不迁移，
		// 这样基准只测量 Range 快照分配本身。
		p.nextTime.Store(util.CurrentMS() + 3600*1000 + 60*1000)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.processWheelTick(wheel, TimerTypeHour)
	}
}

// reentrantHasNextTimer 在 HasNext 里复刻「Tick 收尾与业务 Pause+AddTimer 重连交错」的窗口。
//
// 真实竞态：finishTimerTick 以锁外方式先读 HasNext、再读 IsActive；业务若在两次读
// 之间完成 Pause+AddTimer，会留下「HasNext 看到不活跃、IsActive 看到活跃、代次已
// 推进」的组合。旧实现把它当作自然耗尽并把实例归还对象池，重连后的定时器就此丢失。
//
// hook 用真实 Pause/AddTimer 制造该状态；HasNext 返回 false 只是复刻「锁外那次读取
// 落在不活跃窗口里」的观测结果，使窗口确定化。
type reentrantHasNextTimer struct {
	// Timer 提供其余接口实现。
	Timer
	// hook 在 HasNext 返回前执行，模拟业务重连。
	hook func()
	// hookErr 记录 hook 内 AddTimer 的错误文本。
	hookErr atomic.Value
}

// Tick 本用例不触发回调。
func (p *reentrantHasNextTimer) Tick() {}

// HasNext 先执行重连 hook，再返回 false 表示「锁外这次读取看到不活跃」。
func (p *reentrantHasNextTimer) HasNext() bool {
	if p.hook != nil {
		p.hook()
	}
	return false
}

// TestFinishTimerTick_ReconnectDuringFinishNotReleased 回归：Tick 收尾瞬间业务执行
// Pause+AddTimer 重连时，旧代次的调度项不得把已重挂的实例归还对象池。
func TestFinishTimerTick_ReconnectDuringFinishNotReleased(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	tm, err := NewTimer[*reentrantHasNextTimer](60_000, InfiniteCount, func(p *reentrantHasNextTimer) {
		p.hook = func() {
			// 业务重连的真实写法：先停调度，再按当前状态重新挂回。
			p.Pause()
			if err := AddTimer(p); err != nil {
				p.hookErr.Store(err.Error())
			}
		}
	})
	if err != nil {
		t.Fatalf("new timer: %v", err)
	}

	parent := tm.GetParent()
	task := timerTask{timer: tm, gen: parent.gen.Load(), mgr: m}
	// 模拟 executeTimer：先登记在飞，再走收尾路径。
	if !parent.beginTick(task.gen) {
		t.Fatal("beginTick 应成功")
	}

	before := GetTimerPoolStats().Puts
	m.finishTimerTick(task, parent)

	if v := tm.hookErr.Load(); v != nil {
		t.Fatalf("重连 AddTimer 失败: %v", v)
	}
	if parent.released.Load() || parent.inPool.Load() {
		t.Fatalf("并发重连后不得归还对象池: released=%v inPool=%v", parent.released.Load(), parent.inPool.Load())
	}
	if delta := GetTimerPoolStats().Puts - before; delta != 0 {
		t.Fatalf("不应发生任何归还，Puts 增量=%d", delta)
	}
	// 实例仍在调用方手里，正常作废收尾应能回池。
	tm.Remove()
	if delta := GetTimerPoolStats().Puts - before; delta != 1 {
		t.Fatalf("收尾 Remove 应恰好归还一次，Puts 增量=%d", delta)
	}
}

// reconnectTickTimer 每次 Tick 都执行一次 Pause+AddTimer，模拟断线重连。
type reconnectTickTimer struct {
	// Timer 提供其余接口实现。
	Timer
	// n 已执行的回调次数。
	n atomic.Int64
	// errText 记录 AddTimer 的错误文本。
	errText atomic.Value
}

// Tick 先计数，再按业务重连写法重挂自身。
func (p *reconnectTickTimer) Tick() {
	p.n.Add(1)
	p.Pause()
	if err := AddTimer(p); err != nil {
		p.errText.Store(err.Error())
	}
}

// TestTickReconnectKeepsScheduling 验证 Tick 内 Pause+AddTimer 的重连路径不会
// 让定时器停摆，也不会返回 ErrTimerReleased。
func TestTickReconnectKeepsScheduling(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*reconnectTickTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatalf("new timer: %v", err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatalf("AddTimer: %v", err)
	}

	waitUntil(t, 3*time.Second, "Tick 内 Pause+AddTimer 后定时器应继续触发", func() bool {
		return tm.n.Load() >= 10
	})
	if v := tm.errText.Load(); v != nil {
		t.Fatalf("重连 AddTimer 失败: %v", v)
	}
	tm.Remove()
}

// ============================================================================
// 对象池所有权、作废归还与续期失效的定向验证。
//
// 语义基线：
//   - Remove()   = 彻底作废：摘链 + 归还对象池。此后调用方必须丢弃该指针；
//     对已作废实例调用 AddTimer 返回 ErrTimerReleased（不 panic、不放行）。
//   - Pause()    = 只停调度：所有权留在调用方手里，可用 AddTimer 复活
//     （rpc 客户端断线重连即此形态）。
//   - 有限次数自然耗尽（HasNext()==false 且仍活跃）⇒ 消费协程自动作废并回池。
//
// 必须守住的三条不变量（历史缺陷回归）：
//   1) 同一实例绝不同时属于两个逻辑定时器。双主的两个定时器会互相把对方的
//      调度项判为过期，实测症状是「30 个逻辑定时器只剩 7 个唯一实例、0 次 Tick」。
//   2) 业务回调在飞时不得归还实例（否则新主人与该回调同时写同一块内存）。
//   3) 调用方手工构造的宿主对象（type X struct{ Timer }）没有入池资格，
//      一旦被池收走，宿主内存会被当成空白定时器覆盖。
//
// sync.Pool 没有可观测长度，「是否真的回池」用 GetTimerPoolStats().Puts 断言。
// ============================================================================

type reviveTimer struct {
	Timer
	n atomic.Int64
}

func (r *reviveTimer) Tick() { r.n.Add(1) }

// pausingTimer 在回调里把自己暂停：验证「Tick 内 Pause」不会被误当成耗尽回池
type pausingTimer struct {
	Timer
	n     atomic.Int64
	pause bool
}

func (p *pausingTimer) Tick() {
	p.n.Add(1)
	if p.pause {
		p.Pause()
	}
}

// manualTimer 完全由调用方 new 出来，从未进过对象池
type manualTimer struct {
	Timer
	n atomic.Int64
}

func (m *manualTimer) Tick() { m.n.Add(1) }

// drainAddChan 排空裸轮的入队通道：没有 runWheel 协程消费它，测试自行把残留
// 调度项喂给 handleTimerAdd，之后才能断言轮内确无残留。
func drainAddChan(m *TimerManager, wi TimerType) {
	wheel := m.wheels[wi]
	for {
		select {
		case task := <-wheel.addTimerChan:
			m.handleTimerAdd(wheel, task)
		default:
			return
		}
	}
}

// ----------------------------------------------------------------------------
// 测试 1：Pause → AddTimer 复活必须继续工作，且实例绝不重复发放。
// ----------------------------------------------------------------------------

func TestPoolOwnership_PauseThenReviveNoAliasing(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	const total = 16
	held := make(map[*reviveTimer]bool, total*2)

	newTimer := func() *reviveTimer {
		t.Helper()
		tm, err := NewTimer[*reviveTimer](5, InfiniteCount, func(p *reviveTimer) { p.n.Store(0) })
		if err != nil {
			t.Fatal(err)
		}
		if held[tm] {
			t.Fatalf("✘ 实例 %p 被重复发放：它仍在别的逻辑定时器手里", tm)
		}
		held[tm] = true
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		return tm
	}

	live := make([]*reviveTimer, 0, total)
	for i := 0; i < total; i++ {
		live = append(live, newTimer())
	}
	time.Sleep(80 * time.Millisecond)

	for _, tm := range live {
		if tm.n.Load() == 0 {
			t.Fatalf("✘ 定时器 uid=%d 从未 Tick（实例可能已被池共用）", tm.GetUID())
		}
	}

	// 半数 Pause 后复活（rpc 形态），半数 Remove 永久作废（所有权交还池）
	revived := live[:total/2]
	retired := live[total/2:]
	for _, tm := range revived {
		tm.Pause()
	}
	for _, tm := range retired {
		tm.Remove()
		delete(held, tm) // 指针仍在手里，但它已经不归我们了
	}

	fresh := make([]*reviveTimer, 0, total/2)
	for i := 0; i < total/2; i++ {
		fresh = append(fresh, newTimer())
	}
	for _, tm := range revived {
		if err := AddTimer(tm); err != nil {
			t.Fatalf("✘ Pause 后复活失败（AddTimer 应支持复活）: uid=%d err=%v", tm.GetUID(), err)
		}
	}

	time.Sleep(80 * time.Millisecond)

	var ticked, stalled int
	for _, tm := range append(append([]*reviveTimer{}, revived...), fresh...) {
		if tm.n.Load() == 0 {
			stalled++
		} else {
			ticked++
		}
	}
	if stalled > 0 {
		t.Fatalf("✘ %d 个定时器一次都没 Tick（复活被池串号打断），正常 %d 个", stalled, ticked)
	}
	if len(held) != total {
		t.Fatalf("✘ 唯一实例数 %d 与在用逻辑定时器数 %d 不符", len(held), total)
	}
	t.Logf("✔ %d 个在用定时器全部持有唯一实例，Pause 复活与新建的 %d 个全部持续 Tick", len(held), ticked)
}

// ----------------------------------------------------------------------------
// 测试 2：Remove 即永久作废 —— 归还对象池、拒绝复活、实例可被池再发。
// ----------------------------------------------------------------------------

func TestPoolOwnership_RemoveReleasesToPoolAndRefusesRevive(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*reviveTimer](50, InfiniteCount, func(p *reviveTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)

	tm.Remove() // 彻底作废：摘链 + 归还对象池
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ Remove 未归还对象池: Puts %d -> %d", putsBefore, puts)
	}

	if err := m.AddTimer(tm); !errors.Is(err, ErrTimerReleased) {
		t.Fatalf("✘ 已作废实例必须拒绝复活，got=%v", err)
	}
	if tm.GetParent().IsActive() {
		t.Fatal("✘ 被拒绝的复活不得把定时器重新置为活跃")
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)

	// 归还过的实例是合法的可复用对象：NewTimer 认领后所有权转移，可正常调度。
	again, err := NewTimer[*reviveTimer](50, InfiniteCount, func(p *reviveTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(again); err != nil {
		t.Fatalf("✘ 池中复用实例注册失败: %v", err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, again, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	t.Log("✔ Remove 已归还对象池：旧指针复活被拒（ErrTimerReleased），池复用注册正常")
}

// ----------------------------------------------------------------------------
// 测试 3：续期必须作废于「业务 Pause」之后，绝不把停掉的定时器唤醒。
// ----------------------------------------------------------------------------

func TestTickReschedule_RenewalCanceledAfterPause(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*reviveTimer](50, InfiniteCount, func(p *reviveTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)

	task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)

	// 派发之后、续期之前，业务抢先停表（本用例直连停表以覆盖该窗口）
	tm.Pause()
	if err := m.addTimerWithRetry(tm, task.gen); !errors.Is(err, ErrTimerCanceled) {
		t.Fatalf("✘ 代次已变，续期应作废并返回 ErrTimerCanceled，got=%v", err)
	}
	if p.IsActive() {
		t.Fatal("✘ 作废的续期不得把已停表的定时器重新置为活跃")
	}
	if p.wheel.Load() != nil {
		t.Fatal("✘ 作废的续期不得把已停表的定时器挂回时间轮")
	}
	select {
	case readd := <-m.wheels[TimerTypeMillisecond].addTimerChan:
		if readd.timer == TimerInterface(tm) {
			t.Fatal("✘ 作废的续期仍产生了入队任务")
		}
		m.handleTimerAdd(m.wheels[TimerTypeMillisecond], readd)
	default:
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	if st := m.GetStats(); st.TimersRescheduleFailed != 0 {
		t.Fatalf("✘ 正常作废不应计入调度失败: %+v", st)
	}
	if puts := GetTimerPoolStats().Puts; puts != putsBefore {
		t.Fatalf("✘ Pause 只是停调度，不得归还对象池: Puts %d -> %d", putsBefore, puts)
	}

	// 代次未变时续期照常回队
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	task2 := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
	if err := m.addTimerWithRetry(tm, task2.gen); err != nil {
		t.Fatalf("✘ 代次未变的续期应当成功: %v", err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	t.Log("✔ 续期严格跟随代次：Pause 后作废（不计失败、不入队、不回池），代次未变时正常回链")
}

// ----------------------------------------------------------------------------
// 测试 4：Tick 内 Remove —— 归还恰好一次，且推迟到回调结束之后。
// ----------------------------------------------------------------------------

func TestRelease_TickInnerRemovePoolsExactlyOnce(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*selfRemoveTimer](50, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)

	task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
	m.executeTimer(task) // Tick 内 Remove：此刻 inTick==1，只能挂起归还请求

	if ticks := tm.ticked.Load(); ticks != 1 {
		t.Fatalf("✘ Tick 未执行: %d", ticks)
	}
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ Tick 内 Remove 应归还对象池恰好一次: Puts %d -> %d", putsBefore, puts)
	}
	if p.inTick.Load() != 0 {
		t.Fatalf("✘ 回调在飞计数未归零: %d", p.inTick.Load())
	}

	// 重复作废（陈旧主人的第二次 Remove）必须被 inPool 闸挡下：不再摘链、不再 Put
	tm.Remove()
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ 重复 Remove 造成二次归还: Puts=%d", puts)
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	t.Log("✔ Tick 内 Remove 延迟到回调结束后归还，且只归还一次")
}

// ----------------------------------------------------------------------------
// 测试 5：Tick 内 Pause 不得被「耗尽回池」误伤。
// ----------------------------------------------------------------------------

func TestRelease_PauseInsideTickIsNotPooled(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*pausingTimer](50, 1, func(p *pausingTimer) { p.pause = true })
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)

	task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
	m.executeTimer(task)

	if puts := GetTimerPoolStats().Puts; puts != putsBefore {
		t.Fatalf("✘ Tick 内 Pause 被当成耗尽回池: Puts %d -> %d", putsBefore, puts)
	}
	// Pause 之后仍可复活：所有权仍在调用方手里
	if err := m.AddTimer(tm); err != nil {
		t.Fatalf("✘ Pause 后应可复活: %v", err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	tm.Remove()
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ Pause 之后补 Remove 仍须回池: Puts %d -> %d", putsBefore, puts)
	}
	t.Log("✔ Tick 内 Pause 不回池，随后可复活；再 Remove 才回池")
}

// ----------------------------------------------------------------------------
// 测试 6：Pause 之后再 Remove 必须仍然回池。
//
// 回归点：removeFromManagerLocked 早版按「原本是否活跃」短路，Pause 已把活跃
// 标记撤了，随后的 Remove 会整段跳过归还分支，实例永久滞留在调用方手里。
// ----------------------------------------------------------------------------

func TestRelease_PauseThenRemoveStillPools(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*reviveTimer](50, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)

	tm.Pause()
	if puts := GetTimerPoolStats().Puts; puts != putsBefore {
		t.Fatalf("✘ Pause 不该回池: Puts %d -> %d", putsBefore, puts)
	}
	tm.Remove()
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ Pause 后 Remove 未回池: Puts %d -> %d", putsBefore, puts)
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	t.Log("✔ Pause → Remove 仍正常归还对象池")
}

// ----------------------------------------------------------------------------
// 测试 7：有限次数自然耗尽 ⇒ 消费协程自动作废并回池。
// ----------------------------------------------------------------------------

func TestRelease_ExhaustionAutoPools(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*plainTimer](50, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)

	startTicks := tm.n.Load() // 池实例的业务计数可能非 0
	task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
	m.executeTimer(task) // 最后一次执行：HasNext()==false 且仍活跃

	if n := tm.n.Load(); n != startTicks+1 {
		t.Fatalf("✘ 最后一次 Tick 未执行: 起点 %d 之后为 %d", startTicks, n)
	}
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ 耗尽后未自动归还对象池: Puts %d -> %d", putsBefore, puts)
	}
	if c := tm.GetRemainingCount(); c != 0 {
		t.Fatalf("✘ 耗尽后剩余次数应为 0，got=%d", c)
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	if err := m.AddTimer(tm); !errors.Is(err, ErrTimerReleased) {
		t.Fatalf("✘ 已自动作废的实例必须拒绝复活，got=%v", err)
	}
	t.Log("✔ 有限次数耗尽后自动作废回池，旧指针复活被拒")
}

// ----------------------------------------------------------------------------
// 测试 8：手工构造的宿主对象绝不入池（防内存踩踏）。
// ----------------------------------------------------------------------------

func TestRelease_ManualHostNeverPooled(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts
	rejectedBefore := GetTimerPoolStats().PutRejected

	tm := &manualTimer{}
	p := tm.GetParent()
	if err := p.Init(50, InfiniteCount, tm); err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)

	tm.Remove() // 作废：没有池凭证 ⇒ 只摘链，绝不入池

	if puts := GetTimerPoolStats().Puts; puts != putsBefore {
		t.Fatalf("✘ 手工构造的宿主对象被塞进了对象池: Puts %d -> %d", putsBefore, puts)
	}
	if r := GetTimerPoolStats().PutRejected; r != rejectedBefore+1 {
		t.Fatalf("✘ 拒绝入池未留痕: PutRejected %d -> %d", rejectedBefore, r)
	}
	if p.fromPool.Load() {
		t.Fatal("✘ 未经池发放的实例不应持有池凭证")
	}
	assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
	// 宿主对象所有权仍在调用方手里，随后仍可正常使用（等价于 Pause 的效果）
	if err := m.AddTimer(tm); err != nil {
		t.Fatalf("✘ 非池实例作废后应可重新注册: %v", err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	tm.Remove()
	t.Log("✔ 无池凭证的宿主对象只停调度，不入池、不踩踏")
}

// ----------------------------------------------------------------------------
// 测试 9：并发作废 / 停表 / 耗尽，只归还一次，计数不为负。
// ----------------------------------------------------------------------------

func TestRelease_ConcurrentVoidPoolsOnce(t *testing.T) {
	m := newRawWheelManager()
	const count = 32
	putsBefore := GetTimerPoolStats().Puts

	timers := make([]*reviveTimer, 0, count)
	for i := 0; i < count; i++ {
		tm, err := NewTimer[*reviveTimer](50, int64(i%3), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}

	var wg sync.WaitGroup
	for _, tm := range timers {
		wg.Add(4)
		for j := 0; j < 4; j++ {
			go func(tm *reviveTimer, k int) {
				defer wg.Done()
				switch k % 4 {
				case 0, 1:
					tm.Remove()
				case 2:
					tm.Pause()
				default:
					tm.Remove()
					_ = m.AddTimer(tm) // 陈旧主人试图复活已作废实例，必须被拒
				}
			}(tm, j)
		}
	}
	wg.Wait()

	drainAddChan(m, TimerTypeMillisecond)
	if c := m.GetTimerCount(); c != 0 {
		t.Fatalf("✘ 全部作废后仍有残留调度: count=%d len=%d", c, m.wheels[TimerTypeMillisecond].tickWheel.Length())
	}
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+count {
		t.Fatalf("✘ 归还次数应恰等于实例数（每个至多一次）: got=%d want=%d", puts-putsBefore, count)
	}
	t.Logf("✔ %d 个实例并发作废：无残留调度、无重复归还", count)
}

// ----------------------------------------------------------------------------
// 测试 10：rpc 客户端形态 —— Pause/复活 循环后最终 Remove 才交出所有权。
// ----------------------------------------------------------------------------

func TestRelease_RpcStylePauseCycleThenRemove(t *testing.T) {
	m := newRawWheelManager()
	putsBefore := GetTimerPoolStats().Puts

	tm, err := NewTimer[*reviveTimer](50, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := m.AddTimer(tm); err != nil {
			t.Fatalf("✘ 第 %d 次注册失败: %v", i, err)
		}
		tm.Pause()
	}
	if puts := GetTimerPoolStats().Puts; puts != putsBefore {
		t.Fatalf("✘ 重连循环中的 Pause 不得回池: Puts %d -> %d", putsBefore, puts)
	}
	tm.Remove()
	if puts := GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ 最终 Remove 未回池: Puts %d -> %d", putsBefore, puts)
	}
	if err := m.AddTimer(tm); !errors.Is(err, ErrTimerReleased) {
		t.Fatalf("✘ 作废后不得再复活，got=%v", err)
	}
	reviveRefused := GetTimerPoolStats().ReviveRefused
	if reviveRefused == 0 {
		t.Fatal("✘ 拒绝复活未计入统计")
	}
	t.Logf("✔ Pause/复活 循环不回池，Remove 后回池且拒绝复活（累计拒绝 %d 次）", reviveRefused)
}

// ============================================================================
// 续期（reschedule）路径的回归验证。
//
// 这些能力与「多线程执行池」无关，在执行池被移除后仍需保持有效：
//   1. Timer.SetInterval —— 非正间隔必须被忽略，避免时间轮收到非法值；
//   2. AddTimer 无条件推进代次 —— 保证每次 AddTimer 产生的调度项 gen 唯一；
//   3. 门面 AddTimer 支持一次传入多个定时器。
//
// 运行方式：go test ./localtimer -run 'TestTimer_SetInterval|TestAddTimer_' -v
// ============================================================================

// TestTimer_SetInterval 验证 SetInterval 的取值约束：非正值一律忽略。
func TestTimer_SetInterval(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, InfiniteCount, nil); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}

	tr.SetInterval(250)
	if got := tr.GetInterval(); got != 250 {
		t.Fatalf("SetInterval(250) 未生效: %d", got)
	}

	for _, bad := range []int64{0, -1, -1000} {
		tr.SetInterval(bad)
		if got := tr.GetInterval(); got != 250 {
			t.Fatalf("SetInterval(%d) 应被忽略，实际 interval=%d", bad, got)
		}
	}
	t.Log("✔ SetInterval 忽略非正间隔")
}

// genProbeTimer 是仅用于驱动入队 / 续期路径的空实现定时器。
type genProbeTimer struct {
	Timer
	ticked atomic.Int64
}

func (g *genProbeTimer) Tick() { g.ticked.Add(1) }

// TestAddTimer_AlwaysAdvancesGen 验证每次 AddTimer 都推进代次，包括「复活」路径。
//
// 摘链那一段只在「原本是活跃」时才推进代次；业务先 Pause 把定时器置为 inactive、
// 再 AddTimer 复活时它不会推进。若此时不额外推进一次代次，在途的旧调度项会与
// 新调度项共享同一个 gen，被 isValid 误判为有效，导致同一实例重复调度。
func TestAddTimer_AlwaysAdvancesGen(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*genProbeTimer](1000, InfiniteCount, nil)
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}
	p := tm.GetParent()

	// 活跃状态下的 AddTimer
	before := p.gen.Load()
	if err := m.AddTimer(tm); err != nil {
		t.Fatalf("AddTimer 失败: %v", err)
	}
	if after := p.gen.Load(); after <= before {
		t.Fatalf("活跃态 AddTimer 未推进代次: %d -> %d", before, after)
	}

	// 复活路径：先 Pause 置为 inactive，再 AddTimer
	tm.Pause()
	if p.IsActive() {
		t.Fatal("Pause 后应为 inactive")
	}

	before = p.gen.Load()
	if err := m.AddTimer(tm); err != nil {
		t.Fatalf("复活 AddTimer 失败: %v", err)
	}
	if after := p.gen.Load(); after <= before {
		t.Fatalf("✘ 复活路径 AddTimer 未推进代次: %d -> %d（在途旧调度项会被误判有效）",
			before, after)
	}
	t.Log("✔ 每次 AddTimer 都推进代次（含复活路径）")
}

// TestAddTimer_Variadic 验证门面 AddTimer 支持一次传入多个定时器，且拒绝 nil 元素。
func TestAddTimer_Variadic(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	a, err := NewTimer[*genProbeTimer](1000, InfiniteCount, nil)
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}
	b, err := NewTimer[*genProbeTimer](1000, InfiniteCount, nil)
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}

	if err := AddTimer(a, b); err != nil {
		t.Fatalf("AddTimer 可变参数调用失败: %v", err)
	}
	if !a.IsActive() || !b.IsActive() {
		t.Fatal("✘ 可变参数 AddTimer 未把两个定时器都置为活跃")
	}

	if err := AddTimer(a, nil); err != ErrTimerNilParent {
		t.Fatalf("含 nil 元素应返回 ErrTimerNilParent，实际: %v", err)
	}
	t.Log("✔ AddTimer 支持可变参数并拒绝 nil 元素")
}

// 验证 Timer 的 Init/HasNext/GetRemainingCount 行为，绕开对象池（pool 行为不可预测）
func TestTimer_Init_Basic(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, 3, nil); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if tr.interval.Load() != 100 || tr.count.Load() != 3 {
		t.Fatalf("Init 后字段未设置: %+v", tr)
	}
	if !tr.isActive.Load() {
		t.Fatal("Init 后 isActive 应为 true")
	}
}

func TestTimer_Init_InvalidInterval(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(0, 1, nil); err != ErrTimerInvalidInterval {
		t.Fatalf("期望 ErrTimerInvalidInterval，实际: %v", err)
	}
}

func TestTimer_Init_Infinite(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, InfiniteCount, nil); err != nil {
		t.Fatal(err)
	}
	if tr.count.Load() != CountCorrectionValue {
		t.Fatalf("InfiniteCount 应转 CountCorrectionValue，实际: %d", tr.count.Load())
	}
}

func TestTimer_HasNext_Decrement(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 2, nil)
	// 注：HasNext 通过 nextTime=now+interval 计算，需要先调一次让 nextTime 变化
	if !tr.HasNext() {
		t.Fatal("第一次 HasNext 应为 true（剩 1 次）")
	}
	if tr.HasNext() {
		t.Fatal("第二次 HasNext 应为 false（用完）")
	}
}

func TestTimer_HasNext_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	for i := 0; i < 50; i++ {
		if !tr.HasNext() {
			t.Fatalf("第 %d 次 HasNext 应仍为 true", i)
		}
	}
}

func TestTimer_GetRemainingCount(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 5, nil)
	if got := tr.GetRemainingCount(); got != 5 {
		t.Fatalf("初始应剩 5，实际: %d", got)
	}
}

func TestTimer_GetRemainingCount_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	if got := tr.GetRemainingCount(); got != InfiniteCount {
		t.Fatalf("无限模式应返回 InfiniteCount，实际: %d", got)
	}
}

func TestTimer_SetCount(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil)
	tr.SetCount(InfiniteCount)
	if tr.GetRemainingCount() != InfiniteCount {
		t.Fatal("SetCount(InfiniteCount) 异常")
	}
}

func TestTimer_GetInterval(t *testing.T) {
	tr := &Timer{}
	tr.Init(500, 1, nil)
	if got := tr.GetInterval(); got != 500 {
		t.Fatalf("GetInterval=%d want=500", got)
	}
}

func TestTimer_RemoveFromManager_NoWheel(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil)
	// 无 wheel 时直接调用不应 panic
	tr.RemoveFromManager(false)
	if tr.IsActive() {
		t.Fatal("RemoveFromManager 后 IsActive 应为 false")
	}
	// 无 self、无池凭证：彻底作废同样不应 panic，也不会被塞进对象池
	puts := GetTimerPoolStats().Puts
	tr.RemoveFromManager(true)
	if GetTimerPoolStats().Puts != puts {
		t.Fatal("✘ 无池凭证的裸 Timer 不得归还对象池")
	}
}

func TestTimer_CalculateType(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil) // nextTime 已设置
	cases := []struct {
		ms   int64
		want TimerType
	}{
		{50, TimerTypeMillisecond},
		{2 * 1000, TimerTypeSecond},
		{2 * 60 * 1000, TimerTypeMinute},
		{2 * 10 * 60 * 1000, TimerTypeTenMinute},
		{2 * 60 * 60 * 1000, TimerTypeHour},
	}
	for _, c := range cases {
		if got := tr.calculateType(c.ms); got != c.want {
			t.Fatalf("interval=%dms got=%v want=%v", c.ms, got, c.want)
		}
	}
}

// TestTimer_CalculateTypeEmptyTable 空精度表不得返回 -1 档位（会越界索引 m.wheels）。
func TestTimer_CalculateTypeEmptyTable(t *testing.T) {
	orig := defaultWheelConfigs
	t.Cleanup(func() { defaultWheelConfigs = orig })
	defaultWheelConfigs = nil

	tr := &Timer{}
	if got := tr.calculateType(1); got != TimerTypeMillisecond {
		t.Fatalf("空精度表应退回毫秒档, got=%v", got)
	}
}

func TestTimerType_String(t *testing.T) {
	cases := map[TimerType]string{
		TimerTypeMillisecond: "millisecond",
		TimerTypeSecond:      "second",
		TimerTypeMinute:      "minute",
		TimerTypeTenMinute:   "ten-minute",
		TimerTypeHour:        "hour",
		TimerType(99):        "unknown",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("TimerType(%d)=%s want=%s", typ, got, want)
		}
	}
}

func TestNewTimer_NilClass(t *testing.T) {
	// T 为接口类型时 zero 是 nil interface，reflect.TypeOf 取不到类型 → ErrTimerInvalidType
	if _, err := NewTimer[TimerInterface](100, 1, nil); err != ErrTimerInvalidType {
		t.Fatalf("期望 ErrTimerInvalidType，实际: %v", err)
	}
}

func TestNewTimer_InvalidInterval(t *testing.T) {
	// T 为接口类型时直接命中 ErrTimerInvalidType；该用例同时接受 interval 错误
	if _, err := NewTimer[TimerInterface](0, 1, nil); err != ErrTimerInvalidInterval && err != ErrTimerInvalidType {
		t.Fatalf("期望 interval 错误，实际: %v", err)
	}
}

func TestAddTimer_SystemNotReady(t *testing.T) {
	if err := AddTimer(nil); err != ErrTimerSystemNotReady {
		t.Fatalf("期望 ErrTimerSystemNotReady，实际: %v", err)
	}
}

func TestIsSystemRunning_False(t *testing.T) {
	if IsSystemRunning() {
		t.Fatal("未 Run 的系统不应视为运行中")
	}
}

func TestGetSystemStats_ZeroBeforeRun(t *testing.T) {
	total, _ := GetSystemStats()
	if total != 0 {
		t.Fatalf("未启动时 total 应为 0，实际: %d", total)
	}
}

func TestTimerStats_SnapshotCopyable(t *testing.T) {
	var s TimerStats
	s.TimersAdded = 10
	s.TimersExecuted = 20
	cp := s
	if cp.TimersAdded != 10 || cp.TimersExecuted != 20 {
		t.Fatal("TimerStats 应可安全复制")
	}
}

func TestConstants(t *testing.T) {
	if MaxTimerChannelNum <= 0 || MaxAddTimerChannelNum <= 0 {
		t.Fatal("通道上限常量应为正")
	}
	if InfiniteCount != -1 {
		t.Fatalf("InfiniteCount 应为 -1，实际: %d", InfiniteCount)
	}
	if DefaultWheelCount != 5 {
		t.Fatalf("DefaultWheelCount 应为 5，实际: %d", DefaultWheelCount)
	}
}

// 防止 sync 包导入警告（保留以供未来并发测试扩展）
var _ = sync.Mutex{}

// ============================================================================
// Tick 续期回链与引用计数非负性的定向验证。
//
// 验证点 1（计数）：wheel.timerCount 与 Timer.count 在派发/续期/移除/并发压测
//   全程不为负；有限次数耗尽后归零而非 -1。
// 验证点 2（回链）：-1（InfiniteCount）永久定时器与 count>0 的定时器，Tick 后
//   必须恰好重入队 1 条调度项（gen 最新、mgr 归属不变），经 handleTimerAdd
//   重新入链后计数与链表恢复一致；次数耗尽则不再入队。
//
// 调度拓扑提醒：AddTimer/续期写入的是 wheel.addTimerChan（轮私有通道），
// processWheelTick 到期派发才按 uid 写入调度分片通道（默认 1 片）。测试 1/2 直驱 executeTimer
// 并消费轮通道，排除时间片竞态；测试 3 走完整 Run/TimeTick/时间轮链路。
//
// 运行方式：
//   go test ./localtimer -run 'TestTickReschedule' -v
//   go test -race -count=2 ./localtimer/
// ============================================================================

// newRawWheelManager 构造一个不启动轮协程的管理器：wheels[i] 为裸 TimerWheel，
// addTimerChan 无人消费，入链全部由测试手动喂给 handleTimerAdd，保证确定性。
func newRawWheelManager() *TimerManager {
	ensureTimerChannel()
	m := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, 5),
	}
	for i, config := range []int64{1, 1000, 60000, 600000, 3600000} {
		m.wheels[i] = &TimerWheel{
			wheelConfig:  config,
			tickWheel:    newTestRing(config), // 与生产同规格的桶环（S67）
			addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
		}
	}
	return m
}

// wheelAddTimer 复刻 runWheel 消费轮通道的动作：取出本定时器的一条入队任务
// 并交给 handleTimerAdd 入链。expect=false 时断言通道中没有本定时器的任务。
// 续期入队发生在 executeTimer 返回之前（同步路径），因此只做非阻塞检查。
func wheelAddTimer(t *testing.T, m *TimerManager, wi TimerType, tm TimerInterface, expect bool) {
	t.Helper()
	ch := m.wheels[wi].addTimerChan
	if !expect {
		select {
		case task := <-ch:
			if task.timer == tm {
				t.Fatalf("✘ 不该续期的定时器被重新入队: gen=%d", task.gen)
			}
			m.handleTimerAdd(m.wheels[wi], task)
		default:
		}
		return
	}
	for {
		select {
		case task := <-ch:
			if task.timer != tm {
				m.handleTimerAdd(m.wheels[wi], task)
				continue
			}
			m.handleTimerAdd(m.wheels[wi], task)
			return
		default:
			// 通道已被清空但仍没等到：续期彻底丢失
			t.Fatal("✘ Tick 后未产生续期入队任务（定时器未回到 manager 队列）")
			return
		}
	}
}

// assertWheelRequeueOnce 校验轮通道里恰好只有这一条本定时器的入队任务。
func assertWheelRequeueOnce(t *testing.T, m *TimerManager, wi TimerType, tm TimerInterface) {
	t.Helper()
	wheelAddTimer(t, m, wi, tm, false)
}

// dispatchAndUnlink 走生产派发入口（commitDispatch）：认领归属 → 投递全局调度
// 通道 → 摘链扣计数，再把这条任务从通道里取出来交给 executeTimer，等价于
// TimeTick 消费端拿走它。
//
// 早期版本是手写复刻那段临界区（摘链 + Store(nil) + 计数 -1），与改造后的协议
// 已经漂移：真实路径写的是 migratingMarker 而不是 nil，且摘链前要先过 CAS。
// 复刻版测得再绿也证明不了生产代码，所以改成直接调用被测函数。
// 代次在投递前捕获，与 executeTimer 续期时比对的正是这条调度项的代次。
func dispatchAndUnlink(t *testing.T, m *TimerManager, wi TimerType, timer TimerInterface, parent *Timer) timerTask {
	t.Helper()
	ch := currentTimerChannel()
	task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
	node, ok := timer.(list.INode)
	if !ok {
		t.Fatalf("✘ 测试定时器未实现 list.INode: %T", timer)
	}
	if got := m.commitDispatch(m.wheels[wi], parent, node, task, ch); got != dispatchSent {
		t.Fatalf("✘ 生产派发入口拒绝派发本定时器: result=%d", got)
	}
	for {
		select {
		case got := <-ch:
			if got.timer == timer {
				return got
			}
		default:
			t.Fatal("✘ 结论是已投递，调度通道里却取不到这条任务")
		}
	}
}

// assertWheelConsistent 断言某个裸轮计数与链表严格一致（无并发时可精确相等）。
func assertWheelConsistent(t *testing.T, m *TimerManager, wi TimerType, want int64) {
	t.Helper()
	w := m.wheels[wi]
	if c := w.timerCount.Load(); c != want {
		t.Fatalf("✘ wheel[%d] 计数异常: got=%d want=%d len=%d", wi, c, want, w.tickWheel.Length())
	}
	if l := int64(w.tickWheel.Length()); l != want {
		t.Fatalf("✘ wheel[%d] 链表长度与计数不一致: count=%d len=%d", wi, w.timerCount.Load(), l)
	}
}

// ----------------------------------------------------------------------------
// 测试 1：-1 永久定时器，每次 Tick 后必须恰好重入队 1 条最新代次的调度项，
// 重新入链后计数/链表恢复；count 恒为无限哨兵，永不递减。
// ----------------------------------------------------------------------------

func TestTickReschedule_InfiniteTimerReentersQueue(t *testing.T) {
	m := newRawWheelManager()
	tm, err := NewTimer[*plainTimer](50, InfiniteCount, func(p *plainTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if p.wheel.Load() != nil {
		t.Fatal("池中复用实例带有残留的时间轮归属，前置状态不干净")
	}

	// 初始入链：模拟轮协程消费 AddTimer 的入队任务
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)

	const rounds = 3
	for r := 0; r < rounds; r++ {
		beforeGen := p.gen.Load()
		task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
		assertWheelConsistent(t, m, TimerTypeMillisecond, 0)

		m.executeTimer(task)

		if got := tm.n.Load(); got != int64(r+1) {
			t.Fatalf("✘ 第 %d 轮 Tick 未正确执行: n=%d", r+1, got)
		}
		if c := p.GetRemainingCount(); c != InfiniteCount {
			t.Fatalf("✘ 永久定时器剩余次数被误扣减: %d", c)
		}
		if !p.IsActive() {
			t.Fatal("✘ Tick 后定时器不应失活")
		}
		if p.gen.Load() <= beforeGen {
			t.Fatalf("✘ 续期未推进代次: %d -> %d", beforeGen, p.gen.Load())
		}

		// 必须恰好回队 1 条，且代次为最新、归属管理器不变
		select {
		case readd := <-m.wheels[TimerTypeMillisecond].addTimerChan:
			if readd.timer != TimerInterface(tm) {
				t.Fatal("✘ 轮通道出现非本定时器的任务")
			}
			if readd.gen != p.gen.Load() {
				t.Fatalf("✘ 续期调度项代次过期: task.gen=%d 当前 gen=%d", readd.gen, p.gen.Load())
			}
			if readd.mgr != m {
				t.Fatal("✘ 续期调度项归属管理器被改嫁")
			}
			m.handleTimerAdd(m.wheels[TimerTypeMillisecond], readd)
		default:
			t.Fatal("✘ Tick 后未产生续期入队任务（定时器未回到 manager 队列）")
		}
		assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
		assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	}
	t.Logf("✔ %d 轮 Tick 续期：每轮恰好回队 1 条最新代次任务，入链后计数与链表恒一致", rounds)
}

// ----------------------------------------------------------------------------
// 测试 2：count>0 的定时器逐次 Tick 续期，恰好执行 count 次后不再入队；
// 次数收敛为 0（不出现 -1），count==1 / count==0 边界同口径。
// ----------------------------------------------------------------------------

func TestTickReschedule_FiniteCountUntilExhausted(t *testing.T) {
	cases := []struct {
		name        string
		count       int64
		ticks       int64
		neverLinked bool
	}{
		{"count=3", 3, 3, false},
		{"count=1边界", 1, 1, false},
		{"count=0边界", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRawWheelManager()
			tm, err := NewTimer[*plainTimer](50, tc.count, func(p *plainTimer) { p.n.Store(0) })
			if err != nil {
				t.Fatal(err)
			}
			p := tm.GetParent()
			if p.wheel.Load() != nil {
				t.Fatal("池中复用实例带有残留的时间轮归属，前置状态不干净")
			}

			if tc.count > 0 {
				if err := m.AddTimer(tm); err != nil {
					t.Fatal(err)
				}
				wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
				assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
			}

			for r := int64(0); r < tc.ticks; r++ {
				task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
				m.executeTimer(task)

				if got := tm.n.Load(); got != r+1 {
					t.Fatalf("✘ 第 %d 次 Tick 未执行: n=%d", r+1, got)
				}
				want := tc.count - (r + 1)
				if got := p.GetRemainingCount(); got != want {
					t.Fatalf("✘ 第 %d 次 Tick 后剩余次数: got=%d want=%d", r+1, got, want)
				}
				if want > 0 {
					// count 仍 >0：必须重新入队并回链
					wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
					assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
					assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
				} else {
					// 最后一次执行：不再续期，节点保持已摘链状态自然消亡
					assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
					assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
					if p.wheel.Load() != nil {
						t.Fatal("✘ 次数耗尽后不应仍挂在时间轮上")
					}
				}
			}

			if got := p.GetRemainingCount(); got != 0 {
				t.Fatalf("✘ 耗尽后剩余次数应为 0（不得为负）: %d", got)
			}
			if tc.neverLinked {
				// count=0 从未入链：即便业务误触发一次到期执行，也不得回队
				m.executeTimer(timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
				assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
				assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
			}
		})
	}
	t.Log("✔ count>0 逐次回链续期、耗尽即止，计数收敛为 0 无 -1")
}

// ----------------------------------------------------------------------------
// 测试 3：完整链路（Run + TimeTick + 真实轮协程）业务形态压测。
//
// 看门狗持续校验：
//   - 逐轮 timerCount >= 0 且 timerCount <= 链表长度（见说明 1）；
//   - 全局计数求和不超过存活定时器数（超发即幻影）；
//   - 每个定时器 GetRemainingCount() 永不为负（业务只设正数或 InfiniteCount）。
//
// 说明 1：逐轮只断言 0 <= count <= len，不要求严格相等。相等在无并发时成立
// （测试 1/2 逐轮精确断言），并发下会瞬时偏移：timerCount 的扣减与摘链在同一
// wheelLock 临界区内完成，而 list 在 Range 期间走延迟删除（delPending），节点
// 真正离开链表、len 随之递减要等到整轮遍历收尾，故观测窗口内可能 len > count。
// 非负、不超存活数、不多于链表长度这三条则是任何时刻都成立的硬不变量，
// 最终一致由「全部移除后收敛归零」保证。
// 说明 2：扰动协程刻意不做裸 AddTimer 洪泛——那是纯背压场景（已由
// TestRegression_BackpressureNoGoroutineExplosion 覆盖），会把有限的通道容量
// 打满并触发设计内的丢弃/续期失败告警，干扰本次「回链正确性」的观测口径。
// 说明 3：压测覆盖「Pause → AddTimer 复活」（rpc 断线重连的真实形态）。
// Remove 的语义是「彻底作废 + 归还对象池」，作废后指针即成为悬垂引用，故本用例
// 只在收尾各调用一次。历史缺陷（30 个逻辑定时器只剩 7 个唯一实例、0 次 Tick）
// 的根因是「Remove 回池而业务仍持有同一指针」，如今由三条协议共同封死：
// inPool 的 CAS 认领保证同一实例只发一份、released 粘性标记让 AddTimer 拒绝复活
// 已作废实例、Tick 在飞时归还请求延迟到回调结束（endTick）才落地。
// ----------------------------------------------------------------------------

func TestTickReschedule_InvariantUnderLoad(t *testing.T) {
	Run(context.Background())
	putsStart := GetTimerPoolStats().Puts
	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	const numTimers = 30
	timers := make([]*plainTimer, 0, numTimers)
	for i := 0; i < numTimers; i++ {
		cnt := int64(InfiniteCount)
		if i%2 == 0 {
			cnt = int64(3 + i%5)
		}
		// 对象池会复用实例，n 带历史值，必须归零后才是本轮真实 Tick 数
		tm, err := NewTimer[*plainTimer](int64(10+i*70), cnt, func(p *plainTimer) {
			p.n.Store(0)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var maxLinked atomic.Int64
	created := make([][2]int64, numTimers) // [uid, 初始count]
	for i, tm := range timers {
		created[i] = [2]int64{tm.GetUID(), tm.GetRemainingCount()}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var sumCount int64
			for wi, w := range m.wheels {
				w.wheelLock.Lock()
				c := w.timerCount.Load()
				l := int64(w.tickWheel.Length())
				w.wheelLock.Unlock()
				if c < 0 {
					t.Errorf("✘ 压测期间 wheel[%d] 计数为负: %d", wi, c)
					return
				}
				if c > l {
					t.Errorf("✘ wheel[%d] 计数多于链表长度（幻影计数）: count=%d len=%d", wi, c, l)
					return
				}
				if c > numTimers {
					t.Errorf("✘ wheel[%d] 计数超发: count=%d 存活定时器仅 %d", wi, c, numTimers)
					return
				}
				sumCount += c
			}
			if sumCount > maxLinked.Load() {
				maxLinked.Store(sumCount)
			}
			if sumCount > numTimers {
				t.Errorf("✘ 全局计数 %d 超过存活定时器数 %d（幻影定时器）", sumCount, numTimers)
				return
			}
			for ti, tm := range timers {
				p := tm.GetParent()
				if raw := p.count.Load(); raw != CountCorrectionValue && raw < 0 {
					t.Errorf("✘ count 原始值为负: raw=%d | idx=%d uid=%d(创建时=%d) initCount=%d gen=%d active=%v interval=%d wheel=%v",
						raw, ti, p.GetUID(), created[ti][0], created[ti][1], p.gen.Load(), p.IsActive(),
						p.GetInterval(), p.wheel.Load() != nil)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// 业务侧并发扰动：重新入队、改次数、Pause 后复活（rpc 重连形态）。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tm := timers[(idx*3)%numTimers]
				switch idx % 4 {
				case 0:
					_ = AddTimer(tm)
				case 1:
					tm.SetCount(InfiniteCount)
				case 2:
					tm.SetCount(5)
				case 3:
					tm.Pause()
					time.Sleep(2 * time.Millisecond)
					_ = AddTimer(tm)
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	time.Sleep(800 * time.Millisecond)
	close(stop)
	wg.Wait()
	<-done

	// Tick 总数必须在作废之前统计：Remove 之后实例已归池，随时可能被下一个
	// NewTimer 认领，再读它的字段就是悬垂访问。
	var tickSum int64
	for _, tm := range timers {
		tickSum += tm.n.Load()
	}

	// 全量作废并等待收敛：全局计数与逐轮链表长度归零
	for _, tm := range timers {
		tm.Remove()
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var sumCount, sumLen int64
		for _, w := range m.wheels {
			w.wheelLock.Lock()
			sumCount += w.timerCount.Load()
			sumLen += int64(w.tickWheel.Length())
			w.wheelLock.Unlock()
		}
		if sumCount == 0 && sumLen == 0 {
			break
		}
		if time.Now().After(deadline) {
			var detail string
			for wi, w := range m.wheels {
				detail += fmt.Sprintf("wheel[%d] count=%d len=%d; ",
					wi, w.timerCount.Load(), w.tickWheel.Length())
			}
			TimeStop(context.Background())
			t.Fatalf("✘ 全部移除后未收敛归零: count=%d len=%d | %s", sumCount, sumLen, detail)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stats := m.GetStats()
	TimeStop(context.Background())

	t.Logf("迭代统计: Executed=%d Added=%d Removed=%d Dropped=%d RescheduleFailed=%d | 业务Tick总和=%d 在轮峰值=%d",
		stats.TimersExecuted, stats.TimersAdded, stats.TimersRemoved, stats.TimersDropped,
		stats.TimersRescheduleFailed, tickSum, maxLinked.Load())

	if stats.TimersExecuted == 0 {
		ch := currentTimerChannel()
		t.Fatalf("✘ 压测期间没有任何 Tick 被正确执行（续期链路中断）| chanLen=%d mapLen=%d exec=%d added=%d",
			len(ch), timerManagerMap.Length(), stats.TimersExecuted, stats.TimersAdded)
	}
	if stats.TimersRescheduleFailed > 0 {
		t.Errorf("✘ 存在续期最终失败的定时器(丢失风险): RescheduleFailed=%d Dropped=%d",
			stats.TimersRescheduleFailed, stats.TimersDropped)
	}
	// 净归还次数超过存活实例数 ⇒ 同一实例被重复 Put（所有权失控的直接证据）
	if puts := GetTimerPoolStats().Puts - putsStart; puts > numTimers {
		t.Errorf("✘ 对象池归还次数超过定时器数量（疑似重复归还）: Puts=+%d 定时器=%d", puts, numTimers)
	}
	t.Logf("✔ 压测通过: Executed=%d Migrations=%d Dropped=%d，全程计数非负且收敛归零",
		stats.TimersExecuted, stats.WheelMigrations, stats.TimersDropped)
}

// ========== TryAdvance 推进与改期语义 ==========

func TestTryAdvance_DecrementFinite(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, 3, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// count=3: 第一次推进 → true (剩2)
	if !tr.TryAdvance() {
		t.Fatal("TryAdvance #1 应为 true")
	}
	if tr.count.Load() != 2 {
		t.Fatalf("TryAdvance #1 后 count 应为 2, got %d", tr.count.Load())
	}

	// count=2: 第二次推进 → true (剩1)
	if !tr.TryAdvance() {
		t.Fatal("TryAdvance #2 应为 true")
	}
	if tr.count.Load() != 1 {
		t.Fatalf("TryAdvance #2 后 count 应为 1, got %d", tr.count.Load())
	}

	// count=1: 第三次推进 → false (最后一次，扣到0后返回false)
	if tr.TryAdvance() {
		t.Fatal("TryAdvance #3 应为 false (最后一次)")
	}
	if tr.count.Load() != 0 {
		t.Fatalf("TryAdvance #3 后 count 应为 0, got %d", tr.count.Load())
	}

	// 再调用 → false，不会变为负数
	if tr.TryAdvance() {
		t.Fatal("TryAdvance #4 应为 false")
	}
	if tr.count.Load() != 0 {
		t.Fatalf("count 不应为负, got %d", tr.count.Load())
	}
}

func TestTryAdvance_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(50, InfiniteCount, nil)

	for i := 0; i < 100; i++ {
		if !tr.TryAdvance() {
			t.Fatalf("TryAdvance 第 %d 次应为 true (无限次)", i)
		}
	}
	// count 不变（仍为 CountCorrectionValue）
	if tr.count.Load() != CountCorrectionValue {
		t.Fatalf("无限次 count 应保持 CountCorrectionValue, got %d", tr.count.Load())
	}
}

func TestTryAdvance_UpdatesNextTime(t *testing.T) {
	tr := &Timer{}
	tr.Init(200, InfiniteCount, nil)

	// 显式设置一个过期的 nextTime，确保 TryAdvance 后值被推进
	tr.nextTime.Store(1)

	tr.TryAdvance()
	after := tr.nextTime.Load()

	// nextTime 应被推进到当前时间+interval 的附近
	if after <= 1 {
		t.Fatalf("nextTime 应被推进: got %d", after)
	}
}

func TestTryAdvance_InactiveReturnsFalse(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	tr.isActive.Store(false) // 模拟已暂停

	if tr.TryAdvance() {
		t.Fatal("isActive=false 时 TryAdvance 应返回 false")
	}
}

// ========== HasNext 旧行为不回归 ==========

func TestHasNext_DelegatesToTryAdvance(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 2, nil)

	// HasNext 应与 TryAdvance 行为一致
	if !tr.HasNext() {
		t.Fatal("HasNext #1 应为 true")
	}
	if tr.HasNext() {
		t.Fatal("HasNext #2 应为 false (count耗尽)")
	}
}

func TestHasNext_Infinite_NoRegression(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)

	for i := 0; i < 50; i++ {
		if !tr.HasNext() {
			t.Fatalf("HasNext 第 %d 次应为 true", i)
		}
	}
}

func TestHasNext_Inactive_NoRegression(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 5, nil)
	tr.isActive.Store(false)

	if tr.HasNext() {
		t.Fatal("isActive=false 时 HasNext 应返回 false")
	}
}

// waitUntil 轮询断言：在 d 内满足 cond 才算通过
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("✘ %s（等待 %v 超时）", what, d)
}
