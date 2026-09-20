package localtimer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// ============================================================================
// 阶段 3（S10-S14）回归用例：
//   S10 临界区内 panic 不得把 wheelLock 永久扣住
//   S11 迁移/派发的归属改写顺序，不得把定时器留在两头无归属
//   S12 list.Add 失败必须回滚归属与计数
//   S13 beginTick 与对象池认领之间的 use-after-free 窗口
//   S14 同一实例的归还只能被认领一次
// ============================================================================

// panicParentTimer 在时间轮临界区（GetParent）内 panic，用于确定性检验 panic 后锁是否释放。
type panicParentTimer struct {
	Timer
	n atomic.Int64
}

func (p *panicParentTimer) GetParent() *Timer { panic("boom inside wheel critical section") }
func (p *panicParentTimer) Tick()             { p.n.Add(1) }

// failAddTimer 的 GetNode 会 panic，使 list.Add 走 recover 分支返回 false。
type failAddTimer struct{ *plainTimer }

func (f *failAddTimer) GetNode() *list.Node { panic("cannot resolve node") }

func newStandaloneWheel(config int64) *TimerWheel {
	return &TimerWheel{
		wheelConfig:  config,
		tickWheel:    list.NewList(),
		addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
	}
}

// wheelLockFree 在 d 内尝试获取轮写锁，成功说明锁未被扣住。
func wheelLockFree(w *TimerWheel, d time.Duration) bool {
	got := make(chan struct{})
	go func() {
		w.wheelLock.Lock()
		close(got)
		w.wheelLock.Unlock()
	}()
	select {
	case <-got:
		return true
	case <-time.After(d):
		return false
	}
}

// TestTimer_PanicInTickSectionKeepsLock 回归（S10）：processWheelTick 的临界区曾在
// Range 回调 panic 后跳过 wheelLock.Unlock（非 defer），一次异常就把该轮永久扣死，
// 后续 tick 全部阻塞、Close 也永远等不到轮协程退出。
func TestTimer_PanicInTickSectionKeepsLock(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	wheel := newStandaloneWheel(1)
	tm := &panicParentTimer{}
	tm.Timer.Init(20, InfiniteCount, tm)
	if !wheel.tickWheel.Add(tm) {
		t.Fatal("注入伪定时器失败")
	}
	wheel.timerCount.Add(1)

	func() {
		defer func() { _ = recover() }()
		m.processWheelTick(wheel, TimerTypeMillisecond)
	}()

	if !wheelLockFree(wheel, 2*time.Second) {
		t.Fatal("✘ processWheelTick panic 后 wheelLock 仍被占用（S10 回归失败）")
	}
}

// TestTimer_PanicInCleanupKeepsLock 回归（S10）：cleanupWheel 的持锁段同样必须 defer 解锁。
func TestTimer_PanicInCleanupKeepsLock(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	wheel := newStandaloneWheel(1)
	tm := &panicParentTimer{}
	tm.Timer.Init(20, InfiniteCount, tm)
	if !wheel.tickWheel.Add(tm) {
		t.Fatal("注入伪定时器失败")
	}
	wheel.timerCount.Add(1)

	func() {
		defer func() { _ = recover() }()
		m.cleanupWheel(wheel)
	}()

	if !wheelLockFree(wheel, 2*time.Second) {
		t.Fatal("✘ cleanupWheel panic 后 wheelLock 未释放（S10 回归失败）")
	}
}

// TestTimer_PanicInRemoveKeepsLock 回归（S10）：RemoveFromManager 的摘链段 panic
// 不得把定时器私有锁与轮锁一起扣住。
func TestTimer_PanicInRemoveKeepsLock(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*plainTimer](20, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	wheel := newStandaloneWheel(1)
	parent.wheel.Store(wheel)
	if !wheel.tickWheel.Add(tm) {
		t.Fatal("入链失败")
	}
	wheel.timerCount.Add(1)

	tm.Remove()

	if !wheelLockFree(wheel, 2*time.Second) {
		t.Fatal("✘ Remove 之后 wheelLock 被扣住")
	}
	done := make(chan struct{})
	go func() {
		parent.mu.Lock()
		parent.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ Remove 之后定时器私有锁被扣住（S10 回归失败）")
	}
}

// TestWheel_MarkerIsClaimable 回归（S11）：处于「投递途中」占位归属的节点必须能被
// 目标轮认领；真实归属其他轮时才拒绝重复入链。
func TestWheel_MarkerIsClaimable(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer[*plainTimer](10, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()

	src := newStandaloneWheel(1)
	dst := newStandaloneWheel(1000)

	parent.wheel.Store(migratingMarker)
	m.handleTimerAdd(dst, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})

	if parent.wheel.Load() != dst {
		t.Fatal("✘ 迁移中的节点未被目标轮认领（S11 回归失败）")
	}
	if c := dst.timerCount.Load(); c != 1 {
		t.Fatalf("✘ 认领后目标轮计数应为 1，实际 %d", c)
	}
	if dst.tickWheel.Length() != 1 {
		t.Fatal("✘ 认领后节点未真正入链")
	}

	// 已真实归属 dst 的重复入链必须被拒绝，且不污染另一个轮
	m.handleTimerAdd(src, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
	if parent.wheel.Load() != dst {
		t.Fatal("✘ 已入链节点被抢走归属")
	}
	if c := src.timerCount.Load(); c != 0 {
		t.Fatalf("✘ 重复入链让另一个轮计数变成 %d", c)
	}

	parent.wheel.Store(nil)
	tm.Remove()
}

// TestWheel_AddFailureKeepsCountAligned 回归（S12）：list.Add 返回 false 时必须
// 回滚归属，且绝不自增计数（否则 count 与链长永久漂移，形成幻影 +1）。
func TestWheel_AddFailureKeepsCountAligned(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	base, err := NewTimer[*plainTimer](10, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &failAddTimer{plainTimer: base}
	parent := base.GetParent()

	wheel := newStandaloneWheel(1)
	m.handleTimerAdd(wheel, timerTask{timer: wrapper, gen: parent.gen.Load(), mgr: m})

	if c := wheel.timerCount.Load(); c != 0 {
		t.Fatalf("✘ 入链失败却留下计数 %d（幻影 +1）", c)
	}
	if wheel.tickWheel.Length() != 0 {
		t.Fatalf("✘ 入链失败但链表非空: %d", wheel.tickWheel.Length())
	}
	if w := parent.wheel.Load(); w != nil {
		t.Fatalf("✘ 入链失败却残留归属 %v", w)
	}

	// 正常路径仍须严格配对
	m.handleTimerAdd(wheel, timerTask{timer: base, gen: parent.gen.Load(), mgr: m})
	if parent.wheel.Load() != wheel {
		t.Fatal("✘ 正常入链未登记归属")
	}
	if c := wheel.timerCount.Load(); c != int64(wheel.tickWheel.Length()) {
		t.Fatalf("✘ 正常入链后计数与链长不一致: count=%d len=%d", c, wheel.tickWheel.Length())
	}
	parent.wheel.Store(nil)
	base.Remove()
}

// TestTimer_NoTickAfterRelease 回归（S13）：isValid 通过后、beginTick 之前实例被
// 归还对象池，消费协程绝不能继续执行这个已不属于它的回调。
func TestTimer_NoTickAfterRelease(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()

	// 与当前代次完全一致的调度项：绕开 isValid 的代次闸门，专门考验 beginTick
	task := timerTask{timer: tm, gen: parent.gen.Load(), mgr: m}

	// 模拟「实例已被归还对象池、正等待下一个主人」
	parent.mu.Lock()
	parent.released.Store(true)
	parent.pendingRelease.Store(false)
	parent.inPool.Store(true)
	parent.mu.Unlock()

	m.executeTimer(task)

	if got := tm.n.Load(); got != 0 {
		t.Fatalf("✘ 已作废实例仍然执行了业务 Tick（执行 %d 次）", got)
	}
	if got := parent.inTick.Load(); got != 0 {
		t.Fatalf("✘ beginTick 失败后 inTick 仍为 %d，实例永久卡在在飞状态", got)
	}

	// 复位后交还给池，避免污染其他用例
	parent.mu.Lock()
	parent.released.Store(false)
	parent.inPool.Store(false)
	parent.mu.Unlock()
}

// TestTimer_BeginTickRejectsStaleGen beginTick 必须拒绝代次不匹配的在飞登记。
func TestTimer_BeginTickRejectsStaleGen(t *testing.T) {
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	current := parent.gen.Load()
	parent.nextGen() // 模拟新主人认领后推进代次

	if parent.beginTick(current) {
		t.Fatal("✘ 过期代次的调度项竟然登记成功")
	}
	if parent.inTick.Load() != 0 {
		t.Fatalf("✘ 拒绝后 inTick 未回退: %d", parent.inTick.Load())
	}
	if !parent.beginTick(parent.gen.Load()) {
		t.Fatal("✘ 当代调度项应能登记在飞")
	}
	parent.endTick()
	if parent.inTick.Load() != 0 {
		t.Fatalf("✘ endTick 后 inTick 未归零: %d", parent.inTick.Load())
	}
	tm.Remove()
}

// TestRelease_TwoEndTicksPoolOnce 回归（S14）：Tick 在飞期间业务 Remove 与
// endTick 的补做只能有一次真正落地，其余路径必须被仲裁挡下。
func TestRelease_TwoEndTicksPoolOnce(t *testing.T) {
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()

	before := GetTimerPoolStats().Puts

	// 制造「回调在飞 + 挂起归还」状态，再并发触发两条归还路径
	parent.inTick.Store(3)
	parent.pendingRelease.Store(true)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm.Remove()
			parent.endTick()
		}()
	}
	wg.Wait()

	if delta := GetTimerPoolStats().Puts - before; delta > 1 {
		t.Fatalf("✘ 同一实例被归还 %d 次（最多允许 1 次）", delta)
	}
	if !parent.inPool.Load() {
		t.Fatal("✘ 挂起的归还请求未落地，实例泄漏在使用者手里")
	}

	parent.mu.Lock()
	outcome := parent.requestReleaseLocked()
	parent.mu.Unlock()
	if outcome == releaseNow {
		t.Fatal("✘ 已在池中的实例被再次判定为可立即归还（双回池风险）")
	}
}

// TestWheel_MigrationNeverStrandsTimer 活性回归（S11 端到端）：
// 亚秒级定时器反复跨档迁移后仍必须持续 Tick，且不允许在轮里留下计数残影。
func TestWheel_MigrationNeverStrandsTimer(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	const num = 40
	timers := make([]*plainTimer, 0, num)
	for i := 0; i < num; i++ {
		tm, err := NewTimer[*plainTimer](20, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatalf("AddTimer 失败: %v", err)
		}
		timers = append(timers, tm)
	}

	// 反复把到期时间推到秒档再拉回毫秒档，强制每个周期都发生跨档迁移
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		now := time.Now().UnixMilli()
		for _, tm := range timers {
			tm.GetParent().nextTime.Store(now + 1500)
		}
		time.Sleep(60 * time.Millisecond)
		now = time.Now().UnixMilli()
		for _, tm := range timers {
			tm.GetParent().nextTime.Store(now + 20)
		}
		time.Sleep(60 * time.Millisecond)
	}

	stalled := 0
	for _, tm := range timers {
		if tm.n.Load() == 0 {
			stalled++
		}
	}
	if stalled > 0 {
		t.Fatalf("✘ %d/%d 个定时器在反复跨档迁移后彻底停摆（S11 回归失败）", stalled, num)
	}

	// 全部停表后各轮必须回到干净状态：迁移若丢过定时器，这里会留下计数残影
	for _, tm := range timers {
		tm.Pause()
	}
	time.Sleep(300 * time.Millisecond)
	for wi, w := range m.wheels {
		if c := w.timerCount.Load(); c != 0 {
			t.Fatalf("✘ 轮 %d 停表后仍有 %d 个定时器未回收（len=%d）", wi, c, w.tickWheel.Length())
		}
		if n := w.tickWheel.Length(); n != 0 {
			t.Fatalf("✘ 轮 %d 停表后链表仍有 %d 个节点", wi, n)
		}
	}

	for _, tm := range timers {
		tm.Remove()
	}
}

// TestWheel_MigrationOrphanProbe 是 S11 的确定性探测：反复执行「源轮 tick 触发跨档迁移
// → 目标轮立即认领」，统计迁移后两头无归属的丢单次数。
// 修复前该顺序为「先投通道、后置 wheel=nil」，认领方有机会在归属仍指向源轮时
// 把调度项判为重复入链直接丢弃；实测 5000 轮可复现丢单，修复后恒为 0。
func TestWheel_MigrationOrphanProbe(t *testing.T) {
	m := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, DefaultWheelCount),
	}
	src := newStandaloneWheel(1)
	dst := newStandaloneWheel(util.MILLISECONDS_OF_SECOND)
	m.wheels[TimerTypeMillisecond] = src
	m.wheels[TimerTypeSecond] = dst

	const rounds = 5000
	orphan := 0
	for round := 0; round < rounds; round++ {
		tm, err := NewTimer[*plainTimer](20, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		parent := tm.GetParent()
		m.handleTimerAdd(src, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		if parent.wheel.Load() != src {
			t.Fatalf("第 %d 轮：未能建立初始归属", round)
		}

		// 推到秒档，制造一次跨档迁移
		parent.nextTime.Store(time.Now().UnixMilli() + 1500)

		var wg sync.WaitGroup
		var claimed atomic.Bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			task := <-dst.addTimerChan
			m.handleTimerAdd(dst, task)
			claimed.Store(parent.wheel.Load() == dst)
		}()

		m.processWheelTick(src, TimerTypeMillisecond)
		wg.Wait()

		if !claimed.Load() {
			orphan++
		}
		parent.wheel.Store(nil)
		tm.Remove()
	}

	if orphan > 0 {
		t.Fatalf("✘ %d/%d 次跨档迁移后定时器两头无归属（永久停摆）", orphan, rounds)
	}
}
