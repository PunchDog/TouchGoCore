package localtimer

import (
	"testing"
	"time"

	"touchgocore/util"
)

// ============================================================================
// 基准（S64）：到期派发路径。
//
// 每轮把 n 个已到期定时器重新入链，再跑一次 processWheelTick，测量「认领归属 →
// 投递 → 摘链扣计数」的每节点代价。变更前是「整轮持锁扫一遍 + 每节点无锁 CAS」，
// 变更后是「无锁判定 + 仅命中节点回锁认领」，于是业务 Remove 不再排在整轮扫描后面。
// ============================================================================

func BenchmarkProcessWheelTickDispatch(b *testing.B) {
	// 用裸轮：真实管理器的毫秒轮协程会每 1ms 扫描同一个轮，与基准抢派发权
	m := newRawWheelManager()

	const n = 256
	wheel := m.wheels[TimerTypeMillisecond]
	timers := make([]TimerInterface, 0, n)
	for i := 0; i < n; i++ {
		tm, err := NewTimer[*plainTimer](50, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		timers = append(timers, tm)
	}

	ch := currentTimerChannel()
	b.ResetTimer()
	for it := 0; it < b.N; it++ {
		now := util.CurrentMS()
		for _, tm := range timers {
			parent := tm.GetParent()
			parent.nextTime.Store(now - 10)
			m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		}
		m.processWheelTick(wheel, TimerTypeMillisecond)
		for len(ch) > 0 {
			<-ch
		}
	}
}

// BenchmarkRemoveLatencyUnderScan 测量业务 Remove 的最坏等待：另一个协程持续长扫描
// 同一个轮（1000 个远未到期的定时器，只拉长扫描不派发）。变更前一次扫描整轮持锁，
// Remove 只能排在整轮之后；变更后每节点临界区只覆盖「复核 → 认领 → 投递 → 摘链」。
//
// S67 桶化之后这条变成了「最好情况」：那 1000 个远未到期的节点不在本拍窗口里，扫描
// 协程每拍只过两格空桶，压根不构成对 Remove 的压制。口径保留用于跨阶段对照，
// 桶化后的对口测量是 BenchmarkRemoveLatencyUnderBucketScan。
func BenchmarkRemoveLatencyUnderScan(b *testing.B) {
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeHour]

	const backlog = 1000
	addFar := func() {
		tm, err := NewTimer[*plainTimer](3600*1000, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(util.CurrentMS() + 7200*1000)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
	}
	for i := 0; i < backlog; i++ {
		addFar()
	}

	stop := make(chan struct{})
	scanning := make(chan struct{})
	go func() {
		defer close(scanning)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.processWheelTick(wheel, TimerTypeHour)
		}
	}()

	b.ResetTimer()
	var totalWait time.Duration
	for i := 0; i < b.N; i++ {
		tm, err := NewTimer[*plainTimer](3600*1000, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(util.CurrentMS() + 7200*1000)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})

		start := time.Now()
		tm.Remove()
		totalWait += time.Since(start)
	}
	b.StopTimer()
	close(stop)
	<-scanning

	b.ReportMetric(float64(totalWait.Nanoseconds())/float64(b.N), "remove-wait-ns/op")
}

// BenchmarkRemoveLatencyUnderBucketScan 是 BenchmarkRemoveLatencyUnderScan 在桶化之后的
// 对口版本 —— 前者已经测不到它想测的东西：小时轮里 1000 个「远未到期」的节点在桶化后
// 根本不在本拍窗口内，扫描协程每拍只走两格空桶，"长扫描" 这个前提消失了，测出来的
// 等待自然短得毫无意义。
//
// 这里把负载搬到真正会被扫的地方：毫秒轮 + 1000 个已到期节点，每拍都要复核、认领、
// 投递、摘链这么一整个满桶。业务 Remove 要和它抢同一把 wheelLock，于是这条才是
// 「一次扫描锁住整轮」这个旧风险在桶化后的等价形态。
//
// 口径与旧条目一致（另一协程持续扫描同一轮，主协程只测一次 Remove 的墙钟等待），
// 两条可以横向比：桶化改的是「一拍看多少个节点」，这里改的是「那些节点是否真被看到」。
func BenchmarkRemoveLatencyUnderBucketScan(b *testing.B) {
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeMillisecond]
	ch := currentTimerChannel()

	const backlog = 1000
	timers := make([]TimerInterface, 0, backlog)
	for i := 0; i < backlog; i++ {
		tm, err := NewTimer[*plainTimer](50, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		timers = append(timers, tm)
	}
	refill := func() {
		now := util.CurrentMS()
		for _, tm := range timers {
			parent := tm.GetParent()
			parent.nextTime.Store(now - 10)
			m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
		}
	}
	refill()

	stop, scanned := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(scanned)
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.processWheelTick(wheel, TimerTypeMillisecond)
			for len(ch) > 0 {
				<-ch
			}
			refill() // 满桶要一直满着，否则扫描协程空转，Remove 也就没得等
		}
	}()

	b.ResetTimer()
	var totalWait time.Duration
	for i := 0; i < b.N; i++ {
		tm, err := NewTimer[*plainTimer](50, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(util.CurrentMS() - 10)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})

		start := time.Now()
		tm.Remove()
		totalWait += time.Since(start)
	}
	b.StopTimer()
	close(stop)
	<-scanned

	b.ReportMetric(float64(backlog), "bucket-backlog")
	b.ReportMetric(float64(totalWait.Nanoseconds())/float64(b.N), "remove-wait-ns/op")
}
