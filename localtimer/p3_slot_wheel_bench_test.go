package localtimer

import (
	"fmt"
	"testing"
	"time"

	"touchgocore/util"
)

// ============================================================================
// 基准 harness（S67 前置）：先量清「每 tick 扫完整轮」的代价，再谈分桶。
//
// 现实现每档轮是一条按入链顺序排列的链表，processWheelTick 每 tick 从头 Range
// 到尾：是否到期、是否要下沉到更精确的轮，都只能逐个读 nextTime 判断。于是
// 单次 tick 的开销是 O(轮内在链数)，与「这一瞬间真正到期的个数」无关。
// 后果是自我放大的：亚秒定时器越多，每个 tick 越慢，每个 tick 能消化的到期数
// 越少，到期项越积越多。分桶（slot = nextTime/wheelConfig % slots）要把它降成
// O(到期桶内的节点数)，前提是先把这条曲线量出来 —— 否则改完无法证明收益。
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
// spread=true 时把节点摊到 23 个不同的小时桶上（真实负载的形状）；false 时全压在
// 同一时刻（最坏形状：分桶后它们仍在同一个桶里）。两种形状都要量：只看摊开会高估
// 收益，只看压叠会低估收益。函数末尾的自检 tick 用来钉住口径——错了一个 tick 就炸。
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
	if n := len(currentTimerChannel()); n != 0 {
		b.Fatalf("✘ 自检 tick 产生了 %d 条派发，口径已失真", n)
	}
	return timers
}

// fillFarFuture 往指定轮灌 n 个「统一在 horizon 毫秒后到期」的定时器，
// 供「既有扫描存量、又有到期增量」的混合场景使用；调用方随后会按自己的
// 分布重排 nextTime，这里只负责把节点合法地挂进本轮。
func fillFarFuture(b *testing.B, m *TimerManager, wi TimerType, n int, horizon int64) []TimerInterface {
	b.Helper()
	wheel := m.wheels[wi]
	timers := make([]TimerInterface, 0, n)
	for i := 0; i < n; i++ {
		tm, err := NewTimer[*plainTimer](horizon, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(util.CurrentMS() + horizon)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		timers = append(timers, tm)
	}
	if got := wheel.timerCount.Load(); got != int64(n) {
		b.Fatalf("✘ 灌入不完整: count=%d want=%d len=%d", got, n, wheel.tickWheel.Length())
	}
	return timers
}

// BenchmarkWheelScanScaling 量「纯扫描、零到期」下单次 tick 随在链数的增长曲线。
// 这是 S67 要消掉的那一项：变更后它应当只随桶数走，而与 n 基本无关。
func BenchmarkWheelScanScaling(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000} {
		for _, shape := range []struct {
			name   string
			spread bool
		}{{"same-slot", false}, {"spread", true}} {
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
	timers := fillFarFuture(b, m, TimerTypeMillisecond, n, 999)

	// 把间隔摊成 1~999ms：每个 tick 平均有 n/999 ≈ 10 个到期，其余是纯扫描存量。
	now := util.CurrentMS()
	for i, tm := range timers {
		tm.GetParent().nextTime.Store(now + int64(i%999) + 1)
	}

	ch := currentTimerChannel()
	var tickTotal time.Duration
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
			parent.nextTime.Store(util.CurrentMS() + int64(len(ch)%999) + 1)
			m.handleTimerAdd(wheel, timerTask{timer: task.timer, gen: parent.gen.Load(), mgr: m})
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
	timers := fillFarFuture(b, m, TimerTypeMillisecond, n, 199)

	now := util.CurrentMS()
	nominal := make(map[TimerInterface]int64, n)
	for i, tm := range timers {
		offset := int64(i%199) + 1
		tm.GetParent().nextTime.Store(now + offset)
		nominal[tm] = offset
	}
	ch := currentTimerChannel()

	fired := make(map[TimerInterface]int64, n)
	wallStart := time.Now()
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

	var lateSum, lateN, never int64
	for tm, want := range nominal {
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
}
