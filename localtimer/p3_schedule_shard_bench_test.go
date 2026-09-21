package localtimer

import (
	"context"
	"strconv"
	"testing"
)

// ============================================================================
// 基准（S68）：分片只往派发热路径上加了「按 uid 取片」这一步，量清它的代价，
// 以及为什么派发路径必须用通道数组快照而不是逐节点查全局。
//
// 慢回调被隔离掉的那份收益不在这里量：它是「一个 Tick 卡住时其它定时器还跑不跑」
// 的定性差异，由 TestSlowTickOnlyBlocksOwnShard 两个子用例直接钉住（单片窗口内
// 探针 0~2 次 / 分片窗口内约 40 次）。
// ============================================================================

// BenchmarkScheduleChanForPerNode 复刻 tickWheelSection 的取片方式：
// 快照一次 + 逐节点取模，对逐节点加锁查全局。
func BenchmarkScheduleChanForPerNode(b *testing.B) {
	const nodes = 256
	uids := make([]int64, nodes)
	for i := range uids {
		uids[i] = int64(i + 1)
	}

	for _, shards := range []int{1, 4, 8} {
		setShardsForBench(b, shards)
		b.Run(fmtShards(shards)+"/snapshot", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				channels := currentTimerChannels()
				var sink int
				for _, uid := range uids {
					if shardOf(channels, uid) != nil {
						sink++
					}
				}
				if sink != nodes {
					b.Fatal("路由结果出现 nil 通道")
				}
			}
		})
		b.Run(fmtShards(shards)+"/perNodeLock", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var sink int
				for _, uid := range uids {
					// 逐节点取一次全局快照（每节点一对加锁/解锁）
					if shardOf(currentTimerChannels(), uid) != nil {
						sink++
					}
				}
				if sink != nodes {
					b.Fatal("路由结果出现 nil 通道")
				}
			}
		})
	}
}

// setShardsForBench 把调度通道重建为指定片数（基准只测路由，不复用 Run 生命周期）
func setShardsForBench(b *testing.B, n int) {
	b.Helper()
	TimeStop(context.Background())
	resetTimerChannel()
	if got := SetScheduleShards(n); got != n {
		b.Fatalf("设置分片数失败: got=%d want=%d", got, n)
	}
	ensureTimerChannel()
	b.Cleanup(func() {
		resetTimerChannel()
		SetScheduleShards(DefaultScheduleShards)
	})
}

func fmtShards(n int) string {
	return "shards=" + strconv.Itoa(n)
}
