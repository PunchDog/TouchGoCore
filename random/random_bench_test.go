package random

import "testing"

func BenchmarkMersenneTwister_Uint64(b *testing.B) {
	mt := NewMersenneTwister(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mt.Uint64()
	}
}

func BenchmarkMersenneTwister_Int63(b *testing.B) {
	mt := NewMersenneTwister(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mt.Int63()
	}
}

func BenchmarkMersenneTwister_Float64(b *testing.B) {
	mt := NewMersenneTwister(nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mt.Float64()
	}
}

func BenchmarkNextInt64(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = NextInt64()
	}
}

func BenchmarkIsPrime(b *testing.B) {
	nums := []int64{2, 97, 1009, 10007, 99991}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, n := range nums {
			_ = isPrime(n)
		}
	}
}
