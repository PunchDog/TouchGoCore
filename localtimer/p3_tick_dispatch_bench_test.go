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
