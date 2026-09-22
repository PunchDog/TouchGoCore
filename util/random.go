package util

import (
	"touchgocore/random"
)

// RandInt 返回 [0,max) 区间内的伪随机 int64；max<=0 时返回 0。
//
// 随机源已从 math/rand/v2 换成本仓 random 包的包级默认实例：全仓只留一条发生器链路，
// 崩溃兜底与并发保护都在那边统一处理。代价是每次取值要过 defaultRandom 的一把互斥锁
// （实测 ns/op 见 docs/perf-baseline.md），而 math/rand/v2 的顶层函数走 per-P 无锁缓存。
// 本仓唯一的生产调用方是对话文本随机选取，量级上完全够；真到了热点上，
// 调用方可以自己持有一个 random.New(seed) 实例，避免和全局实例抢同一把锁。
//
// 默认实例按进程启动时刻播种，只保证取值不重复、不保证跨进程复现（与换掉之前一致）。
// 上游 `random.NextInt64` 的幅度在 [0,1<<63) 上均匀（两路取值异或，非求平均），
// 因此取模只剩「值域宽度不是 max 的整数倍」这一点残余偏斜，相对量级 ≤ 2max/2^63：
// max 到 2^20 时仍小于 2e-13，只有当 max 逼近 2^40 以上才值得改用 rejection 采样。
func RandInt(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return random.NextInt64() % max
}

// RandRange 返回 [min,max) 区间内的伪随机 int64；参数传反时自动交换，相等时返回 min。
//
// 仓内无生产调用方（只有 p0_regress_test.go 覆盖），但它与 RandInt 是一对配套 API，
// 语义清晰、有测试钉住，因此按公开能力保留，不标 Deprecated。
//
// 顺带收掉一个宕机面：区间宽度大到 int64 溢出时（如 RandRange(math.MaxInt64, -1)），
// 旧实现把负数宽度交给 randv2.Int64N 会 panic，现在按 RandInt 的约定退化成返回 min。
func RandRange(max int64, min int64) (ret int64) {
	if min > max {
		min, max = max, min
	}
	if max == min {
		return min
	}
	return min + RandInt(max-min)
}
