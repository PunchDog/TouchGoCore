package localtimer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// ============================================================================
// 阶段 8（S34-S39）回归用例：
//   S34 Run 与 NewTimerManager 的注册竞态不得留下「未 Close 就被抹掉」的管理器
//   S35 Close 必须自摘注册表，并排空入队通道残留（计数守恒、活跃标记回滚）
//   S36 收尾预算：业务 Tick 卡死时进程仍要停得下来，超时强制回池
//   S37 已关闭管理器的陈旧调度项不得跨 Run 执行；ctx 取消要如实反映到状态；
//       调度通道后建时消费协程必须接手
//   S38 背压告警按轮限频；「水位过高」与「确有丢弃」两种事件分开
//   S39 续期彻底失败要回池并可被业务接管；TimersRemoved 覆盖全路径；
//       UID 跨管理器唯一；门面变参注册失败回滚
// ============================================================================

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

// within 断言 fn 在 d 内返回，否则视为挂死
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("✘ %s 未在 %v 内返回（疑似挂死）", what, d)
	}
}

// newTestWheel 造一个没有协程消费的独立时间轮，便于确定性填满它的入队通道
func newTestWheel(mgr *TimerManager, config int64, chanCap int) *TimerWheel {
	return &TimerWheel{
		wheelConfig:  config,
		tickWheel:    list.NewList(),
		addTimerChan: make(chan timerTask, chanCap),
		mgr:          mgr,
	}
}

// newOfflineManager 造一个没有轮协程运行的管理器：入队通道没人消化，
// 因此能被确定性地灌到水位线或直接灌满。各轮都必须建好——
// CloseCtx / drainPendingAdds 会遍历整张轮表。
func newOfflineManager(chanCap int) *TimerManager {
	mgr := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, DefaultWheelCount),
	}
	for i, config := range []int64{
		1,
		util.MILLISECONDS_OF_SECOND,
		util.MILLISECONDS_OF_MINUTE,
		util.MILLISECONDS_OF_10_MINUTE,
		util.MILLISECONDS_OF_HOUR,
	} {
		mgr.wheels[i] = newTestWheel(mgr, config, chanCap)
	}
	return mgr
}

// chainTimer 把定时器手工挂进轮里，并可选把到期时间提前
func chainTimer(t *testing.T, wheel *TimerWheel, tm TimerInterface, expired bool) *Timer {
	t.Helper()
	parent := tm.GetParent()
	node, ok := tm.(list.INode)
	if !ok {
		t.Fatal("定时器未实现 list.INode")
	}
	if !wheel.tickWheel.Add(node) {
		t.Fatal("入链失败")
	}
	parent.wheel.Store(wheel)
	wheel.timerCount.Add(1)
	if expired {
		parent.nextTime.Store(util.CurrentMS() - 10)
	}
	return parent
}

// ----------------------------------------------------------------------------
// S34 / S35
// ----------------------------------------------------------------------------

// TestManagerCloseUnregistersItself 回归（S35）：Close 必须把自身从注册表摘掉。
// 自摘让「离开注册表」与「已经 Close」严格同义，Run/TimeStop 才不可能抹掉一个
// 时间轮协程仍在跑的管理器。
func TestManagerCloseUnregistersItself(t *testing.T) {
	m := NewTimerManager()
	if _, ok := timerManagerMap.Load(m); !ok {
		t.Fatal("✘ 新建管理器未登记进注册表")
	}

	m.Close()

	if _, ok := timerManagerMap.Load(m); ok {
		t.Fatal("✘ Close 后管理器仍留在注册表中")
	}
}

// TestRegistryFreezesRegistrationDuringBatchClose 回归（S34）：批量关闭取待关清单
// 的窗口内必须冻结注册。否则并发 NewTimerManager 落在清单外又被随后的清空抹掉，
// 它名下 5 个时间轮协程 + 5 个 ticker 永久泄漏。
func TestRegistryFreezesRegistrationDuringBatchClose(t *testing.T) {
	registered := make(chan *TimerManager, 1)

	timerRegistryMu.Lock()
	go func() {
		registered <- NewTimerManager()
	}()

	select {
	case mgr := <-registered:
		timerRegistryMu.Unlock()
		mgr.Close()
		t.Fatal("✘ 注册未被冻结窗口阻挡：清空注册表会抹掉这个从未 Close 的管理器")
	case <-time.After(200 * time.Millisecond):
	}
	timerRegistryMu.Unlock()

	select {
	case mgr := <-registered:
		defer mgr.Close()
		if _, ok := timerManagerMap.Load(mgr); !ok {
			t.Fatal("✘ 解冻后管理器仍未登记进注册表")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("✘ 解冻后 NewTimerManager 仍未完成注册")
	}
}

// TestDrainPendingAddsCountsResidualTasks 回归（S35）：轮协程退出后入队通道里的
// 残留调度项永远不会被消化，必须排空、回滚活跃标记并计入统计，否则实例既
// 「幻影活跃」又回不了对象池，监控上也看不出这笔损失。
func TestDrainPendingAddsCountsResidualTasks(t *testing.T) {
	mgr := newOfflineManager(4)
	wheel := mgr.wheels[0]

	tm, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	parent.isActive.Store(true)

	wheel.addTimerChan <- timerTask{timer: tm, gen: parent.gen.Load(), mgr: mgr}

	before := mgr.GetStats()
	mgr.drainPendingAdds()
	after := mgr.GetStats()

	if after.TimersDropped != before.TimersDropped+1 {
		t.Fatalf("✘ 残留调度项未计入 TimersDropped: before=%d after=%d", before.TimersDropped, after.TimersDropped)
	}
	if after.TimersRemoved != before.TimersRemoved+1 {
		t.Fatalf("✘ 残留调度项未计入 TimersRemoved: before=%d after=%d", before.TimersRemoved, after.TimersRemoved)
	}
	if parent.IsActive() {
		t.Fatal("✘ 残留调度项被丢弃后定时器仍标记为活跃（幻影活跃）")
	}
	if n := len(wheel.addTimerChan); n != 0 {
		t.Fatalf("✘ 入队通道未排空: 残留=%d", n)
	}
}

// ----------------------------------------------------------------------------
// S36
// ----------------------------------------------------------------------------

// TestCleanupWheelHonorsCloseBudget 回归（S36 M3）：收尾预算已耗尽时不得再执行
// 业务 Tick，必须把剩余定时器强制归还对象池并放弃回调。
func TestCleanupWheelHonorsCloseBudget(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	wheel := newTestWheel(m, 1, 4)
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 实例可能来自对象池，n 是业务字段（池不复位），起点以认领时刻为准
	startTicks := tm.n.Load()
	chainTimer(t, wheel, tm, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预算已耗尽
	m.closeCtx.Store(&ctx)

	putsBefore := GetTimerPoolStats().Puts
	within(t, 3*time.Second, "cleanupWheel 在预算耗尽后仍等业务 Tick", func() {
		m.cleanupWheel(wheel, TimerTypeMillisecond)
	})

	if n := tm.n.Load(); n != startTicks {
		t.Fatalf("✘ 预算耗尽后仍执行了业务 Tick: 新增=%d 起点=%d", n-startTicks, startTicks)
	}
	if tm.GetParent().IsActive() {
		t.Fatal("✘ 强制废弃后定时器仍活跃")
	}
	waitUntil(t, 2*time.Second, "超时收尾的定时器未归还对象池", func() bool {
		return GetTimerPoolStats().Puts > putsBefore
	})
}

// TestCloseCtxReturnsOnBudgetTimeout 回归（S36 M3）：有轮协程卡住时 CloseCtx 必须
// 在预算内返回并报告超时，否则 TimeStop 永久挂起、进程停不下来。
func TestCloseCtxReturnsOnBudgetTimeout(t *testing.T) {
	m := NewTimerManager()
	// 模拟一个卡在业务 Tick 里、再也退不出的轮协程
	m.wheelWG.Add(1)
	defer m.wheelWG.Done()

	budget, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var timedOut bool
	start := util.CurrentMS()
	within(t, 3*time.Second, "CloseCtx 等待卡死的时间轮", func() {
		timedOut = m.CloseCtx(budget)
	})
	elapsed := util.CurrentMS() - start

	if !timedOut {
		t.Fatal("✘ 收尾超时未被报告（调用方无从得知有定时器被强制回收）")
	}
	if elapsed > 2000 {
		t.Fatalf("✘ CloseCtx 未遵守收尾预算: 耗时=%dms", elapsed)
	}
}

// TestAbandonExpiredReleasesToPool 回归（S36）：超时放弃的实例必须真的回池，
// 否则它们离开了时间轮又留在池外，永久占着内存。
func TestAbandonExpiredReleasesToPool(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	tasks := []timerTask{{timer: tm, gen: parent.gen.Load(), mgr: m}}

	putsBefore := GetTimerPoolStats().Puts
	within(t, 2*time.Second, "abandonExpired", func() {
		m.abandonExpired(TimerTypeMillisecond, tasks)
	})

	if parent.IsActive() {
		t.Fatal("✘ 强制废弃后定时器仍活跃")
	}
	waitUntil(t, 2*time.Second, "强制废弃的实例未归还对象池", func() bool {
		return GetTimerPoolStats().Puts > putsBefore
	})
}

// ----------------------------------------------------------------------------
// S37
// ----------------------------------------------------------------------------

// TestTimeTickSkipsClosedManagerTask 回归（S37 M6）：全局调度通道跨生命周期共享，
// 上一轮（或停机途中）遗留的调度项绝不能被新一轮消费协程执行。
func TestTimeTickSkipsClosedManagerTask(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	stale := NewTimerManager()
	stale.Close() // 该调度项的归属管理器已关闭

	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	parent.isActive.Store(true)
	startTicks := tm.n.Load() // 池实例的业务计数可能非 0

	currentTimerChannel() <- timerTask{timer: tm, gen: parent.gen.Load(), mgr: stale}

	time.Sleep(300 * time.Millisecond)
	if n := tm.n.Load(); n != startTicks {
		t.Fatalf("✘ 已关闭管理器的陈旧调度项被执行: 新增=%d 起点=%d", n-startTicks, startTicks)
	}
}

// TestRunCtxCancelReflectedInIsSystemRunning 回归（S37 M7）：ctx 取消后消费协程退出，
// IsSystemRunning 必须转为 false。否则业务照旧 AddTimer，调度项默默堆到通道满。
func TestRunCtxCancelReflectedInIsSystemRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Run(ctx)
	defer TimeStop(context.Background())

	if !IsSystemRunning() {
		t.Fatal("✘ Run 之后系统应处于运行状态")
	}

	cancel()
	waitUntil(t, 3*time.Second, "ctx 取消后 IsSystemRunning 未转为 false", func() bool {
		return !IsSystemRunning()
	})
	if rt := timerRT.Load(); rt != nil {
		t.Fatal("✘ ctx 取消后 timerRT 仍指向已退出的生命周期")
	}
}

// TestTimeTickPicksUpRecreatedChannel 回归（S37 M8）：消费协程先于调度通道启动时
// 不得直接退出，必须周期重取快照接手新建的通道。
func TestTimeTickPicksUpRecreatedChannel(t *testing.T) {
	TimeStop(context.Background()) // 清掉可能的残留消费者与通道

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := &timerRuntime{closech: make(chan struct{}), ctx: ctx}
	timerRT.Store(rt)
	exited := make(chan struct{})
	go func() {
		timeTick(rt)
		close(exited)
	}()

	// 修复前：timeTick 在此刻读到 nil 通道就直接 return 了
	ensureTimerChannel()

	m := NewTimerManager()

	// 复位业务计数：实例可能来自对象池，下面的 waitUntil 以 0 为基线
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, func(p *plainTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 3*time.Second, "调度通道后建时消费协程没有接手（定时器永不触发）", func() bool {
		return tm.n.Load() > 0
	})

	rt.shutdown()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("✘ 消费协程未响应关闭信号")
	}
	TimeStop(context.Background())
}

// ----------------------------------------------------------------------------
// S38
// ----------------------------------------------------------------------------

// TestAddTimerLockedWarningKinds 回归（S38 M2）：入队成功但水位过高时不得报成
// 丢弃事件；真正丢弃时告警必须作为事件交回锁外的调用方，同时回滚活跃标记。
func TestAddTimerLockedWarningKinds(t *testing.T) {
	// 无轮协程消费的管理器：入队通道可以被确定性地灌到 90% 与 100%
	mgr := newOfflineManager(10)

	tm, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	// addTimerLocked 按定时器的剩余间隔选轮，水位必须灌在它实际会落到的那一轮
	wheel := mgr.wheels[parent.GetType()]

	for i := 0; i < 9; i++ {
		wheel.addTimerChan <- timerTask{}
	}

	var warn *addTimerWarning
	parent.mu.Lock()
	err, warn = mgr.addTimerLocked(tm, parent, 0)
	parent.mu.Unlock()

	if err != nil {
		t.Fatalf("✘ 水位过高时入队应成功: %v", err)
	}
	if warn == nil {
		t.Fatal("✘ 水位过高未产生告警（修复前是在持锁期间直接打日志）")
	}
	if warn.dropped != 0 || warn.used != 9 || warn.capacity != 10 {
		t.Fatalf("✘ 水位告警字段错误: dropped=%d used=%d cap=%d", warn.dropped, warn.used, warn.capacity)
	}

	// 上面成功入队的那一条已经把通道推到临界水位；补足到容量上限，
	// 下一次入队必然落到丢弃分支（这里只补空位，绝不写出会阻塞的发送）。
	fullWheel := mgr.wheels[parent.GetType()]
	for i := len(fullWheel.addTimerChan); i < cap(fullWheel.addTimerChan); i++ {
		fullWheel.addTimerChan <- timerTask{}
	}
	parent.mu.Lock()
	err, warn = mgr.addTimerLocked(tm, parent, 0)
	parent.mu.Unlock()

	if !errors.Is(err, ErrTimerChannelFull) {
		t.Fatalf("✘ 通道满时应返回 ErrTimerChannelFull: %v", err)
	}
	if warn == nil || warn.dropped != 1 {
		t.Fatalf("✘ 丢弃事件未登记: %+v", warn)
	}
	if parent.IsActive() {
		t.Fatal("✘ 入队失败后仍标记为活跃（幻影活跃）")
	}
}

// TestWheelShouldWarnIsPerWheel 回归（S38 L9）：限频必须按轮独立计数，
// 否则繁忙的毫秒轮会把安静的小时轮「正在丢调度项」的唯一线索吞掉。
func TestWheelShouldWarnIsPerWheel(t *testing.T) {
	busy := newTestWheel(nil, 1, 4)
	quiet := newTestWheel(nil, util.MILLISECONDS_OF_HOUR, 4)

	if !wheelShouldWarn(busy) {
		t.Fatal("✘ 首次告警被限频拦下")
	}
	if wheelShouldWarn(busy) {
		t.Fatal("✘ 同一时间轮一秒内重复告警（限频失效）")
	}
	if !wheelShouldWarn(quiet) {
		t.Fatal("✘ 别的轮被 busy 轮的限频时间戳吞掉告警")
	}
}

// ----------------------------------------------------------------------------
// S39
// ----------------------------------------------------------------------------

// TestNotifyTimerLostReleasesAndNotifies 回归（S39 M12）：续期重试彻底失败不能只
// 留一条日志——定时器已经永久失联，必须回池并给业务一个可接管的回调。
func TestNotifyTimerLostReleasesAndNotifies(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	uid := tm.GetUID()

	var (
		hookOnce sync.Once
		got      TimerLostInfo
	)
	SetOnTimerLost(func(info TimerLostInfo) {
		hookOnce.Do(func() { got = info })
	})
	defer SetOnTimerLost(nil)

	putsBefore := GetTimerPoolStats().Puts

	// 复刻 executeTimer 的上下文：回调在飞期间作废，归还由 endTick 落地
	if !parent.beginTick(parent.gen.Load()) {
		t.Fatal("✘ beginTick 失败，无法模拟回调在飞状态")
	}
	m.notifyTimerLost(tm, parent, ErrTimerChannelFull)
	parent.endTick()

	if got.UID != uid || !errors.Is(got.Cause, ErrTimerChannelFull) {
		t.Fatalf("✘ OnTimerLost 回调未收到正确信息: %+v", got)
	}
	if got.Manager != m {
		t.Fatal("✘ OnTimerLost 回调的归属管理器错误")
	}
	if parent.IsActive() {
		t.Fatal("✘ 续期彻底失败后定时器仍活跃")
	}
	waitUntil(t, 2*time.Second, "续期失败的定时器未归还对象池", func() bool {
		return GetTimerPoolStats().Puts > putsBefore
	})
}

// TestStatsTimersRemovedCoversAllPaths 回归（S39 M13）：Remove / Pause 都必须体现在
// TimersRemoved 上，否则监控里这个指标永远只增不起来，丢定时器也看不出来。
func TestStatsTimersRemovedCoversAllPaths(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "定时器未入链", func() bool { return m.GetTimerCount() > 0 })

	before := m.GetStats().TimersRemoved
	tm.Remove()
	if after := m.GetStats().TimersRemoved; after <= before {
		t.Fatalf("✘ Remove 未计入 TimersRemoved: before=%d after=%d", before, after)
	}

	tm2, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddTimer(tm2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "第二个定时器未入链", func() bool { return m.GetTimerCount() > 0 })

	before = m.GetStats().TimersRemoved
	tm2.Pause()
	if after := m.GetStats().TimersRemoved; after <= before {
		t.Fatalf("✘ Pause 未计入 TimersRemoved: before=%d after=%d", before, after)
	}
	tm2.Remove()
}

// TestUIDUniqueAcrossManagers 回归（S39 M15）：UID 必须跨管理器全局唯一，
// 否则两个管理器各自的日志与监控里同一个 uid 指向两个业务，无法排障。
func TestUIDUniqueAcrossManagers(t *testing.T) {
	m1 := NewTimerManager()
	defer m1.Close()
	m2 := NewTimerManager()
	defer m2.Close()

	t1, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m1.AddTimer(t1); err != nil {
		t.Fatal(err)
	}
	t2, err := NewTimer[*plainTimer](500, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.AddTimer(t2); err != nil {
		t.Fatal(err)
	}

	if t1.GetUID() == 0 || t2.GetUID() == 0 {
		t.Fatalf("✘ UID 未分配: %d / %d", t1.GetUID(), t2.GetUID())
	}
	if t1.GetUID() == t2.GetUID() {
		t.Fatalf("✘ 跨管理器 UID 冲突: %d", t1.GetUID())
	}
}

// TestFacadeAddTimerRollsBackOnFailure 回归（S39 M16）：变参注册中途失败时，
// 已经注册成功的前半批必须撤下，否则调用方拿到错误却发现一半定时器已经在跑。
func TestFacadeAddTimerRollsBackOnFailure(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	a, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := AddTimer(a, b, nil); !errors.Is(err, ErrTimerNilParent) {
		t.Fatalf("✘ 变参注册未拒绝 nil 项: %v", err)
	}

	// 实例可能来自对象池，n 是业务字段（池不复位），只断言「注册失败后没有新增」
	na0, nb0 := a.n.Load(), b.n.Load()
	time.Sleep(300 * time.Millisecond)
	if na, nb := a.n.Load(), b.n.Load(); na != na0 || nb != nb0 {
		t.Fatalf("✘ 注册失败后前半批定时器仍在跑: a+%d b+%d", a.n.Load()-na0, b.n.Load()-nb0)
	}
	if a.GetParent().IsActive() || b.GetParent().IsActive() {
		t.Fatal("✘ 回滚后定时器仍标记活跃")
	}
	// 回滚只停调度：所有权仍在调用方手里，随后可自行 Remove 作废
	a.Remove()
	b.Remove()
}

// TestFacadeAddTimerRollbackKeepsOwnership 回归（S39 M16）：回滚必须是 Pause 语义，
// 不能顺手把调用方还在引用的实例交给对象池。
func TestFacadeAddTimerRollbackKeepsOwnership(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	a, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.GetParent().abandoned() {
		t.Fatal("✘ 新建实例被标记为已作废")
	}

	putsBefore := GetTimerPoolStats().Puts

	// 末项非法：a 已经注册成功，必须走回滚分支
	if err := AddTimer(a, nil); !errors.Is(err, ErrTimerNilParent) {
		t.Fatalf("✘ 变参注册未拒绝 nil 项: %v", err)
	}
	if a.GetParent().abandoned() {
		t.Fatal("✘ 回滚把仍在调用方手里的实例送进了对象池（应为 Pause 语义）")
	}
	if GetTimerPoolStats().Puts != putsBefore {
		t.Fatalf("✘ 回滚把实例归还了对象池: Puts %d -> %d", putsBefore, GetTimerPoolStats().Puts)
	}
	// 暂停后仍可复活：证明所有权没被交出去
	if err := AddTimer(a); err != nil {
		t.Fatalf("✘ 回滚后无法复活定时器: %v", err)
	}
	a.Remove()
}
