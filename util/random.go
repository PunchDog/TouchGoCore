package util

import (
	randv2 "math/rand/v2"
)

// 随机64位，返回 [0, max)；max<=0 时返回 0
// 使用 math/rand/v2 顶层函数：内置协程安全，避免共享非线程安全实例
func RandInt(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return randv2.Int64N(max)
}

// 随机范围 [min, max)；max<min 时自动交换，相等时返回 min
func RandRange(max int64, min int64) (ret int64) {
	if min > max {
		min, max = max, min
	}
	if max == min {
		return min
	}
	return min + randv2.Int64N(max-min)
}
