package localtimer

import (
	"sync/atomic"

	"touchgocore/util"
	"touchgocore/vars"
)

// TimerStats 保存性能统计信息（快照值，非实时引用）
type TimerStats struct {
	TimersAdded     int64 // 已添加定时器数量
	TimersRemoved   int64 // 脱离调度的次数：暂停/移除/耗尽/关闭清场/背压丢弃（含复活前的摘链，不含派发与迁移）
	TimersExecuted  int64 // 已执行定时器数量
	TimersDropped   int64 // 因背压被丢弃的调度次数
	WheelMigrations int64 // 时间轮迁移次数

	TimersRescheduleFailed int64 // 续期重试后仍失败、定时器已被强制归还对象池的次数（>0 说明需要告警）
}

// timerStatsCounters 管理器内部的原子计数器
type timerStatsCounters struct {
	timersAdded     atomic.Int64
	timersRemoved   atomic.Int64
	timersExecuted  atomic.Int64
	timersDropped   atomic.Int64
	wheelMigrations atomic.Int64

	timersRescheduleFailed atomic.Int64
}

// wheelShouldWarn 限频判定：每个时间轮每秒最多一条告警。
//
// 各轮独立计数，否则毫秒轮常年背压会把小时轮的告警一并吞掉，
// 而后者恰恰是「某个低频业务定时器正在丢调度」的唯一线索。
func wheelShouldWarn(wheel *TimerWheel) bool {
	now := util.CurrentMS()
	last := wheel.lastBackpressureWarn.Load()
	if now-last < 1000 {
		return false
	}
	return wheel.lastBackpressureWarn.CompareAndSwap(last, now)
}

// notifyBackpressure 背压丢弃告警（确有调度项被丢下）。
//
// 必须在释放 wheelLock 与 parent.mu 之后调用——vars 的异步日志在缓冲写满时会
// 阻塞最长 5 秒（见 vars/async_channel.go 的阻塞模式分支），持锁调用会把整个
// 时间轮或这把定时器私有锁拖死。
func (m *TimerManager) notifyBackpressure(wheel *TimerWheel, wheelType TimerType, dropped int64) {
	if wheel == nil || !wheelShouldWarn(wheel) {
		return
	}
	vars.Warning("定时器背压: 轮=%s, 本次丢弃=%d, 累计丢弃=%d",
		wheelType.String(), dropped, m.stats.timersDropped.Load())
}

// notifyHighWatermark 入队通道水位告警：本次入队成功，只是队列已接近上限。
// 与丢弃告警分开措辞，否则「本次丢弃=0」的背压日志会让人误判为统计出错。
func (m *TimerManager) notifyHighWatermark(wheel *TimerWheel, wheelType TimerType, used, capacity int64) {
	if wheel == nil || !wheelShouldWarn(wheel) {
		return
	}
	vars.Warning("定时器入队通道水位过高: 轮=%s, 已用=%d/%d, 累计丢弃=%d",
		wheelType.String(), used, capacity, m.stats.timersDropped.Load())
}

// GetStats 返回性能统计信息
func (m *TimerManager) GetStats() TimerStats {
	return TimerStats{
		TimersAdded:     m.stats.timersAdded.Load(),
		TimersRemoved:   m.stats.timersRemoved.Load(),
		TimersExecuted:  m.stats.timersExecuted.Load(),
		TimersDropped:   m.stats.timersDropped.Load(),
		WheelMigrations: m.stats.wheelMigrations.Load(),

		TimersRescheduleFailed: m.stats.timersRescheduleFailed.Load(),
	}
}

// GetTimerCount 返回所有时间轮中的定时器总数
func (m *TimerManager) GetTimerCount() int64 {
	var total int64
	for _, wheel := range m.wheels {
		total += wheel.timerCount.Load()
	}
	return total
}

// GetWheelTimerCount 返回某一档时间轮在链的定时器数量。越界档位返回 0。
//
// 与总数分开暴露，是为了让「毫秒轮爆满而其它轮空转」和「五档均匀分布」在监控上
// 长得不一样——只有前者需要处置。
func (m *TimerManager) GetWheelTimerCount(wheelType TimerType) int64 {
	if int(wheelType) < 0 || int(wheelType) >= len(m.wheels) {
		return 0
	}
	if wheel := m.wheels[wheelType]; wheel != nil {
		return wheel.timerCount.Load()
	}
	return 0
}

// TimerQueueStats 调度链路的即时积压：两级通道的在队任务数与容量。
//
// TimerStats 只有累计计数，说得清「一共丢过多少」，看不出「此刻正在堆」。
// 而通道深度才是「消费端跟不上 / 某个轮协程卡在业务 Tick 里」的实时信号。
type TimerQueueStats struct {
	// ScheduleLen 全部分片调度通道的在队任务数之和（S68）。分片只是把一条队列
	// 切成 N 条，总量口径不变，便于与分片化之前的看板/告警继续对齐。
	ScheduleLen int
	ScheduleCap int // 各分片容量之和；持续贴近说明消费端跟不上
	// ScheduleShardLen 每片各自的在队数，下标即分片号（S68）。
	// 只有总和不够用：某一片被慢回调占死时它已经在丢弃调度项，
	// 而总和只涨到 1/N，按总量配的阈值永远不会响。
	ScheduleShardLen []int
	ScheduleShardCap int // 单片容量（各片相同）
	WheelLen         []int
	WheelCap         []int
}

// GetQueueStats 读取此刻的通道积压。管理器已 Close（通道被释放）或尚未 Run 时
// 各项为 0；WheelLen/WheelCap 恒与 m.wheels 等长。
func (m *TimerManager) GetQueueStats() TimerQueueStats {
	s := TimerQueueStats{
		WheelLen: make([]int, len(m.wheels)),
		WheelCap: make([]int, len(m.wheels)),
	}
	channels := currentTimerChannels()
	s.ScheduleShardLen = make([]int, len(channels))
	s.ScheduleShardCap = int(shardCapFor(len(channels)))
	for i, shard := range channels {
		s.ScheduleShardLen[i] = len(shard)
		s.ScheduleLen += len(shard)
		s.ScheduleCap += cap(shard)
	}
	for i, wheel := range m.wheels {
		if wheel == nil {
			continue
		}
		s.WheelLen[i] = len(wheel.addTimerChan)
		s.WheelCap[i] = cap(wheel.addTimerChan)
	}
	return s
}
