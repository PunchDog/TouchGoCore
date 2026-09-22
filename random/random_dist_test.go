package random

import (
	"fmt"
	"math"
	"math/bits"
	"testing"
)

// 本文件用卡方拟合优度检验回答一个问题：取值到底均不均匀。
// random_crash_test.go 里的桶检只看「有没有单桶统治 / 有没有空桶」，
// 那种 3 倍门限能抓出退化的发生器，抓不出温和的偏斜，也证明不了均匀。

// distDraws 是每组检验的取值次数。10 万次对 10000 桶仍有每桶期望 10 次，
// 卡方近似成立；再加大只是在给测试套件加时间。
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

// chiCrit 给出自由度 df 在显著性水平 α=0.0001 下的临界值（Wilson–Hilferty 近似）。
// 取到 1e-4 而不是教科书默认的 0.05：本文件有 20 多个断言、又要跟着每轮提交反复跑，
// α=0.05 意味着每轮平均假失败一次，其结局是被所有人无视，那就等于没有门。
// 松到这个程度也不牺牲检出力：真出偏斜时（旧 MonteCarlo 那种 2^17 模数）卡方是几万量级，
// 而这里的临界值只有 1e2~1e4。
func chiCrit(df int) float64 {
	const z = 3.8906
	k := float64(df)
	t := 1 - 2.0/(9.0*k) + z*math.Sqrt(2.0/(9.0*k))
	return k * t * t * t
}

// accept 断言 counts 与各桶等概率的假设相容。
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

// magnitudeBucket 把 [0,1<<63) 的值按幅度切成 k 个等宽桶：floor(v*k / 2^63)。
//
// 不能写成 uint64(v)*uint64(k)>>63：v<2^63、k=64 时乘积到 2^69，回绕会把所有值挤进同一个桶
// （这个函数最初的版本就是这么错的，卡方统计量直接爆到 3.1e6）。用 128 位乘法的高 64 位才准。
func magnitudeBucket(v int64, k int) int {
	hi, lo := bits.Mul64(uint64(v), uint64(k))
	return int(hi<<1 | lo>>63)
}

// autocorrLag1 返回序列与其自身错一位的相关系数。
// 均匀性只保证边际分布，抓不出「值在时间上走低跑道」这类退化，那正是它管的。
func autocorrLag1(xs []float64) float64 {
	n := float64(len(xs))
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= n

	var c0, c1 float64
	for i, x := range xs {
		dx := x - mean
		c0 += dx * dx
		if i > 0 {
			c1 += dx * (xs[i-1] - mean)
		}
	}
	return c1 / c0
}

// TestMersenneTwister_Uniform 单独检验 MT19937-64 这一路：
// 幅度按等宽分桶、以及按调用方真实用法的取模分桶。
func TestMersenneTwister_Uniform(t *testing.T) {
	seed := int64(20260922)
	mt := NewMersenneTwister(&seed)

	mag := make([]int, 64)
	for i := 0; i < distDraws; i++ {
		mag[magnitudeBucket(mt.Int63(), 64)]++
	}
	accept(t, "MT Int63 幅度 64 桶", mag)

	for _, n := range []int{2, 3, 8, 62, 1000, 10000} {
		counts := make([]int, n)
		for i := 0; i < distDraws; i++ {
			counts[mt.Int63()%int64(n)]++
		}
		accept(t, fmt.Sprintf("MT Int63%%%d", n), counts)
	}

	xs := make([]float64, distDraws)
	seed2 := int64(7)
	mt2 := NewMersenneTwister(&seed2)
	for i := range xs {
		xs[i] = float64(mt2.Int63()) / float64(int64(1)<<62)
	}
	if r := autocorrLag1(xs); math.Abs(r) > 0.02 {
		t.Errorf("MT lag-1 自相关 = %.4f，超出 ±0.02", r)
	}
}

// TestMonteCarlo_Uniform 单独检验 xoshiro256** 这一路。
// 旧实现在这里是重灾区：LCG 模数只有 2^17~2^24，低位桶会明显偏。
func TestMonteCarlo_Uniform(t *testing.T) {
	seed := int64(20260922)
	mc := NewMonteCarlo(&seed)

	mag := make([]int, 64)
	for i := 0; i < distDraws; i++ {
		mag[magnitudeBucket(mc.nextInt64(), 64)]++
	}
	accept(t, "MC nextInt64 幅度 64 桶", mag)

	for _, n := range []int{2, 3, 8, 62, 1000, 10000} {
		counts := make([]int, n)
		for i := 0; i < distDraws; i++ {
			counts[mc.nextInt64()%int64(n)]++
		}
		accept(t, fmt.Sprintf("MC nextInt64%%%d", n), counts)
	}

	xs := make([]float64, distDraws)
	seed2 := int64(7)
	mc2 := NewMonteCarlo(&seed2)
	for i := range xs {
		xs[i] = float64(mc2.nextInt64()) / float64(int64(1)<<62)
	}
	if r := autocorrLag1(xs); math.Abs(r) > 0.02 {
		t.Errorf("MC lag-1 自相关 = %.4f，超出 ±0.02", r)
	}
}

// TestNextInt64_ResidueUniform 检验组合两路之后、调用方真正用到的残余分布。
//
// 两路异或不打坏它：异或结果的边际分布跟随其中均匀的那一路，
// 残余偏斜只剩「63 位不是 n 的整数倍」这一点，量级 ≤ 2n/2^63。
func TestNextInt64_ResidueUniform(t *testing.T) {
	for _, n := range []int{2, 3, 8, 62, 1000, 10000} {
		counts := make([]int, n)
		for i := 0; i < distDraws; i++ {
			counts[NextInt64()%int64(n)]++
		}
		accept(t, fmt.Sprintf("NextInt64%%%d", n), counts)
	}
}

// TestNextInt64_MagnitudeUniform 是幅度均匀性的回归门。
//
// 这里曾经是把两路取值求平均（(a+b)>>1），两个独立均匀量求平均会得到以 2^62 为峰的
// 三角分布：实测中间桶是边缘桶的 57~97 倍（理论 63 倍），值域两端几乎取不到值。
// 现在两路改成异或，异或结果的边际分布跟随其中均匀的那一路，幅度恢复均匀。
// 三条断言各自拦一种回归：卡方拦整体偏斜，中间/边缘比拦「质量被挤向中段」，
// 最大值断言拦「值域被压窄」——三角分布下 10 万次取值的最大值只到约 0.75×2^63。
func TestNextInt64_MagnitudeUniform(t *testing.T) {
	const k = 64
	counts := make([]int, k)
	var maxSeen int64
	for i := 0; i < distDraws; i++ {
		v := NextInt64()
		counts[magnitudeBucket(v, k)]++
		if v > maxSeen {
			maxSeen = v
		}
	}

	accept(t, "NextInt64 幅度 64 桶", counts)

	edge := counts[0]
	if edge == 0 {
		t.Fatal("最小幅度桶一次没命中：取值不再覆盖整个值域")
	}
	ratio := float64(counts[k/2-1]+counts[k/2]) / 2 / float64(edge)
	t.Logf("中间桶/边缘桶 = %.2f（均匀 ≈1，旧的求平均实现 ≈63；边缘桶命中 %d 次）", ratio, edge)
	if ratio < 0.4 || ratio > 2.5 {
		t.Errorf("中间桶/边缘桶 = %.2f，偏离 1 太多：两路的组合方式又退回求平均了？", ratio)
	}

	// 均匀分布下 10 万次取值的期望最大值 ≈ (1 - 1/100001)×2^63，留三个数量级余量。
	// 这里必须用无类型常量算 1<<63：写成 int64(1)<<63 会在编译期就溢出。
	if bound := float64(1<<63) * 0.99; float64(maxSeen) < bound {
		t.Errorf("最大观测值 = %.4g，低于 0.99×2^63 = %.4g：值域被压窄", float64(maxSeen), bound)
	}

	var left, right int
	for j := 0; j < k/2; j++ {
		left += counts[j]
		right += counts[k-1-j]
	}
	accept(t, "左右半区对称", []int{left, right})
}
