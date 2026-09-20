package localtimer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

	task := dispatchAndUnlink(m, TimerTypeMillisecond, tm, p)
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
	task2 := dispatchAndUnlink(m, TimerTypeMillisecond, tm, p)
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

	task := dispatchAndUnlink(m, TimerTypeMillisecond, tm, p)
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

	task := dispatchAndUnlink(m, TimerTypeMillisecond, tm, p)
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
	task := dispatchAndUnlink(m, TimerTypeMillisecond, tm, p)
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
