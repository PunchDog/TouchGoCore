package util

import (
	"fmt"
	"math"
	randv2 "math/rand/v2"
	"testing"
	"touchgocore/random"
)

// 本文件回答两件事：RandInt/RandRange 换成 random 包之后分布还均不均匀，
// 以及换来的锁开销是多少。基准与断言放在一起，是为了让两者在同一次运行里可对照。

// distDraws 是每组检验的取值次数。10 万次对 1000 桶仍是每桶期望 100 次，
// 卡方近似成立；桶数取到 10000 时才会掉到期望 10 次以下，这里不往上加。
const distDraws = 100000

// chiSquare 返回观测频次对「各桶等概率」这一原假设的卡方统计量。
func chiSquare(counts []int) float64 {
	var total float64
	for _, c := range counts {
		total += float64(c)
	}
	expected := total / float64(len(counts))

	var x2 float64
	for _, c := range counts {
		d := float64(c) - expected
		x2 += d * d / expected
	}
	return x2
}

// chiCrit 给出自由度 df 在 α=1e-4 下的临界值（Wilson–Hilferty 近似）。
// 显著性取得比教科书松，是因为断言多、又要跟着每轮提交反复跑；
// 真偏斜时统计量是几万量级，不会因此漏判。
func chiCrit(df int) float64 {
	const z = 3.8906
	k := float64(df)
	t := 1 - 2.0/(9.0*k) + z*math.Sqrt(2.0/(9.0*k))
	return k * t * t * t
}

func accept(t *testing.T, name string, counts []int) {
	t.Helper()

	x2 := chiSquare(counts)
	df := len(counts) - 1
	if crit := chiCrit(df); x2 > crit {
		t.Errorf("%s：卡方 %.1f 超过临界值 %.1f（df=%d, α=1e-4），分布不均匀", name, x2, crit, df)
	} else {
		t.Logf("%s：卡方 %.1f ≤ %.1f（df=%d）通过", name, x2, crit, df)
	}
}

// TestRandInt_ResidueUniform 按调用方的真实用法检验残余分布：
// 对小 n 取模选桶（n=3 与 n=62 分别对应对话文本条数量级与 RandomStr 的字符集大小）。
func TestRandInt_ResidueUniform(t *testing.T) {
	for _, n := range []int{2, 3, 8, 62, 1000} {
		counts := make([]int, n)
		for i := 0; i < distDraws; i++ {
			counts[int(RandInt(int64(n)))]++
		}
		accept(t, fmt.Sprintf("RandInt(%d)", n), counts)
	}
}

// TestRandRange_ResidueUniform 检验带偏移区间的分布：命中应铺满 [3,103) 的每个整数。
func TestRandRange_ResidueUniform(t *testing.T) {
	const (
		lo = int64(3)
		hi = int64(103)
	)
	counts := make([]int, hi-lo)
	for i := 0; i < distDraws; i++ {
		v := RandRange(hi, lo)
		if v < lo || v >= hi {
			t.Fatalf("RandRange(%d,%d) = %d 越界", hi, lo, v)
		}
		counts[v-lo]++
	}
	accept(t, "RandRange(103,3)", counts)
}

// TestRandRange_HugeWidth 钉住换源顺带收掉的宕机面：
// 宽度大到 int64 溢出成负数时，旧实现把它交给 randv2.Int64N 会 panic。
func TestRandRange_HugeWidth(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RandRange 大区间 panic: %v", r)
		}
	}()

	if got := RandRange(math.MaxInt64, -1); got != -1 {
		t.Errorf("宽度溢出时 RandRange(MaxInt64,-1) = %d，期望按 RandInt 的兜底返回 min=-1", got)
	}
	_ = RandRange(math.MaxInt64, math.MinInt64)
	_ = RandInt(math.MaxInt64)
}

// BenchmarkRandInt 是换源之后的实际开销：每次取值一把全局互斥锁 + 两路引擎。
func BenchmarkRandInt(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = RandInt(1000)
	}
}

// BenchmarkUnderlyingNextInt64 把 RandInt 底下那次取值单独量出来，
// 用同一个时间窗把「取模 + 边界判断」这一层的开销从锁与引擎里剥出去。
func BenchmarkUnderlyingNextInt64(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = random.NextInt64()
	}
}

// BenchmarkRandIntStdlibRef 保留被换掉的 math/rand/v2 路径作为同窗参照：
// 它走 per-P 无锁缓存且用 rejection-free 归约，与生产代码无关，只用于量化换源代价。
func BenchmarkRandIntStdlibRef(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = randv2.Int64N(1000)
	}
}
