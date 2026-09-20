package touchgocore

import (
	"sync"

	"touchgocore/localtimer"
	"touchgocore/metrics"
	"touchgocore/vars"

	"github.com/prometheus/client_golang/prometheus"
)

// ============================================================================
// 时间轮积压采集（S66）。
//
// localtimer 原先只有累计计数（丢过多少、迁过多少次），看不出「此刻正在堆」：
// 调度通道与各级入链通道的深度才是「消费端跟不上」或「某个轮协程卡在业务 Tick
// 里」的实时信号，而深度堆到上限的下一步就是静默丢弃。
//
// 采用拉取式 Collector：抓取瞬间读一次快照。既不在调度热路径上挂钩子，
// 也不需要一条常驻上报协程去和关闭流程抢退出时机。
// ============================================================================

type timerBacklogCollector struct{}

var (
	timerQueueDepthDesc = prometheus.NewDesc(
		"touchgocore_timer_queue_depth",
		"调度/入链通道此刻的在队调度项数", []string{"queue"}, nil)
	timerQueueCapacityDesc = prometheus.NewDesc(
		"touchgocore_timer_queue_capacity",
		"调度/入链通道容量，深度长期贴近即为背压前兆", []string{"queue"}, nil)
	timerInWheelDesc = prometheus.NewDesc(
		"touchgocore_timer_in_wheel",
		"各时间轮中在链的定时器数量", []string{"wheel"}, nil)
	timerDroppedDesc = prometheus.NewDesc(
		"touchgocore_timer_dropped_total",
		"因背压被丢弃的调度项累计", nil, nil)
	timerRescheduleFailedDesc = prometheus.NewDesc(
		"touchgocore_timer_reschedule_failed_total",
		"续期重试仍失败、被强制归还对象池的定时器累计（大于 0 需要告警）", nil, nil)
	timerPoolInUseDesc = prometheus.NewDesc(
		"touchgocore_timer_pool_in_use",
		"当前外借中的定时器对象数（Gets 减 Puts）", nil, nil)
)

func (timerBacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- timerQueueDepthDesc
	ch <- timerQueueCapacityDesc
	ch <- timerInWheelDesc
	ch <- timerDroppedDesc
	ch <- timerRescheduleFailedDesc
	ch <- timerPoolInUseDesc
}

func (timerBacklogCollector) Collect(ch chan<- prometheus.Metric) {
	queue := localtimer.GetQueueStats()

	ch <- prometheus.MustNewConstMetric(
		timerQueueDepthDesc, prometheus.GaugeValue, float64(queue.ScheduleLen), "schedule")
	ch <- prometheus.MustNewConstMetric(
		timerQueueCapacityDesc, prometheus.GaugeValue, float64(queue.ScheduleCap), "schedule")
	for i, depth := range queue.WheelLen {
		wheel := localtimer.TimerType(i).String()
		ch <- prometheus.MustNewConstMetric(
			timerQueueDepthDesc, prometheus.GaugeValue, float64(depth), "wheel:"+wheel)
		capacity := 0
		if i < len(queue.WheelCap) {
			capacity = queue.WheelCap[i]
		}
		ch <- prometheus.MustNewConstMetric(
			timerQueueCapacityDesc, prometheus.GaugeValue, float64(capacity), "wheel:"+wheel)
	}

	total, stats := localtimer.GetSystemStats()
	ch <- prometheus.MustNewConstMetric(
		timerDroppedDesc, prometheus.CounterValue, float64(stats.TimersDropped))
	ch <- prometheus.MustNewConstMetric(
		timerRescheduleFailedDesc, prometheus.CounterValue, float64(stats.TimersRescheduleFailed))

	// 外借量而非「Puts 减 Gets 当空闲数」：gets 在池空时现造实例也会计一次，
	// 于是 Puts-Gets 是负数，既不是池内待认领数也没有别的含义；Gets-Puts 才是
	// 「有多少实例正被业务拿着」——归还链路卡住时它只增不减。
	pool := localtimer.GetTimerPoolStats()
	ch <- prometheus.MustNewConstMetric(
		timerPoolInUseDesc, prometheus.GaugeValue, float64(pool.Gets-pool.Puts))

	// 每轮在链数按档读：只报总数的话，「毫秒轮爆满而其它轮空转」与「均匀分布」
	// 长得一模一样，而前者才是要处置的那种。
	ch <- prometheus.MustNewConstMetric(timerInWheelDesc, prometheus.GaugeValue, float64(total), "all")
	if mgr := localtimer.GetDefaultManager(); mgr != nil {
		for i := range queue.WheelLen {
			wheel := localtimer.TimerType(i)
			ch <- prometheus.MustNewConstMetric(timerInWheelDesc, prometheus.GaugeValue,
				float64(mgr.GetWheelTimerCount(wheel)), wheel.String())
		}
	}
}

var timerCollectorOnce sync.Once

// registerTimerBacklogCollector 把采集器挂进监控 registry。幂等。
func registerTimerBacklogCollector() {
	timerCollectorOnce.Do(func() {
		if err := metrics.Registry().Register(timerBacklogCollector{}); err != nil {
			vars.Error("时间轮积压采集器注册失败: %v", err)
			return
		}
	})
}
