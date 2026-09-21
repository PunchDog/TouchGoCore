package localtimer

import (
	"sync/atomic"

	"touchgocore/vars"
)

// ==================== 调度通道分片（S68） ====================
//
// 到期派发原本只有一条全局通道、一个 timeTick 消费协程：业务 Tick 在这条协程上
// 同步执行，于是一个慢回调（下游 RPC、磁盘、锁）就把整台机器的定时器一起排住，
// 表现为「所有定时器一起晚点」，而不是「只有这一类晚点」。
//
// 分片按 uid 取模把调度项钉在固定一条通道上，一条通道只有一个消费者，于是：
//   - 片内严格保序：同一 uid 的历次派发永远走同一条通道、同一个协程；
//   - 片间并发：某一片卡在业务 Tick 里，只影响它自己那一片的定时器；
//   - 单个定时器的 Tick 仍然只会在一个协程上跑，不需要业务加锁。
//
// 默认 1 片，即既往行为。放开分片等于把「所有业务 Tick 在同一个协程上串行」这条
// 框架假设改成「同一定时器内串行、跨定时器并发」，属于业务侧的并发语义变更，
// 必须由调用方显式开启。

const (
	// DefaultScheduleShards 默认分片数：单条调度通道 + 单个消费协程，
	// 与 S68 之前的行为逐字节一致（全局 FIFO、业务 Tick 串行）。
	DefaultScheduleShards = 1

	// MaxScheduleShards 分片数上限。每片一个常驻协程，且每片容量为
	// MaxTimerChannelNum/分片数——放开到几百只是把协程数与空通道内存抬上去，
	// 真正的并行度受 CPU 与下游约束。
	MaxScheduleShards = 64
)

var scheduleShardWant atomic.Int32

// effectiveShards 取下一次建通道时分片数量（越界的期望值就地夹到合法区间）。
func effectiveShards() int {
	n := int(scheduleShardWant.Load())
	switch {
	case n <= 0:
		return DefaultScheduleShards
	case n > MaxScheduleShards:
		return MaxScheduleShards
	default:
		return n
	}
}

// scheduleShardCount 返回当前生效的分片数：通道已建好时以通道数组为准，
// 否则返回待生效的期望值。消费协程的个数必须与它一致——多起协程会抢同一片
// （破坏片内保序），少起则有分片永久无人消费。
func scheduleShardCount() int {
	if channels := currentTimerChannels(); len(channels) > 0 {
		return len(channels)
	}
	return effectiveShards()
}

// channelSetReady 报告调度通道是否已建好（建好即不可改分片数）
func channelSetReady() bool {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	return timerChannelReady && len(timerChannels) > 0
}

// SetScheduleShards 设置调度通道分片数（即消费者协程数），返回实际生效值。
//
// 只在调度通道还不存在时有效。用框架宿主（App）时，计时器是 App.Start 里最先起的
// 服务（见根包 services.go 的 timerService），因此必须在 NewApp 之后、app.Start()
// 之前调用；裸用本包时在第一次 Run（或第一次 NewTimerManager）之前调用。
// 通道数组长度就是消费者数量，中途改数会让多出来的分片永久无人消费、
// 少掉的分片被两个协程同抢而失去片内保序，因此已建好通道时原样拒绝并告警。
//
// 限制：只有 Run 会为每一片各起一个消费者。宿主若不走 Run 而自己驱动 TimeTick()，
// 它只消费 0 号片，此时开分片等于让其余各片停摆，必须保持 1 片。
//
// 语义代价：n>1 时业务 Tick 不再保证跑在同一个协程上（跨定时器并发）。
// 只有确认自己的 Tick 可重入（或各定时器之间本就无共享状态）才应放开。
// 定时器自身仍然安全：同一 uid 恒定落同一片，跨片之间互不相同的对象没有共享状态；
// 对象池、代次与归属协议与分片数无关。
func SetScheduleShards(n int) int {
	if n <= 0 {
		n = DefaultScheduleShards
	}
	if n > MaxScheduleShards {
		vars.Warning("调度通道分片数超出上限，已夹到 %d: 请求=%d", MaxScheduleShards, n)
		n = MaxScheduleShards
	}
	if channelSetReady() {
		cur := scheduleShardCount()
		// 用 Error 而不是 Warning：调用方的意图被整段忽略，而后果是「定时器照样
		// 跑、只是没并行」，跑起来看不出区别。忽略返回值的宿主必须被吵醒。
		vars.Error("调度通道已建立，分片数保持 %d 不变: 请求=%d（须在 Run/NewTimerManager 之前设置，"+
			"用框架时是 NewApp 之后、Start 之前）", cur, n)
		return cur
	}
	scheduleShardWant.Store(int32(n))
	return n
}

// ScheduleShards 返回当前生效的调度通道分片数
func ScheduleShards() int {
	return scheduleShardCount()
}
