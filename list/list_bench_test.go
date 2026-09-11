package list

import "testing"

func BenchmarkList_Add(b *testing.B) {
	l := NewList()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Add(&Node{})
	}
}

func BenchmarkList_Range(b *testing.B) {
	l := NewList()
	for i := 0; i < 1000; i++ {
		l.Add(&Node{})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Range(func(n INode) bool {
			_ = n.GetNode()
			return true
		})
	}
}

func BenchmarkList_Clear(b *testing.B) {
	l := NewList()
	for i := 0; i < 1000; i++ {
		l.Add(&Node{})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Range(func(n INode) bool {
			n.GetNode().Remove()
			return true
		})
		l.Clear()
		for j := 0; j < 1000; j++ {
			l.Add(&Node{})
		}
	}
}
