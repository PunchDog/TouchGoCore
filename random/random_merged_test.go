package random

import (
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"sync"
	"testing"
)

// mustNotPanic 把「今天必崩」的调用面钉成断言。
// 只在 recover 里格式化取值，不做类型断言：旧实现正是在这里写 err.(error)，
// 遇到非 error 的 panic 值会二次 panic 且无人 recover。
func mustNotPanic(t *testing.T, name string, f func()) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s 发生 panic: %v", name, r)
		}
	}()
	f()
}

// TestMersenneTwister_StreamPinned 逐位钉住 MT19937-64 的输出流。
// 期望值取自改动前的提交（同一种子下 Int63 前 8 个值，与 Uint64 前 700 个值之和），
// TestMersenneTwister_Determinism 只能证明「同种子自洽」，证不了流没变。
func TestMersenneTwister_StreamPinned(t *testing.T) {
	wantInt63 := []int64{
		24439008469247521, 2137609256016817061, 2941160478758282955, 457094689621632894,
		5774262341096151890, 5339118007413155779, 6618504531372420612, 1677435431905907170,
	}
	seed := int64(123)
	mt := NewMersenneTwister(&seed)
	for i, want := range wantInt63 {
		if got := mt.Int63(); got != want {
			t.Fatalf("第 %d 个 Int63 = %d，基线为 %d（twist/升温变换被改动了）", i+1, got, want)
		}
	}

	// 700 > 2*N，覆盖两轮以上的 twist
	const words = 700
	seed42 := int64(42)
	mt42 := NewMersenneTwister(&seed42)
	var sum uint64
	for i := 0; i < words; i++ {
		sum += mt42.Uint64()
	}
	if sum != 4327044709594624079 {
		t.Fatalf("连续 %d 个 Uint64 之和 = %d，基线为 %d", words, sum, uint64(4327044709594624079))
	}
}

func TestMersenneTwister_CrashSurfaces(t *testing.T) {
	var zero int64
	mustNotPanic(t, "Seed(nil)", func() {
		mt := NewMersenneTwister(&zero)
		_ = mt.Int63()
		mt.Seed(nil)
		_ = mt.Int63()
	})
	mustNotPanic(t, "零值 MersenneTwister 取值", func() {
		var mt MersenneTwister
		first := mt.Uint64()
		second := mt.Uint64()
		if first == 0 && second == 0 {
			t.Error("零值实例应自行播种出值，不应恒为 0")
		}
	})
	mustNotPanic(t, "resetMersenneTwister 用于零值实例", func() {
		var mt MersenneTwister
		mt.resetMersenneTwister()
		_ = mt.Int63()
	})
}

// TestMersenneTwister_Int31Range 覆盖文档承诺的非负区间（改前有 2^-31 概率出负数）。
func TestMersenneTwister_Int31Range(t *testing.T) {
	seed := int64(7)
	mt := NewMersenneTwister(&seed)
	for i := 0; i < 200000; i++ {
		if v := mt.Int31(); v < 0 {
			t.Fatalf("Int31 应非负，实际: %d", v)
		}
	}
}

func TestMonteCarlo_CrashSurfaces(t *testing.T) {
	own := int64(0)
	mustNotPanic(t, "NewMonteCarlo(nil)", func() {
		mc := NewMonteCarlo(nil)
		if mc.M != math.MaxInt64 {
			t.Errorf("M = %d，应为输出上界 %d", mc.M, int64(math.MaxInt64))
		}
		_ = mc.nextInt64()
	})
	mustNotPanic(t, "NewMonteCarlo(&0)", func() {
		mc := NewMonteCarlo(&own)
		_ = mc.nextInt64()
	})
	mustNotPanic(t, "零值 MonteCarlo 取值", func() {
		var mc MonteCarlo
		_ = mc.nextInt64()
	})
}

// TestMonteCarlo_Stream 校验换参数后仍是确定性的 64 位发生器，且值域落在 [0,M]。
func TestMonteCarlo_Stream(t *testing.T) {
	seedA, seedB := int64(2026), int64(2026)
	mcA, mcB := NewMonteCarlo(&seedA), NewMonteCarlo(&seedB)
	seen := make(map[int64]int, 1000)
	for i := 0; i < 1000; i++ {
		v := mcA.nextInt64()
		if v < 0 || v > mcA.M {
			t.Fatalf("取值越出 [0,M]: %d", v)
		}
		seen[v]++
		if got := mcB.nextInt64(); got != v {
			t.Fatalf("同种子第 %d 次序列不一致: %d != %d", i, got, v)
		}
	}
	if len(seen) < 990 {
		t.Fatalf("1000 次取值只得到 %d 个不同值，发生器退化", len(seen))
	}
}

// TestRandom_CrashSurfaces 钉住旧实现里不可 recover 的 integer divide by zero 路径。
func TestRandom_CrashSurfaces(t *testing.T) {
	mustNotPanic(t, "零值 Random 取值", func() {
		var r Random
		for i := 0; i < 10; i++ {
			if v := r.NextInt64(); v < 0 {
				t.Fatalf("取值应为非负，实际: %d", v)
			}
		}
	})
	mustNotPanic(t, "nil 接收者取值", func() {
		var r *Random
		_ = r.NextInt64()
	})
	mustNotPanic(t, "nil 接收者 New", func() {
		var r *Random
		r.New()
	})
}

// TestRandom_NewReplaysConstructionSeed 校验 New() 原地重播构造种子，
// 不再像旧实现那样按被两路累加漂移后的种子重播（那会让重播结果取决于已取次数）。
func TestRandom_NewReplaysConstructionSeed(t *testing.T) {
	r := New(99)
	r.NextInt64()
	r.NextInt64()

	r.New()
	first := r.NextInt64()
	r.NextInt64()
	r.NextInt64()
	r.New()
	if again := r.NextInt64(); again != first {
		t.Fatalf("New() 两次后首个取值不一致: %d != %d", again, first)
	}
}

// TestNextInt64_LowBitsUsed 检查两路异或后的低位真的可用：
// 旧的 MonteCarlo 只有 17~24 位状态，最低几位几乎恒定时这里就会露出来。
func TestNextInt64_LowBitsUsed(t *testing.T) {
	const draws = 60000
	var odd int
	seen := make(map[int64]int, draws)
	for i := 0; i < draws; i++ {
		v := NextInt64()
		if v < 0 {
			t.Fatalf("取值应为非负，实际: %d", v)
		}
		seen[v]++
		odd += int(v & 1)
	}
	if odd < draws/4 || odd > draws*3/4 {
		t.Fatalf("最低位失衡：%d/%d 为奇数", odd, draws)
	}
	if len(seen) < draws*9/10 {
		t.Fatalf("%d 次取值只有 %d 个不同值", draws, len(seen))
	}
}

// TestRandomStrBucketsSpread 覆盖调用方的真实用法：按低位取模选字符/选层数。
func TestRandomStrBucketsSpread(t *testing.T) {
	const (
		draws   = 60000
		buckets = 62
	)
	counts := make([]int, buckets)
	for i := 0; i < draws; i++ {
		counts[NextInt64()%buckets]++
	}
	// 期望每桶约 968；单桶超过期望 3 倍即判定偏斜
	if limit := draws / buckets * 3; limit > 0 {
		for b, c := range counts {
			if c > limit {
				t.Fatalf("桶 %d 命中 %d 次，超过 %d 的偏斜门限", b, c, limit)
			}
			if c == 0 {
				t.Fatalf("桶 %d 一次都没命中，低位退化", b)
			}
		}
	}
}

// TestConcurrentNextInt64 让多个 goroutine 抢同一个共享实例与包级默认实例。
// 本机无 gcc 故跑不了 -race：这里只保证不崩、不出负值、不重复成一片，
// 并发正确性来自构造（写一次的引擎指针 + 单一互斥 + sync.OnceValue）。
func TestConcurrentNextInt64(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("单核机器上的并发压力测试无意义")
	}

	const (
		goroutines = 8
		draws      = 20000
	)
	r := New(42)

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	bad := make(chan string, goroutines*2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < draws; j++ {
				if v := r.NextInt64(); v < 0 {
					bad <- "共享实例出现负值"
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < draws; j++ {
				if v := NextInt64(); v < 0 {
					bad <- "包级默认实例出现负值"
					return
				}
			}
		}()
	}
	wg.Wait()
	close(bad)
	for msg := range bad {
		t.Error(msg)
	}
}

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

func TestNewMersenneTwister_NilSeed(t *testing.T) {
	mt := NewMersenneTwister(nil)
	if mt == nil {
		t.Fatal("nil 种子应使用回退非零值，不应返回 nil")
	}
	// 第一次取值应成功
	if v := mt.Int63(); v < 0 {
		t.Fatalf("Int63 应非负，实际: %d", v)
	}
}

func TestNewMersenneTwister_ZeroSeed(t *testing.T) {
	zero := int64(0)
	mt := NewMersenneTwister(&zero)
	if mt == nil {
		t.Fatal("0 种子应回退非零值，不应返回 nil")
	}
	if zero == 0 {
		t.Fatal("零种子应被改为非零值")
	}
}

func TestMersenneTwister_Intn(t *testing.T) {
	mt := NewMersenneTwister(nil)
	for i := 0; i < 100; i++ {
		v := mt.Intn(10)
		if v < 0 || v >= 10 {
			t.Fatalf("Intn(10) 越界: %d", v)
		}
	}
}

func TestMersenneTwister_FloatRange(t *testing.T) {
	mt := NewMersenneTwister(nil)
	if v := mt.Float64Range(1.0, 5.0); v < 1.0 || v >= 5.0 {
		t.Fatalf("Float64Range 越界: %f", v)
	}
	if v := mt.Float32Range(1.0, 5.0); v < 1.0 || v >= 5.0 {
		t.Fatalf("Float32Range 越界: %f", v)
	}
}

func TestIsPrime(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{3, true},
		{4, false},
		{5, true},
		{9, false},
		{11, true},
		{17, true},
		{25, false},
		{97, true},
	}
	for _, c := range cases {
		if got := isPrime(c.n); got != c.want {
			t.Fatalf("isPrime(%d)=%v want=%v", c.n, got, c.want)
		}
	}
}

func TestGcd(t *testing.T) {
	cases := []struct {
		a, b, want int64
	}{
		{12, 8, 4},
		{17, 5, 1},
		{100, 0, 100},
		{0, 0, 0}, // 退化情况
	}
	for _, c := range cases {
		if got := gcd(c.a, c.b); got != c.want {
			t.Fatalf("gcd(%d,%d)=%d want=%d", c.a, c.b, got, c.want)
		}
	}
}

func TestNextInt64(t *testing.T) {
	v := NextInt64()
	if v < 0 {
		t.Fatalf("NextInt64 应非负，实际: %d", v)
	}
}

func TestMersenneTwister_Determinism(t *testing.T) {
	seed := int64(123)
	mt1 := NewMersenneTwister(&seed)
	seed2 := int64(123)
	mt2 := NewMersenneTwister(&seed2)
	for i := 0; i < 10; i++ {
		if mt1.Int63() != mt2.Int63() {
			t.Fatalf("相同种子第 %d 次序列不一致", i)
		}
	}
}
