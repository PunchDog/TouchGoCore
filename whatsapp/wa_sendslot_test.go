package whatsapp

// ============================================================================
// 速率闸的排队语义（waitSendSlot）
//
// 2026-10-30 之前这里是「窗口满就拒单」：超配额的那条直接以「超出单账号发送
// 速率上限（send_rate）」失败。60|1（窗口 60 秒、配额 1 条）下，一批 100 条
// 只有第 1 条能出去、其余 99 条全失败 —— 运营看到的是「群发一直失败、单发却
// 正常」（单条发送走 WhatsappSendMessage，不过这道闸）。
//
// 现在的语义是「窗口满就等到下一个槽位」：整批最终都会发完，只是慢。
// 这几条用例分别钉住：等待时长怎么算、真会等、认 ctx、以及「不限」的两种写法。
//
// 窗口参数用毫秒级而不是走 send_rate 配置：配置的最小粒度是 1 秒，
// 真按 60|1 跑一条用例要等 60 秒。waitSendSlot 的 window 本来就是入参，
// 直接喂 200ms 既测到同一段代码，又不让测试拖慢。
// ============================================================================

import (
	"context"
	"testing"
	"time"

	"touchgocore/config"
)

// TestSendSlotWaitDuration 等待时长必须精确落在「第 (len-max) 条过期」那一刻。
//
// 不写成「固定等一个 window」：配置调小时那样会白等，配置调大时又等不够
// （等不够就会立刻重试、再被拒，空转一轮）。这个下标计算是排队语义的心脏。
func TestSendSlotWaitDuration(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	window := 60 * time.Second
	now := t0.Add(25 * time.Second)
	// 窗口内三条历史记账（按时间升序，与真实追加顺序一致）。
	w := &waClientWrapper{sendWindow: []time.Time{t0, t0.Add(10 * time.Second), t0.Add(20 * time.Second)}}

	cases := []struct {
		name string
		max  int
		want time.Duration
	}{
		// max=1：要让窗口内只剩 0 条 ⇒ 等最后一条（idx=2）过期 ⇒ t0+20s+60s = t0+80s。
		{"配额 1 要等最后一条过期", 1, 55 * time.Second},
		// max=3：只剩 2 条即可 ⇒ 等第一条（idx=0）过期 ⇒ t0+60s。
		{"配额 3 等最早一条过期", 3, 35 * time.Second},
		// max 比窗口内条数还大：clamp 到 idx=0，不必等满。
		{"配额大于在途条数", 5, 35 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := w.sendSlotWaitDuration(now, window, c.max); got != c.want {
				t.Fatalf("✘ 等待时长 = %s，期望 %s", got, c.want)
			}
		})
	}

	// 空窗口不该算出任何等待（否则每次首条都要白睡）。
	empty := &waClientWrapper{}
	if got := empty.sendSlotWaitDuration(now, window, 1); got != 0 {
		t.Fatalf("✘ 空窗口等待 = %s，期望 0", got)
	}
}

// TestWaitSendSlotQueuesInsteadOfRejecting 窗口满时必须等，而不是返回失败。
//
// 这是本次改动的核心断言：先占满唯一配额，再要一个槽位 —— 旧实现立刻
// 返回「超出上限」，新实现睡到第一条过期后放行。同时验证记账被剪枝
// （等待后窗口里只剩新的那一条，不是两条）。
func TestWaitSendSlotQueuesInsteadOfRejecting(t *testing.T) {
	const window = 200 * time.Millisecond
	w := &waClientWrapper{}

	if !w.takeSendSlot(time.Now(), window, 1) {
		t.Fatal("首条应当放行")
	}
	// 紧接着要第二个槽位：窗口里已有 1 条、配额 1 ⇒ 必须等。
	start := time.Now()
	if err := w.waitSendSlot(context.Background(), window, 1); err != nil {
		t.Fatalf("✘ 等待应成功，实得 %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < window/2 {
		t.Fatalf("✘ 只等了 %s 就放行（窗口 %s）：配额还没让出来，这是超发", elapsed, window)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("✘ 等了 %s，远超窗口 %s", elapsed, window)
	}

	w.mu.Lock()
	n := len(w.sendWindow)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("✘ 等待后窗口内应只剩新记的 1 条（旧的已过期剪掉），实得 %d 条", n)
	}
}

// TestWaitSendSlotHonorsCtx ctx 被打断时必须立刻返回错误，不能把协程钉死。
//
// 窗口上限是 86400 秒（24 小时）。不认 ctx 的话，一个配错的大窗口会让批次
// 协程睡一整天，而账号串行闸还被它占着 —— 那个号整天发不出任何东西。
func TestWaitSendSlotHonorsCtx(t *testing.T) {
	w := &waClientWrapper{}
	if !w.takeSendSlot(time.Now(), time.Hour, 1) {
		t.Fatal("首条应当放行")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := w.waitSendSlot(ctx, time.Hour, 1)
	if err == nil {
		t.Fatal("✘ ctx 已取消，等待应返回错误")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("✘ ctx 已取消却等了 %s：等待没有认 ctx", d)
	}
}

// TestWaitSendSlotUnlimited 两种「不限」写法都必须直接放行、不记账。
//
// window<=0 / max<=0 是 Effective* 归一后的「关闭限速」形态（例如后台把条数
// 配成 0 的极端情形）。这种情况下若还去等，会把一个明确关掉的闸变成永久阻塞。
func TestWaitSendSlotUnlimited(t *testing.T) {
	for _, c := range []struct {
		name   string
		window time.Duration
		max    int
	}{
		{"窗口为 0", 0, 1},
		{"条数为 0", time.Minute, 0},
		{"两者都为 0", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := &waClientWrapper{}
			start := time.Now()
			if err := w.waitSendSlot(context.Background(), c.window, c.max); err != nil {
				t.Fatalf("✘ 不限形态不该返回错误：%v", err)
			}
			if d := time.Since(start); d > 500*time.Millisecond {
				t.Fatalf("✘ 不限形态不该等待，实等 %s", d)
			}
			w.mu.Lock()
			n := len(w.sendWindow)
			w.mu.Unlock()
			if n != 0 {
				t.Fatalf("✘ 不限形态不该记账，窗口内实得 %d 条", n)
			}
		})
	}
}

// TestSendRateOverrideWinsOverConfig 注入的速率必须压过配置文件的 send_rate。
//
// 这是「后台设置优先」在核心侧的唯一落点：业务侧解析出最终值后注入，核心只认它。
// 不生效的后果是最难查的那种 —— 业务侧按后台值分批、核心按文件值放行，
// 表现是「后台明明调了、实际节奏没变」，且不报任何错。
func TestSendRateOverrideWinsOverConfig(t *testing.T) {
	w := &waClientWrapper{cfg: &config.WhatsappSessionsConfig{SendRate: "60|1"}}
	t.Cleanup(func() { SetSendRateOverride(nil) })

	// 未注入：按配置文件。
	if got := w.sendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Fatalf("✘ 未注入时应读配置文件，实得 window=%s max=%d", got.Window, got.Max)
	}

	// 注入后台值：必须压过文件。
	injected := config.WaSendRate{Window: 5 * time.Minute, Max: 3}
	SetSendRateOverride(&injected)
	if got := w.sendRate(); got.Window != 5*time.Minute || got.Max != 3 {
		t.Fatalf("✘ 注入后应读后台值，实得 window=%s max=%d", got.Window, got.Max)
	}

	// 清除注入：回落到配置文件。
	SetSendRateOverride(nil)
	if got := w.sendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Fatalf("✘ 清除后应回落配置文件，实得 window=%s max=%d", got.Window, got.Max)
	}
}

// TestSendRateOverrideIsACopy 注入必须按值拷贝。
//
// 业务侧注入后立刻复用同一个结构体变量（waSendRate 里就是 &r），若这里存指针，
// 调用方后续改 r 会悄悄改掉全局速率 —— 那种 bug 只有跑起来才会以「节奏莫名变了」出现。
func TestSendRateOverrideIsACopy(t *testing.T) {
	t.Cleanup(func() { SetSendRateOverride(nil) })
	w := &waClientWrapper{cfg: &config.WhatsappSessionsConfig{SendRate: "60|1"}}

	src := config.WaSendRate{Window: 2 * time.Minute, Max: 7}
	SetSendRateOverride(&src)
	src.Window = 9 * time.Minute // 调用方复用变量
	src.Max = 99

	if got := w.sendRate(); got.Window != 2*time.Minute || got.Max != 7 {
		t.Fatalf("✘ 注入后被调用方改动污染：window=%s max=%d", got.Window, got.Max)
	}
}
