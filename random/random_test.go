package random

import (
	"sync"
	"testing"
)

func BenchmarkMersenneTwister_NextInt64(b *testing.B) {
	seed := int64(12345)
	mt := NewMersenneTwister(&seed)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mt.nextInt64()
	}
}

func BenchmarkMersenneTwister_NextInt64_Parallel(b *testing.B) {
	seed := int64(12345)
	mt := NewMersenneTwister(&seed)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mt.nextInt64()
		}
	})
}

func BenchmarkMonteCarlo_NextInt64(b *testing.B) {
	seed := int64(12345)
	mc := NewMonteCarlo(&seed)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mc.nextInt64()
	}
}

// 测试 Random 包装器的性能
func BenchmarkRandom_NextInt64(b *testing.B) {
	r := New(12345)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.NextInt64()
	}
}

// 测试锁竞争对 MersenneTwister 的影响
func BenchmarkMersenneTwister_Concurrent(b *testing.B) {
	seed := int64(12345)
	mt := NewMersenneTwister(&seed)
	var wg sync.WaitGroup
	threads := 4
	b.ResetTimer()
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < b.N/threads; j++ {
				mt.nextInt64()
			}
		}()
	}
	wg.Wait()
}
