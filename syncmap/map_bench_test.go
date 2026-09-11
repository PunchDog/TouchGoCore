package syncmap

import "testing"

func BenchmarkMap_StoreLoad(b *testing.B) {
	m := NewMap[int, int]()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Store(i%1024, i)
		_, _ = m.Load(i % 1024)
	}
}

func BenchmarkMap_Store(b *testing.B) {
	m := NewMap[int, int]()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Store(i, i)
	}
}

func BenchmarkMap_Load(b *testing.B) {
	m := NewMap[int, int]()
	for i := 0; i < 1024; i++ {
		m.Store(i, i*10)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Load(i % 1024)
	}
}

func BenchmarkMap_Range(b *testing.B) {
	m := NewMap[int, int]()
	for i := 0; i < 100; i++ {
		m.Store(i, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Range(func(k, v int) bool {
			_ = k + v
			return true
		})
	}
}

func BenchmarkMapAny_StoreLoad(b *testing.B) {
	m := NewAny()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Store(i%1024, i)
		_, _ = m.Load(i % 1024)
	}
}
