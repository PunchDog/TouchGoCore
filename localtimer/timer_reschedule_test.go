package localtimer

import (
	"context"
	"sync/atomic"
	"testing"
)

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
