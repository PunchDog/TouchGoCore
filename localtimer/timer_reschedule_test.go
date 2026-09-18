package localtimer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// 续期（reschedule）路径的回归验证。
//
// 这些能力与「多线程执行池」无关，在执行池被移除后仍需保持有效：
//   1. NextIntervaler —— 业务在续期前重算下一次间隔（墙钟对齐型定时器）；
//   2. Timer.SetInterval —— 非正间隔必须被忽略，避免时间轮收到非法值；
//   3. AddTimer 无条件推进代次 —— 保证每次 AddTimer 产生的调度项 gen 唯一；
//   4. 门面 AddTimer 支持一次传入多个定时器。
//
// 运行方式：go test ./localtimer -run 'TestReschedule_|TestTimer_SetInterval|TestAddTimer_' -v
// ============================================================================

// nextIntervalTimer 是墙钟对齐型定时器的测试替身：
// 下一次间隔由 NextInterval() 动态给出，而不是固定 interval。
type nextIntervalTimer struct {
	Timer
	ticked atomic.Int64
	next   atomic.Int64
}

func (n *nextIntervalTimer) Tick() { n.ticked.Add(1) }

func (n *nextIntervalTimer) NextInterval() int64 { return n.next.Load() }

// TestReschedule_NextIntervalApplied 验证续期时会应用 NextIntervaler 重算出的间隔。
//
// 没有这个能力时，业务只能在 Tick 里自行 Init + AddTimer 重排，与续期路径末尾的
// AddTimer 构成「双写」：两次调用都会推进代次，后投递的调度项会把先投递的判为
// 过期丢弃，业务的重排被静默作废。
func TestReschedule_NextIntervalApplied(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	// 注意：NewTimer 从对象池返回同类型的新实例，必须用返回值而非入参指针
	tm, err := NewTimer(5, InfiniteCount, &nextIntervalTimer{})
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}
	nt := tm.(*nextIntervalTimer)

	if err := AddTimer(tm); err != nil {
		t.Fatalf("AddTimer 失败: %v", err)
	}

	const want int64 = 120
	nt.next.Store(want)

	// 首次 Tick 后的续期即应把 interval 改写为 want
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if nt.ticked.Load() > 0 && nt.GetParent().GetInterval() == want {
			t.Logf("✔ NextIntervaler 生效: 续期间隔已改写为 %dms", want)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("✘ 续期未应用 NextInterval: ticked=%d interval=%d want=%d",
		nt.ticked.Load(), nt.GetParent().GetInterval(), want)
}

// TestReschedule_NextIntervalNonPositiveKeepsCurrent 验证 NextInterval 返回 <=0
// 时不覆盖当前间隔 —— 业务无需区分自己是否处于墙钟对齐模式。
func TestReschedule_NextIntervalNonPositiveKeepsCurrent(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	const base int64 = 20
	tm, err := NewTimer(base, InfiniteCount, &nextIntervalTimer{})
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}
	nt := tm.(*nextIntervalTimer)
	if err := AddTimer(tm); err != nil {
		t.Fatalf("AddTimer 失败: %v", err)
	}

	// next 保持 0：应一直沿用 base
	time.Sleep(200 * time.Millisecond)

	if nt.ticked.Load() == 0 {
		t.Fatal("✘ 定时器未被执行，调度链路异常")
	}
	if got := nt.GetParent().GetInterval(); got != base {
		t.Fatalf("✘ NextInterval 返回 0 时不应覆盖 interval: got=%d want=%d", got, base)
	}
	t.Logf("✔ NextInterval<=0 时沿用原间隔 %dms", base)
}

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
// RemoveFromManager 只在「原本是活跃」时才推进代次；业务先 Remove 把定时器置为
// inactive、再 AddTimer 复活时它会提前返回。若此时不额外推进一次代次，在途的旧
// 调度项会与新调度项共享同一个 gen，被 isValid 误判为有效，导致同一实例重复调度。
func TestAddTimer_AlwaysAdvancesGen(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	tm, err := NewTimer(1000, InfiniteCount, &genProbeTimer{})
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

	// 复活路径：先 Remove 置为 inactive，再 AddTimer
	tm.RemoveFromManager(false)
	if p.IsActive() {
		t.Fatal("RemoveFromManager 后应为 inactive")
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

	a, err := NewTimer(1000, InfiniteCount, &genProbeTimer{})
	if err != nil {
		t.Fatalf("NewTimer 失败: %v", err)
	}
	b, err := NewTimer(1000, InfiniteCount, &genProbeTimer{})
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
