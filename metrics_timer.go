package touchgocore

import (
	"strconv"
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
	timerShardDepthDesc = prometheus.NewDesc(
		"touchgocore_timer_schedule_shard_depth",
		"各调度分片通道此刻的在队调度项数（S68；某一片贴到容量即为该片消费端卡住）",
		[]string{"shard"}, nil)
	timerShardCapacityDesc = prometheus.NewDesc(
		"touchgocore_timer_schedule_shard_capacity",
		"每条调度分片通道的容量，各片之和即允许的调度积压总量", []string{"shard"}, nil)
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
	asyncTimerWorkerLimitDesc = prometheus.NewDesc(
		"touchgocore_async_timer_worker_limit",
		"异步 Tick worker goroutine 数量上限", nil, nil)
	asyncTimerWorkerCountDesc = prometheus.NewDesc(
		"touchgocore_async_timer_worker_count",
		"当前存活的异步 Tick worker goroutine 数量", nil, nil)
	asyncTimerQueueDepthDesc = prometheus.NewDesc(
		"touchgocore_async_timer_queue_depth",
		"动态 worker pool 中等待执行的异步 Tick 任务数（无界队列）", nil, nil)
	asyncTimerInFlightDesc = prometheus.NewDesc(
		"touchgocore_async_timer_in_flight",
		"已由异步 worker 开始且尚未由分片 consumer 完成收尾的 Tick 数", nil, nil)
	asyncTimerOrphanedDesc = prometheus.NewDesc(
		"touchgocore_async_timer_orphaned_total",
		"因原分片消费者已退出而由 worker 本地作废收尾的异步 Tick 累计（大于 0 说明该片已降级）", nil, nil)
)

func (timerBacklogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- timerQueueDepthDesc
	ch <- timerQueueCapacityDesc
	ch <- timerShardDepthDesc
	ch <- timerShardCapacityDesc
	ch <- timerInWheelDesc
	ch <- timerDroppedDesc
	ch <- timerRescheduleFailedDesc
	ch <- timerPoolInUseDesc
	ch <- asyncTimerWorkerLimitDesc
	ch <- asyncTimerWorkerCountDesc
	ch <- asyncTimerQueueDepthDesc
	ch <- asyncTimerInFlightDesc
	ch <- asyncTimerOrphanedDesc
}

func (timerBacklogCollector) Collect(ch chan<- prometheus.Metric) {
	queue := localtimer.GetQueueStats()

	ch <- prometheus.MustNewConstMetric(
		timerQueueDepthDesc, prometheus.GaugeValue, float64(queue.ScheduleLen), "schedule")
	ch <- prometheus.MustNewConstMetric(
		timerQueueCapacityDesc, prometheus.GaugeValue, float64(queue.ScheduleCap), "schedule")
	// 逐片深度另起指标名（S68）：把 "schedule:i" 塞进 queue 标签的话，
	// 不按计划过滤的 sum(queue_depth) 会把总量与各片量一起再加一遍。
	// 单看总深度也看不出问题——4 片时某一片满而总深度只走到 25%，
	// 按总量配的阈值永远不响，所以每片的深度与容量各出一条序列。
	for i, depth := range queue.ScheduleShardLen {
		shard := strconv.Itoa(i)
		ch <- prometheus.MustNewConstMetric(
			timerShardDepthDesc, prometheus.GaugeValue, float64(depth), shard)
		ch <- prometheus.MustNewConstMetric(
			timerShardCapacityDesc, prometheus.GaugeValue, float64(queue.ScheduleShardCap), shard)
	}
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
	// 这两个 _total 取的是「当前这轮 Run → TimeStop 生命周期」的累计值，因此
	// 重建管理器（Run/TimeStop/Run）会让它们回到 0。Counter 归零是 Prometheus
	// rate() 明确处理的 reset，不影响告警，但跨重启做趋势比对时要知道这里有个断点。
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

	asyncWorkers := localtimer.GetAsyncWorkerStats()
	ch <- prometheus.MustNewConstMetric(asyncTimerWorkerLimitDesc, prometheus.GaugeValue, float64(asyncWorkers.Limit))
	ch <- prometheus.MustNewConstMetric(asyncTimerWorkerCountDesc, prometheus.GaugeValue, float64(asyncWorkers.Workers))
	ch <- prometheus.MustNewConstMetric(asyncTimerQueueDepthDesc, prometheus.GaugeValue, float64(asyncWorkers.Queued))
	ch <- prometheus.MustNewConstMetric(asyncTimerInFlightDesc, prometheus.GaugeValue, float64(asyncWorkers.InFlight))
	ch <- prometheus.MustNewConstMetric(asyncTimerOrphanedDesc, prometheus.CounterValue, float64(asyncWorkers.Orphaned))

	// 每轮在链数按档读：只报总数的话，「毫秒轮爆满而其它轮空转」与「均匀分布」
	// 长得一模一样，而前者才是要处置的那种。
	ch <- prometheus.MustNewConstMetric(timerInWheelDesc, prometheus.GaugeValue, float64(total), "all")
	mgr := localtimer.GetDefaultManager()
	for i := range queue.WheelLen {
		var depth int64
		if mgr != nil {
			depth = mgr.GetWheelTimerCount(localtimer.TimerType(i))
		}
		ch <- prometheus.MustNewConstMetric(timerInWheelDesc, prometheus.GaugeValue,
			float64(depth), localtimer.TimerType(i).String())
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
