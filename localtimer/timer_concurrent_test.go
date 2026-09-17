package localtimer

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// MultiThread（多线程执行池）能力测试。
//
// 与 timer_crash_test.go 的既有约束呼应：
//   - 场景 9：绝不无界 fork 协程；本文件用「固定 worker + 有界队列 + 懒创建」保证；
//   - 场景 11：不得泄漏协程；本文件在关闭后断言 worker 全部退出；
//   - MultiThread() 默认 false 时，行为与协程数必须与改造前完全一致。
//
// 运行方式：go test ./localtimer -run TestMultiThread -race -v
// ============================================================================

// concurrentProbe 记录并发度峰值/执行次数，用于并行度断言。
//
// 定时器实例由对象池创建，测试无法向实例注入状态，因此探针放在包级变量，
// 由测试用例设置、用例结束清空（各用例串行执行）。
type concurrentProbe struct {
	inFlight atomic.Int64
	peak     atomic.Int64
	execs    atomic.Int64
	hold     time.Duration
}

func (p *concurrentProbe) enter() {
	cur := p.inFlight.Add(1)
	for {
		peak := p.peak.Load()
		if cur <= peak || p.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	p.execs.Add(1)
	if p.hold > 0 {
		time.Sleep(p.hold)
	}
	p.inFlight.Add(-1)
}

var testProbe atomic.Pointer[concurrentProbe]

func probeEnter() {
	if p := testProbe.Load(); p != nil {
		p.enter()
	}
}

// multiThreadTimer 覆写 MultiThread 恒返回 true —— 业务侧第一种写法。
type multiThreadTimer struct {
	Timer
}

func (m *multiThreadTimer) MultiThread() bool { return true }
func (m *multiThreadTimer) Tick()             { probeEnter() }

// serialTimer 不覆写 MultiThread，保持基类默认（false）。
type serialTimer struct {
	Timer
}

func (s *serialTimer) Tick() { probeEnter() }

// flagTimer 通过基类标记开启并行：需在 NewTimer 之后调用 SetMultiThread(true)。
type flagTimer struct {
	Timer
}

func (f *flagTimer) Tick() { probeEnter() }

// countingTimer 只计数、不阻塞，用于单元级验证重叠跳过语义。
type countingTimer struct {
	Timer
	ticked atomic.Int64
}

func (c *countingTimer) MultiThread() bool { return true }
func (c *countingTimer) Tick()             { c.ticked.Add(1) }

// reAddTimer 在 Tick 内主动把自己重新加入时间轮（业务常见写法），
// 从而制造「上一轮 Tick 未结束就再次到期」的重叠窗口，用于验证跳过保护。
type reAddTimer struct {
	Timer
}

func (r *reAddTimer) MultiThread() bool { return true }

func (r *reAddTimer) Tick() {
	if self := r.getSelf(); self != nil {
		// 先重新入轮：下一个调度点必然落在本轮耗时逻辑结束之前，制造真实的重叠窗口
		_ = AddTimer(self)
	}
	probeEnter() // 耗时的业务逻辑
}

// blockingTimer 用于直接测试执行池队列满的行为：Tick 阻塞直到 release 关闭。
type blockingTimer struct {
	Timer
	started atomic.Int64
	release chan struct{}
}

func (b *blockingTimer) MultiThread() bool { return true }

func (b *blockingTimer) Tick() {
	b.started.Add(1)
	if b.release != nil {
		<-b.release
	}
}

// waitUntil 轮询等待条件成立，超时返回 false。
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// ============================================================================
// 1. 并行生效：多个 MultiThread 定时器必须真正并发执行（并发度峰值 > 1）。
// ============================================================================

func TestMultiThread_ParallelExecution(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	probe := &concurrentProbe{hold: 40 * time.Millisecond}
	testProbe.Store(probe)
	defer testProbe.Store(nil)

	const n = 4
	var timers []TimerInterface
	for i := 0; i < n; i++ {
		tm, err := NewTimer(5, InfiniteCount, &multiThreadTimer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}
	defer func() {
		for _, tm := range timers {
			tm.Remove()
		}
	}()

	if !waitUntil(3*time.Second, func() bool { return probe.peak.Load() >= 2 }) {
		t.Fatalf("✘ MultiThread 定时器未并行执行，并发度峰值=%d（期望 >=2）", probe.peak.Load())
	}

	_, stats := GetSystemStats()
	if stats.TimersAsync == 0 {
		t.Fatalf("✘ 统计缺少并行执行计数: %+v", stats)
	}
	t.Logf("✔ 并行生效: 峰值=%d, 执行=%d, async=%d, fallback=%d",
		probe.peak.Load(), probe.execs.Load(), stats.TimersAsync, stats.TimersInlineFallback)
}

// ============================================================================
// 2. 默认行为不变：未标记的定时器仍由单个消费协程串行执行，且不新增协程。
// ============================================================================

func TestMultiThread_DefaultFalseStaysInline(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	probe := &concurrentProbe{hold: 20 * time.Millisecond}
	testProbe.Store(probe)
	defer testProbe.Store(nil)

	base := runtime.NumGoroutine()

	var timers []TimerInterface
	for i := 0; i < 4; i++ {
		tm, err := NewTimer(5, InfiniteCount, &serialTimer{})
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}
	defer func() {
		for _, tm := range timers {
			tm.Remove()
		}
	}()

	time.Sleep(300 * time.Millisecond)

	if n := probe.execs.Load(); n < 2 {
		t.Fatalf("✘ 串行定时器执行次数过少: %d", n)
	}
	if peak := probe.peak.Load(); peak != 1 {
		t.Fatalf("✘ 默认（MultiThread=false）不应并行，并发度峰值=%d", peak)
	}

	_, stats := GetSystemStats()
	if stats.TimersAsync != 0 || stats.TimersInlineFallback != 0 {
		t.Fatalf("✘ 默认路径不应触碰执行池: %+v", stats)
	}
	// 执行池是懒创建的：没有任何 MultiThread 定时器时，worker 协程数必须为 0
	if now := runtime.NumGoroutine(); now > base+3 {
		t.Fatalf("✘ 默认路径新增了协程: base=%d, now=%d", base, now)
	}
	t.Logf("✔ 默认串行路径行为与协程数均未变化（峰值=%d, 执行=%d）", probe.peak.Load(), probe.execs.Load())
}

// ============================================================================
// 3. 重叠保护（单元级）：上一轮 Tick 未结束则跳过本轮，且跳过不消耗执行次数。
// ============================================================================

func TestMultiThread_SkipDoesNotConsumeCount(t *testing.T) {
	mgr := NewTimerManagerWithOptions(WithExecutorWorkers(2))
	defer mgr.Close()

	ct := &countingTimer{}
	if err := ct.Init(10, 3, ct); err != nil {
		t.Fatal(err)
	}
	defer ct.Remove()

	// 模拟「上一轮 Tick 还没结束」：先占住运行标记
	if !ct.tryBeginRun() {
		t.Fatal("首次 tryBeginRun 应成功")
	}
	before := ct.GetRemainingCount()
	task := timerTask{timer: ct, gen: ct.gen.Load(), mgr: mgr}

	mgr.runTick(task)

	if n := ct.ticked.Load(); n != 0 {
		t.Fatalf("✘ 重叠时不应执行 Tick，实际执行=%d", n)
	}
	if after := ct.GetRemainingCount(); after != before {
		t.Fatalf("✘ 跳过本轮不应消耗执行次数: before=%d, after=%d", before, after)
	}
	if s := mgr.GetStats().TimersSkipped; s != 1 {
		t.Fatalf("✘ 未记录重叠跳过: TimersSkipped=%d", s)
	}

	// 释放后应能正常进入
	ct.endRun()
	if !ct.tryBeginRun() {
		t.Fatal("✘ endRun 后应可再次进入执行")
	}
	ct.endRun()
	t.Logf("✔ 重叠跳过不消耗执行次数: 剩余次数保持 %d, skipped=%d", before, mgr.GetStats().TimersSkipped)
}

// ============================================================================
// 3b. 重叠保护（集成）：业务在 Tick 内 AddTimer 自己，制造重叠窗口时不得并发进入。
// ============================================================================

func TestMultiThread_NoOverlapWhenReAddInTick(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	probe := &concurrentProbe{hold: 60 * time.Millisecond}
	testProbe.Store(probe)
	defer testProbe.Store(nil)

	tm, err := NewTimer(10, InfiniteCount, &reAddTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	defer tm.Remove()

	if !waitUntil(2*time.Second, func() bool { return probe.execs.Load() >= 1 }) {
		t.Fatal("✘ 定时器未执行")
	}
	// 让 Tick 内的 AddTimer 制造的到达点落在下一轮 Tick 执行期间
	if !waitUntil(2*time.Second, func() bool {
		return GetDefaultManager().GetStats().TimersSkipped > 0
	}) {
		t.Fatal("✘ 未记录到重叠跳过：Tick 内 AddTimer 的重叠窗口未被保护")
	}
	if peak := probe.peak.Load(); peak != 1 {
		t.Fatalf("✘ 同一实例出现并发 Tick: 峰值=%d（期望 1）", peak)
	}
	t.Logf("✔ Tick 内重新入轮未造成并发 Tick: 峰值=%d, 跳过=%d",
		probe.peak.Load(), GetDefaultManager().GetStats().TimersSkipped)
}

// ============================================================================
// 4. 池禁用降级：执行池 worker 数为 0 时，MultiThread 定时器退回内联执行，不丢任务。
// ============================================================================

func TestMultiThread_PoolDisabledFallsBackInline(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	probe := &concurrentProbe{}
	testProbe.Store(probe)
	defer testProbe.Store(nil)

	mgr := NewTimerManagerWithOptions(WithExecutorWorkers(0))
	defer mgr.Close()

	tm, err := NewTimer(10, InfiniteCount, &flagTimer{})
	if err != nil {
		t.Fatal(err)
	}
	// 必须在 NewTimer 之后置位（NewTimer 会复位标记）；
	// TimerInterface 只暴露 MultiThread()，置位走 GetParent()（基类 Timer）
	tm.GetParent().SetMultiThread(true)
	if !tm.MultiThread() {
		t.Fatal("SetMultiThread(true) 未生效")
	}
	if err := mgr.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	defer tm.Remove()

	if !waitUntil(3*time.Second, func() bool { return probe.execs.Load() >= 3 }) {
		t.Fatalf("✘ 池禁用后定时器未被内联执行: %d", probe.execs.Load())
	}

	stats := mgr.GetStats()
	if stats.TimersAsync != 0 {
		t.Fatalf("✘ 池已禁用却出现并行执行: %+v", stats)
	}
	if stats.TimersInlineFallback == 0 {
		t.Fatalf("✘ 未统计内联降级次数: %+v", stats)
	}
	if peak := probe.peak.Load(); peak != 1 {
		t.Fatalf("✘ 内联降级路径不应并行: 峰值=%d", peak)
	}
	t.Logf("✔ 池禁用时正确降级为内联: 执行=%d, 降级=%d", probe.execs.Load(), stats.TimersInlineFallback)
}

// ============================================================================
// 5. 队列有界：队列满时 Submit 必须返回 false，由调用方降级为内联执行（不丢任务）。
// ============================================================================

func TestMultiThread_QueueFullReturnsFalse(t *testing.T) {
	mgr := NewTimerManagerWithOptions(WithExecutorWorkers(1))
	defer mgr.Close()

	release := make(chan struct{})
	bt := &blockingTimer{release: release}
	if err := bt.Init(1000, InfiniteCount, bt); err != nil {
		t.Fatal(err)
	}
	task := timerTask{timer: bt, gen: bt.gen.Load(), mgr: mgr}

	e := newTimerExecutor(mgr, 1, 1) // 1 个 worker + 容量 1 的队列
	defer e.Stop(200 * time.Millisecond)

	if !e.Submit(task) {
		t.Fatal("✘ 首个任务应投递成功")
	}
	if !waitUntil(2*time.Second, func() bool { return bt.started.Load() == 1 }) {
		t.Fatal("✘ worker 未开始执行首个任务")
	}
	// 唯一 worker 被占住，第 2 个任务只能进队列（容量 1）
	if !e.Submit(task) {
		t.Fatal("✘ 队列未满时第二个任务应入队成功")
	}
	// 队列已满：必须返回 false，调用方据此降级为内联执行
	if e.Submit(task) {
		t.Fatal("✘ 队列已满时 Submit 必须返回 false（否则会无界堆积）")
	}

	close(release)
	t.Logf("✔ 队列满正确返回 false: 队列=%d/%d", e.Len(), e.Cap())
}

// ============================================================================
// 6. 关闭不泄漏：管理器关闭后有界排空，worker 协程必须全部退出。
// ============================================================================

func TestMultiThread_NoGoroutineLeakAfterClose(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	probe := &concurrentProbe{}
	testProbe.Store(probe)
	defer testProbe.Store(nil)

	mgr := NewTimerManagerWithOptions(WithExecutorWorkers(4))
	tm, err := NewTimer(5, InfiniteCount, &multiThreadTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	defer tm.Remove()

	if !waitUntil(3*time.Second, func() bool { return probe.execs.Load() > 0 }) {
		t.Fatal("✘ 多线程定时器未执行")
	}

	before := runtime.NumGoroutine()
	mgr.Close() // 内部：close(closeChan) → wheelWG.Wait() → executor.Stop(有界)
	mgr.Close() // 幂等：重复关闭不得 panic

	if !waitUntil(3*time.Second, func() bool { return runtime.NumGoroutine() < before }) {
		t.Fatalf("✘ 关闭后 worker 协程未退出: before=%d, after=%d", before, runtime.NumGoroutine())
	}
	t.Logf("✔ 关闭后 worker 全部退出: before=%d, after=%d", before, runtime.NumGoroutine())
}

// ============================================================================
// 7. 对象池复用安全：MultiThread 标记不得串到下一个逻辑定时器。
// ============================================================================

func TestMultiThread_FlagResetOnPoolReuse(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	mgr := NewTimerManagerWithOptions(WithExecutorWorkers(2))
	defer mgr.Close()

	a, err := NewTimer(1000, 1, &flagTimer{})
	if err != nil {
		t.Fatal(err)
	}
	a.GetParent().SetMultiThread(true)
	if !a.MultiThread() {
		t.Fatal("SetMultiThread(true) 未生效")
	}
	if err := mgr.AddTimer(a); err != nil {
		t.Fatal(err)
	}
	a.Remove() // 归还对象池

	b, err := NewTimer(1000, 1, &flagTimer{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Remove()
	if b.MultiThread() {
		t.Fatal("✘ 对象池复用后 MultiThread 标记未复位，会把上一轮配置串到新业务")
	}
	t.Log("✔ 对象池复用未串号：新实例 MultiThread 恢复默认 false")
}

// ============================================================================
// 8. 全局默认 worker 数门面：设置值需被新建管理器继承，并收敛到合法区间。
// ============================================================================

func TestMultiThread_DefaultWorkersConfig(t *testing.T) {
	origin := GetDefaultExecutorWorkers()
	defer SetDefaultExecutorWorkers(origin)

	SetDefaultExecutorWorkers(3)
	if got := GetDefaultExecutorWorkers(); got != 3 {
		t.Fatalf("✘ SetDefaultExecutorWorkers 未生效: %d", got)
	}
	mgr := NewTimerManager()
	if got := mgr.executorWorkers; got != 3 {
		t.Fatalf("✘ 新建管理器未继承默认 worker 数: %d", got)
	}
	mgr.Close()

	SetDefaultExecutorWorkers(MaxConcurrentWorkers + 10)
	if got := GetDefaultExecutorWorkers(); got != MaxConcurrentWorkers {
		t.Fatalf("✘ worker 数未收敛到上限: %d", got)
	}

	// 关闭执行池：worker 数置 0，且不得因此 panic
	SetDefaultExecutorWorkers(0)
	mgr2 := NewTimerManager()
	if mgr2.executorWorkers != 0 {
		t.Fatalf("✘ 禁用执行池未生效: %d", mgr2.executorWorkers)
	}
	if e := mgr2.executorOrCreate(); e != nil {
		t.Fatal("✘ worker 数为 0 时不应创建执行池")
	}
	mgr2.Close()
	t.Log("✔ 默认 worker 数配置、上限收敛与禁用均正确")
}
