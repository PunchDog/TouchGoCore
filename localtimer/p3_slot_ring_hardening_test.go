package localtimer

// 阶段13（S67）复核整改回归：桶环在退化路径上的四条底线。
//
//  1. 桶数由精度表推导（改表即改协议）。推导式写错只会让某一档的滞留时长悄悄偏移，
//     功能用例照样绿，所以必须表驱动钉死；管理器建轮时按当时的表快照推导，彼此不串；
//  2. 可见范围以「一圈 + lookahead 一格」为界：窗口内的桶逐格扫，身后的桶要落后到
//     一圈差一格才被预扫覆盖（那是它被重访的最早机会），再往前就不该再被看到；桶数
//     推导一错，这条界线就提前或推后，代价口径整体走样；
//  3. 扫描回调（含业务实现的 GetParent）抛 panic 时游标绝不能永久停在原地 ——
//     那会让这一档轮每个 tick 都在同一个节点上重炸，等价于整档轮停摆；
//  4. 收尾路径 panic 时「轮空 + 计数归零」的口径仍要成立（阶段12 S66 不变式）。

import (
	"context"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// bombTimer 一个「GetParent 直接 panic」的业务实现：嵌入 plainTimer 从而自带完整的
// list.INode 与 TimerInterface 方法集，只把 GetParent 换掉。挂链要手工做 —— 任何
// 走 GetParent 的入口（handleTimerAdd 的 isValid、归桶的 stepOf）都会被 recover 吞掉。
type bombTimer struct {
	plainTimer
}

func (*bombTimer) GetParent() *Timer {
	panic("业务实现的 GetParent 抛异常")
}

// TestSlotCountForMatchesPrecisionTable 桶数 = 本档滞留时长 / 本档精度。
// 这张表同时决定「绕一圈等于滞留多久」，写错一格就是某档定时器被整圈拖延。
func TestSlotCountForMatchesPrecisionTable(t *testing.T) {
	got := make([]int, len(defaultWheelConfigs))
	for i := range defaultWheelConfigs {
		got[i] = slotCountFor(defaultWheelConfigs, i)
	}
	want := []int{1000, 60, 10, 6, 24}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("✘ 第 %d 档桶数不符: got=%d want=%d", i, got[i], want[i])
		}
	}

	cases := []struct {
		name   string
		config []int64
		i      int
		want   int
	}{
		{"末档无上界取24", []int64{1, 1000}, 1, 24},
		{"跨度超过4096退24", []int64{1, 100000}, 0, 24},
		{"精度表倒挂(下一档更小)", []int64{1000, 1}, 0, 24},
		{"本档精度非正", []int64{0, 1000}, 0, 24},
		{"下标越界", []int64{1, 1000}, 5, 24},
		{"恰好4096", []int64{1, 4096}, 0, 4096},
	}
	for _, c := range cases {
		if n := slotCountFor(c.config, c.i); n != c.want {
			t.Fatalf("✘ %s: got=%d want=%d", c.name, n, c.want)
		}
	}
}

// TestSlotRingExactLapBoundary 可见范围的边界恰好是一圈：落后 60 格走整环扫描，
// 落后 59 格走窗口扫描 —— 加上 lookahead 那一格后窗口正好也铺满一圈，于是两种分支
// 都能重扫到身后那一桶；差一格（落后 58）才真正看不见它。
//
// 这条测的是「一圈之内绝不重扫、到一圈才全扫」的形状：两条分支对节点的可见性
// 恰好在此处交错，桶数若被推导错（slotCount 变小）就会提前进入全扫，代价口径整体走样。
// lookahead 让可见范围宽一格（59 而非 60），这是它的固有代价，也被这里钉住。
//
// 「身后的桶」要造得露出来：入链时 slotFor 一律钳到 cursor+1，所以先把节点挂在
// base+1 那一格，再把游标推到它前面（模拟节点入链后墙钟追过它）。
func TestSlotRingExactLapBoundary(t *testing.T) {
	const sec = util.MILLISECONDS_OF_SECOND
	ring := newTestRing(sec)
	lap := ring.slotCount

	scan := func(targetMinusCur int64) int {
		r := newTestRing(sec)
		base := util.CurrentMS() / sec
		r.cursor.Store(base)
		putAt(t, r, (base+1)*sec+sec/2) // 落在 base+1 那一格
		// 游标推到节点之前，且与节点同余：节点所在桶就此落后于游标
		cur := base + 1 + lap
		r.cursor.Store(cur)
		seen := 0
		r.RangeWindow((cur+targetMinusCur)*sec+sec/2, func(list.INode) bool {
			seen++
			return true
		})
		return seen
	}

	if got := scan(lap); got != 1 {
		t.Fatalf("✘ 落后恰好一圈未走整环扫描（身后的桶被跳过）: seen=%d", got)
	}
	if got := scan(lap - 1); got != 1 {
		t.Fatalf("✘ 落后一圈差一格时 lookahead 未覆盖到身后的桶: seen=%d", got)
	}
	if got := scan(lap - 2); got != 0 {
		t.Fatalf("✘ 一圈之外仍重扫了身后的桶: seen=%d", got)
	}
}

// TestSlotRingPanicInCallbackKeepsCursorMoving 回调 panic 必须就地收口：
// 本桶按残留处理（游标不推进，下拍重试），且 panic 不得抛出 RangeWindow。
func TestSlotRingPanicInCallbackKeepsCursorMoving(t *testing.T) {
	const sec = util.MILLISECONDS_OF_SECOND
	r := newTestRing(sec)
	base := util.CurrentMS() / sec
	r.cursor.Store(base)
	putAt(t, r, (base+1)*sec+sec/2)

	func() {
		defer func() {
			if err := recover(); err != nil {
				t.Fatalf("✘ panic 逃出 RangeWindow，游标将永久停在原地: %v", err)
			}
		}()
		r.RangeWindow((base+1)*sec+sec/2, func(list.INode) bool { panic("回调里炸") })
	}()

	if got := r.cursor.Load(); got != base {
		t.Fatalf("✘ panic 后游标越过了出事的桶: cursor=%d want=%d", got, base)
	}
	seen := 0
	r.RangeWindow((base+1)*sec+sec/2, func(list.INode) bool {
		seen++
		return true
	})
	if seen != 1 {
		t.Fatalf("✘ panic 之后该桶未被重扫（节点被推到一整圈之后）: seen=%d", seen)
	}
}

// TestProcessWheelTickPanicDoesNotStallWheel 端到端：轮里有一个毒节点时，
// 其后各格必须照常派发。变更前一次 panic 只报废本轮扫描；桶化若让 panic 卡住游标，
// 危害反而更大（这一档轮每个 tick 都在同一个节点上重炸，整档停摆）。
//
// 用毫秒轮：毒节点被钳在 cursor+1，健康节点放在 50 格之后，靠真实墙钟推进即可，
// 不必像秒档那样先等一整秒才扫到第二格。
func TestProcessWheelTickPanicDoesNotStallWheel(t *testing.T) {
	mgr := newRawWheelManager()
	wheel := mgr.wheels[TimerTypeMillisecond]
	now := util.CurrentMS()

	bomb := &bombTimer{}
	bp := &bomb.plainTimer.Timer
	bp.nextTime.Store(now + 5)
	bp.isActive.Store(true)
	if !wheel.tickWheel.Add(bomb) { // stepOf 里的 panic 被吞掉，按 cursor+1 钳制入链
		t.Fatal("✘ 毒节点灌入失败")
	}
	bp.wheel.Store(wheel)
	wheel.timerCount.Add(1)

	healthy, err := NewTimer[*plainTimer](50, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	hp := healthy.GetParent()
	// 比毒节点晚 50 格：毒节点炸掉自己那一格时，这一格还要能被扫到
	hp.nextTime.Store(now + 50)
	mgr.handleTimerAdd(wheel, timerTask{timer: healthy, gen: hp.gen.Load(), mgr: mgr})
	if got, want := wheel.timerCount.Load(), int64(wheel.tickWheel.Length()); got != want {
		t.Fatalf("✘ 灌入后计数与环内长度不符: count=%d len=%d", got, want)
	}

	deadline := time.Now().Add(3 * time.Second)
	ch := currentTimerChannel()
	for {
		func() {
			defer func() {
				if err := recover(); err != nil {
					t.Fatalf("✘ panic 逃出 processWheelTick，其后各桶本轮都扫不到: %v", err)
				}
			}()
			mgr.processWheelTick(wheel, TimerTypeMillisecond)
		}()
		if len(ch) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("✘ 毒节点让整档轮停摆：其后各格的定时器始终没被派发")
		}
		time.Sleep(time.Millisecond)
	}
	for len(ch) > 0 {
		<-ch
	}
	// 派发掉健康节点之后，计数与环内长度仍要配对（毒节点留在原地不丢计数）
	if got, want := wheel.timerCount.Load(), int64(wheel.tickWheel.Length()); got != want {
		t.Fatalf("✘ 毒节点打乱了计数与环内长度: count=%d len=%d", got, want)
	}
	if got := wheel.timerCount.Load(); got != 1 {
		t.Fatalf("✘ 毒节点被误摘或重复计数: count=%d want=1", got)
	}
}

// TestDrainWheelPanicStillClears 收尾时业务 GetParent 抛 panic：
// 「轮空 + 计数归零」不能因为 panic 逃出 Range 就破掉。
func TestDrainWheelPanicStillClears(t *testing.T) {
	mgr := newRawWheelManager()
	wheel := mgr.wheels[TimerTypeSecond]

	bomb := &bombTimer{}
	bp := &bomb.plainTimer.Timer
	bp.nextTime.Store(util.CurrentMS())
	bp.isActive.Store(true)
	if !wheel.tickWheel.Add(bomb) {
		t.Fatal("✘ 毒节点灌入失败")
	}
	bp.wheel.Store(wheel)
	wheel.timerCount.Add(1)

	func() {
		defer func() {
			_ = recover() // panic 允许逃出，但清理必须已经发生
		}()
		mgr.drainWheelLocked(wheel)
	}()

	if n := wheel.tickWheel.Length(); n != 0 {
		t.Fatalf("✘ 收尾 panic 后环内仍有 %d 个节点", n)
	}
	if c := wheel.timerCount.Load(); c != 0 {
		t.Fatalf("✘ 收尾 panic 后计数未归零: %d", c)
	}
}

// TestNewTimerManagerSnapshotsConfigTable 精度表是包级变量，测试为造档改表是常见写法。
// 这条钉住三件事：
//  1. 建轮确实按当下这张表推导（表被硬编码取代时立刻判红）；
//  2. 同一个管理器内部自洽 —— 每档桶数等于「它自己那份表」的相邻档之比，不允许出现
//     「第 2 档按新表建、第 3 档按旧表推桶数」的撕裂形状；
//  3. 先建好的管理器不受后续改表影响，改表也不会污染之后新建的管理器。
//
// 说明：NewTimerManager 里那份快照拷贝（timer_manager.go 的 wheelConfigs）只挡得住
// 「建轮过程中另一处并发改表」造成的撕裂，那种时序在单测里造不出来，所以本用例对
// 「拷 vs 不拷」两种实现都会绿 —— 它守的是口径，不是那行拷贝本身。
func TestNewTimerManagerSnapshotsConfigTable(t *testing.T) {
	orig := defaultWheelConfigs
	m1 := NewTimerManager()
	// CloseCtx 的返回值是「是否因超时而放弃收尾」，true 才是异常
	defer func() {
		if m1.CloseCtx(context.Background()) {
			t.Error("✘ m1 收尾超时")
		}
	}()

	// 换一张「秒档起精度翻倍」的表：此后新建的管理器按新表推导
	defaultWheelConfigs = []int64{1, 2000, 120000, 1200000, 7200000}
	m2 := NewTimerManager()
	defer func() {
		if m2.CloseCtx(context.Background()) {
			t.Error("✘ m2 收尾超时")
		}
	}()
	// 另存一份内容：原地改表下面要用到「改表前」的那份内容，共享底层数组就串了
	newTable := append([]int64(nil), defaultWheelConfigs...)

	check := func(name string, m *TimerManager, table []int64) {
		t.Helper()
		if len(m.wheels) != len(table) {
			t.Fatalf("✘ %s 档数不符: got=%d want=%d", name, len(m.wheels), len(table))
		}
		for i, w := range m.wheels {
			if w.wheelConfig != table[i] {
				t.Fatalf("✘ %s 第 %d 档精度不符: got=%d want=%d", name, i, w.wheelConfig, table[i])
			}
			// 桶数按它自己那份表推导：表与桶数一旦来自两次读取，这里就对不上
			if want := int64(slotCountFor(table, i)); w.tickWheel.slotCount != want {
				t.Fatalf("✘ %s 第 %d 档桶数与自己的精度表不自洽: got=%d want=%d", name, i, w.tickWheel.slotCount, want)
			}
		}
	}

	check("m1（建轮时的旧表）", m1, orig)
	check("m2（改表后的新表）", m2, newTable)

	// 建好之后原地改表：两个已存在的管理器一分不动
	defaultWheelConfigs[1] = 5000
	check("m1（原地改表后）", m1, orig)
	check("m2（原地改表后）", m2, newTable)
	defaultWheelConfigs = orig
}
