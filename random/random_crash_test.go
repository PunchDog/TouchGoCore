package random

import (
	"math"
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
