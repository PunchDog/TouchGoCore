package ranking

import "testing"

func BenchmarkSkipList_Insert(b *testing.B) {
	sl := newSkipList()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sl.insert(int64(i), &RankInfo{ID: int64(i), Value: int64(i)})
	}
}

func BenchmarkSkipList_Search(b *testing.B) {
	sl := newSkipList()
	const N = 10000
	for i := 0; i < N; i++ {
		sl.insert(int64(i), &RankInfo{ID: int64(i), Value: int64(i)})
	}
	target := &RankInfo{ID: 5000, Value: 5000, Timestamp: 0}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sl.search(int64(i%N), target)
	}
}

func BenchmarkRankTree_AddRankInfo(b *testing.B) {
	rt := NewRankTree()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.AddRankInfo(int64(i), int64(i*10), int64(i))
	}
}

func BenchmarkRankInfo_Compare(b *testing.B) {
	a := &RankInfo{ID: 1, Timestamp: 100}
	c := &RankInfo{ID: 2, Timestamp: 200}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = a.Compare(c)
	}
}
