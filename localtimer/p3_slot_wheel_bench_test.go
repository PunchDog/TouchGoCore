package localtimer

import (
	"fmt"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// ============================================================================
// 基准 harness（S67 前后对照）：量清变更前「每 tick 扫完整轮」的代价，以及桶化后
// 同一套负载的实际形状。
//
// 变更前每档轮是一条按入链顺序排列的链表，processWheelTick 每 tick 从头 Range
// 到尾：是否到期、是否要下沉到更精确的轮，都只能逐个读 nextTime 判断。于是
// 单次 tick 的开销是 O(轮内在链数)，与「这一瞬间真正到期的个数」无关。
// 后果是自我放大的：亚秒定时器越多，每个 tick 越慢，每个 tick 能消化的到期数
// 越少，到期项越积越多。
//
// 变更后一次 tick 只付「游标走过的那一格 + lookahead 预扫一格」的钱，于是同一条
// 曲线的含义变了，读数字前必须先看形状落在哪个桶里：
//   - same-slot（变更后改名 idle-single-bucket）：n 个节点全压在同一格，而那一格
//     不在本拍窗口内 —— 变更后每拍一次都不看它，ns-per-timer 归零。这不是「扫描
//     变快了」，是「根本没扫」；真正的 worst case 见 BenchmarkScannedBucketCost；
//   - spread（idle-spread-23h）：n 个节点摊到未来 23 个不同小时。小时轮只有 24 格，
//     跨圈混叠使约 2/23 的节点恰好落进被访的两格，于是这一条量到的才是桶化后常态
//     存量每拍要付的钱。它比上一条难看，只报上一条就是自欺。
//
// 全部用 newRawWheelManager（无轮协程），tick 由基准循环自己驱动：真实管理器的
// 毫秒轮协程每 1ms 扫描同一个轮，会与测量抢派发权，把数字变成噪声。
// ============================================================================

// fillScanLoad 往 wi 轮灌 n 个「整轮滞留、且远未到期」的定时器，用于量纯扫描。
//
// 口径要求节点在整个基准期间既不到期也不下沉：
//   - 会下沉：remaining 小于本档下界时 tick 把它搬去更精确的轮（首版把「远未到期」
//     压到小时尺度却挂在毫秒轮上，第一个 tick 就搬空整轮，之后测的是空表遍历，
//     1k/10k 两档给出 0/8µs 的假曲线）；
//   - 会到期：单档扫描可达数百微秒，基准要跑上十亿纳秒级，压在毫秒轮里的 500ms
//     余量中途就被消化掉，曲线会随链表缩短而递减。
//
// 因此扫描基准用小时轮、偏移自 2 小时起（remaining 始终 >= 1 小时，落在本档区间内）。
//
// spread=true 时把节点摊到 23 个不同的小时桶上（真实负载的形状，跨圈后必有节点混进
// 被访的两格）；false 时全压在同一时刻（同一格，变更后整拍不被访问）。两种形状都要量：
// 只看摊开会高估收益，只看压叠会低估收益。函数末尾的自检 tick 用来钉住口径——
// 错了一个 tick 就炸。
func fillScanLoad(b *testing.B, m *TimerManager, wi TimerType, n int, spread bool) []TimerInterface {
	b.Helper()
	wheel := m.wheels[wi]
	const hourMS = int64(util.MILLISECONDS_OF_HOUR)
	timers := make([]TimerInterface, 0, n)
	now := util.CurrentMS()
	for i := 0; i < n; i++ {
		tm, err := NewTimer[*plainTimer](2*hourMS, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		offset := 2 * hourMS
		if spread {
			offset = (2 + int64(i%23)) * hourMS
		}
		parent := tm.GetParent()
		parent.nextTime.Store(now + offset)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		timers = append(timers, tm)
	}
	if got := wheel.timerCount.Load(); got != int64(n) {
		b.Fatalf("✘ 灌入不完整: count=%d want=%d len=%d", got, n, wheel.tickWheel.Length())
	}

	ch := currentTimerChannel()
	for len(ch) > 0 {
		<-ch
	}
	m.processWheelTick(wheel, wi)
	if got := wheel.timerCount.Load(); got != int64(n) {
		b.Fatalf("✘ 自检 tick 后节点被搬走，测的不是纯扫描: count=%d want=%d", got, n)
	}
	if pending := len(currentTimerChannel()); pending != 0 {
		b.Fatalf("✘ 自检 tick 产生了 %d 条派发，口径已失真", pending)
	}
	return timers
}

// fillSpread 往 wi 轮灌 n 个定时器，第 i 个的时刻是 base+offset(i)；返回 base，
// 便于调用方按同一基准登记标称到期时间（灌 1 万个本身要几十毫秒，另取时刻会把
// 「晚到多少」读成负数）。
//
// 供「既有扫描存量、又有到期增量」的混合场景使用。S67 起必须「先定时刻再入链」：
// 归桶按入链瞬间的 nextTime 计算，先挂再改期会把节点留在一个与它无关的桶里，
// 测出来的就不再是真实负载的形状。
func fillSpread(b *testing.B, m *TimerManager, wi TimerType, n int,
	offset func(i int) int64) (timers []TimerInterface, base int64) {
	b.Helper()
	wheel := m.wheels[wi]
	timers = make([]TimerInterface, 0, n)
	base = util.CurrentMS()
	for i := 0; i < n; i++ {
		horizon := offset(i)
		tm, err := NewTimer[*plainTimer](horizon, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(base + horizon)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		timers = append(timers, tm)
	}
	if got := wheel.timerCount.Load(); got != int64(n) {
		b.Fatalf("✘ 灌入不完整: count=%d want=%d len=%d", got, n, wheel.tickWheel.Length())
	}
	return timers, base
}

// BenchmarkWheelScanScaling 量「纯扫描、零到期」下单次 tick 随在链数的增长曲线。
// 变更前这条曲线严格线性（O(在链数)/拍）；变更后同一条曲线应当与 n 脱钩 —— 但注意
// 脱钩的原因是「那些桶根本没被扫」，被扫的桶内的钱由 BenchmarkScannedBucketCost 量。
func BenchmarkWheelScanScaling(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		for _, shape := range []struct {
			name   string
			spread bool
		}{{"idle-single-bucket", false}, {"idle-spread-23h", true}} {
			b.Run(fmt.Sprintf("%d-timers/%s", n, shape.name), func(b *testing.B) {
				m := newRawWheelManager()
				wheel := m.wheels[TimerTypeHour]
				fillScanLoad(b, m, TimerTypeHour, n, shape.spread)

				var scanTotal time.Duration
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					start := time.Now()
					m.processWheelTick(wheel, TimerTypeHour)
					scanTotal += time.Since(start)
				}
				b.StopTimer()
				b.ReportMetric(float64(n), "timers")
				b.ReportMetric(float64(scanTotal.Nanoseconds())/float64(b.N), "ns-per-tick")
				b.ReportMetric(float64(scanTotal.Nanoseconds())/float64(b.N)/float64(n), "ns-per-timer")
			})
		}
	}
}

// BenchmarkWheelTickWithSubSecondLoad 复现计划里的「1 万亚秒定时器场景」：
// 1 万个 1~999ms 间隔的定时器压在毫秒轮上，每个 tick 既有要扫的存量、也有真正
// 到期的增量。measure 的是「一个 tick 的实际耗时」，分桶后要按同一口径复测。
func BenchmarkWheelTickWithSubSecondLoad(b *testing.B) {
	const n = 10000
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeMillisecond]
	fillSpread(b, m, TimerTypeMillisecond, n, func(i int) int64 {
		return int64(i%999) + 1 // 摊成 1~999ms：每个 tick 平均 n/999 ≈ 10 个到期
	})

	ch := currentTimerChannel()
	var tickTotal time.Duration
	// 回链间隔用轮转计数，不用 len(ch)%999：后者在一次排空里单调递减，等于把整批
	// 到期项重新压回相邻的少数几格，桶越攒越满，ns-per-tick 会抖成 5~120µs 的噪声，
	// 什么也 A/B 不出来。轮转才让「每格约 n/999 个」这个前提全程成立。
	var rotate int64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		m.processWheelTick(wheel, TimerTypeMillisecond)
		tickTotal += time.Since(start)
		// 消费端把到期项取走并回链，模拟 executeTimer 的续期入链，保持存量恒定
		for len(ch) > 0 {
			task := <-ch
			parent := task.timer.GetParent()
			parent.nextTime.Store(util.CurrentMS() + rotate%999 + 1)
			rotate++
			m.handleTimerAdd(wheel, timerTask{timer: task.timer, gen: parent.gen.Load(), mgr: m})
		}
		// 按 1ms 节拍驱动：不分拍等待的话墙钟几乎不走，「每拍有约 10 个到期」这一
		// 前提就不成立，量到的只是空转
		if d := time.Since(start); d < time.Millisecond {
			time.Sleep(time.Millisecond - d)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(n), "timers")
	b.ReportMetric(float64(tickTotal.Nanoseconds())/float64(b.N), "ns-per-tick")
}

// BenchmarkSubSecondDeadlineMiss 量「亚秒定时器实际比标称晚多少毫秒被派发」——
// O(N)/tick 的扫描真正伤到的是这个：单拍扫描一旦超过 1ms，毫秒轮的消费端就永远
// 追不上名义节拍，晚到的不只是这一拍，而是此后每一拍都带着前面的欠账。
//
// 口径用「派发时刻的真实经过毫秒数 − 标称到期毫秒数」，而不是「第几个 tick」：
// 每 tick 的墙钟耗时本身就在漂，按 tick 序号比会把慢扫描的惩罚读成 0。
// 1 万个定时器摊在 1~199ms 内，窗口 300 拍；跑不完的记为 never-fired。
//
// 注意用 TimerInterface 而非 UID 作键：这里绕开 AddTimer 直接 handleTimerAdd 入链，
// uid 只有 AddTimer 才会补发，全部为 0 会把一万个样本塌缩成一条。
func BenchmarkSubSecondDeadlineMiss(b *testing.B) {
	const n = 10000
	const ticks = 300
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeMillisecond]
	nominal := make(map[TimerInterface]int64, n)
	timers, _ := fillSpread(b, m, TimerTypeMillisecond, n, func(i int) int64 {
		return int64(i%199) + 1
	})
	// 标称到期毫秒数按「计时起点」折算：灌 1 万个定时器本身要花几十毫秒，
	// 用灌入前的时刻作基准会把晚到量整体读小。起点之后就已到期的那些，
	// 标称值为负、真实派发在 0 附近，计入的是它们确实被推迟了的那部分。
	ref := util.CurrentMS()
	wallStart := time.Now() // 与 ref 同一瞬间取，否则登记口径整体偏移
	for _, tm := range timers {
		nominal[tm] = tm.GetParent().nextTime.Load() - ref
	}
	ch := currentTimerChannel()

	fired := make(map[TimerInterface]int64, n)
	b.ReportAllocs()
	b.ResetTimer()
	for t := 0; t < ticks; t++ {
		start := time.Now()
		m.processWheelTick(wheel, TimerTypeMillisecond)
		for len(ch) > 0 {
			task := <-ch
			if _, ok := fired[task.timer]; !ok {
				fired[task.timer] = time.Since(wallStart).Milliseconds()
			}
			parent := task.timer.GetParent()
			parent.nextTime.Store(util.CurrentMS() + 10*1000) // 回链为秒级，不再参与本窗口
			m.handleTimerAdd(wheel, timerTask{timer: task.timer, gen: parent.gen.Load(), mgr: m})
		}
		if d := time.Since(start); d < time.Millisecond {
			time.Sleep(time.Millisecond - d)
		}
	}
	b.StopTimer()

	var lateSum, lateN, never, skipped int64
	for tm, want := range nominal {
		// 起点之前就已到期的不入样：它们的「晚到」全部来自灌链本身的耗时（1 万个要
		// 几十毫秒），与轮的实现无关，计入只会把 avg-late 变成灌入速度的度量。
		if want < 1 {
			skipped++
			continue
		}
		got, ok := fired[tm]
		if !ok {
			never++
			continue
		}
		if d := got - want; d > 0 {
			lateSum += d
		}
		lateN++
	}
	b.ReportMetric(float64(n), "timers")
	b.ReportMetric(float64(lateSum)/float64(lateN), "avg-late-ms")
	b.ReportMetric(float64(never), "never-fired")
	b.ReportMetric(float64(skipped), "skipped-already-due")
}

// BenchmarkTickVisitCount 量「桶化之后一次 tick 到底要看多少个节点」——这是变更后
// 真正的代价曲线，也是 BenchmarkWheelScanScaling 那两条读法（一条根本没扫、一条只扫
// 跨圈混进来的那部分）之外的第三种形状：负载就压在会被扫的那两格里。
//
// 口径用固定墙钟反复驱动 RangeWindow，回调只计数不派发：
//   - 固定 target 让游标第二次起走 target<=cursor 分支，每拍稳定覆盖「本格 + 预扫格」
//     两格，于是量到的就是每拍的可见集，不会像真实时钟那样跑着跑着把整点越过、
//     节点因到期或被下沉而离场（那测的就不再是扫描）；
//   - all-in-visited-slot：n 个节点全压在被访那一格（同一秒/同一分钟内一起到期的突发），
//     这一拍必须逐个过 n 个 —— 桶化不能救它，也不该救；
//   - spread-over-ring：n 个节点摊满整环，每拍只看到约 n·2/slotCount 个。
//
// 两条的 visited-per-tick 之比就是桶化的实际收益倍数，比 ns/timer 那种平均值诚实。
func BenchmarkTickVisitCount(b *testing.B) {
	const hour = util.MILLISECONDS_OF_HOUR
	for _, n := range []int{1000, 10000, 50000} {
		for _, shape := range []struct {
			name      string
			inVisited bool
		}{{"all-in-visited-slot", true}, {"spread-over-ring", false}} {
			b.Run(fmt.Sprintf("%d-timers/%s", n, shape.name), func(b *testing.B) {
				r := newTestRing(hour) // 小时轮：24 格
				base := util.CurrentMS() / hour
				r.cursor.Store(base)

				for i := 0; i < n; i++ {
					tm, err := NewTimer[*plainTimer](hour, InfiniteCount, nil)
					if err != nil {
						b.Fatal(err)
					}
					parent := tm.GetParent()
					// 步数 base+1 正是每拍被预扫的那一格；其余格只在摊开形状里被填
					step := int64(base + 1)
					if !shape.inVisited {
						step = base + 2 + int64(i)%int64(r.slotCount-1)
					}
					parent.nextTime.Store(step*hour + hour/2)
					if !r.Add(tm) {
						b.Fatal("Add 失败")
					}
				}
				if got := r.Length(); got != n {
					b.Fatalf("✘ 灌入不完整: len=%d want=%d", got, n)
				}

				target := base*hour + hour/2
				var visited int64
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r.RangeWindow(target, func(list.INode) bool {
						visited++
						return true
					})
				}
				b.StopTimer()

				b.ReportMetric(float64(n), "timers")
				b.ReportMetric(float64(visited)/float64(b.N), "visited-per-tick")
				b.ReportMetric(float64(visited)/float64(b.N)/float64(n)*100, "visited-%")
			})
		}
	}
}
