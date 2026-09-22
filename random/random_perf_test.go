package random

import (
	"sync"
	"testing"
)

// lockPair 只用于测出「一把无争用互斥锁的一次加解锁」的地板价，
// 好判断 MersenneTwister 的耗时到底花在锁上还是花在算术上。
type lockPair struct {
	mu sync.Mutex
	n  int
}

func BenchmarkLockFloor(b *testing.B) {
	var lp lockPair
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lp.mu.Lock()
		lp.n++
		lp.mu.Unlock()
	}
}

// BenchmarkMersenneTwister_DrawNoLock 在计时区外先拿住锁，测纯算术（twist 摊销 + 升温）
// 的单次成本；与 BenchmarkMersenneTwister_Uint64 的差值即锁与出值累加的开销。
func BenchmarkMersenneTwister_DrawNoLock(b *testing.B) {
	seed := int64(1)
	mt := NewMersenneTwister(&seed)

	mt.mu.Lock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mt.nextUint64()
	}
	b.StopTimer()
	mt.mu.Unlock()
}

// BenchmarkMonteCarlo_DrawNoLock 同上，测 xoshiro256** 的单次纯算术成本。
func BenchmarkMonteCarlo_DrawNoLock(b *testing.B) {
	seed := int64(1)
	mc := NewMonteCarlo(&seed)

	mc.mu.Lock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mc.nextUint64Locked()
	}
	b.StopTimer()
	mc.mu.Unlock()
}
