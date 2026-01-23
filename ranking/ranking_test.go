package ranking

import (
	"testing"
	"time"
)

func BenchmarkRankTree_AddRankInfo(b *testing.B) {
	rt := NewRankTree()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.AddRankInfo(int64(i), int64(i), time.Now().UnixNano())
	}
}

func BenchmarkRankTree_QueryRankInfo(b *testing.B) {
	rt := NewRankTree()
	for i := 0; i < 10000; i++ {
		rt.AddRankInfo(int64(i), int64(i), time.Now().UnixNano())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.QueryRankInfo(int64(i % 10000))
	}
}

func BenchmarkRankTree_QueryByRankRange(b *testing.B) {
	rt := NewRankTree()
	for i := 0; i < 10000; i++ {
		rt.AddRankInfo(int64(i), int64(i), time.Now().UnixNano())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.QueryByRankRange(1, 100)
	}
}

func BenchmarkRankTree_UpdateRankInfo(b *testing.B) {
	rt := NewRankTree()
	for i := 0; i < 10000; i++ {
		rt.AddRankInfo(int64(i), int64(i), time.Now().UnixNano())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.UpdateRankInfo(int64(i % 10000), int64(i), time.Now().UnixNano())
	}
}

func BenchmarkRankTree_RemoveRankInfo(b *testing.B) {
	rt := NewRankTree()
	for i := 0; i < 10000; i++ {
		rt.AddRankInfo(int64(i), int64(i), time.Now().UnixNano())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rt.RemoveRankInfo(int64(i % 10000))
	}
}

func TestRankTree_Basic(t *testing.T) {
	rt := NewRankTree()
	
	// 测试添加排名信息
	rt.AddRankInfo(1, 100, time.Now().UnixNano())
	rt.AddRankInfo(2, 200, time.Now().UnixNano())
	rt.AddRankInfo(3, 150, time.Now().UnixNano())
	
	// 测试查询排名
	info := rt.QueryRankInfo(1)
	if info == nil || info.Rank != 3 {
		t.Errorf("Expected rank 3 for uid 1, got %d", info.Rank)
	}
	
	info = rt.QueryRankInfo(2)
	if info == nil || info.Rank != 1 {
		t.Errorf("Expected rank 1 for uid 2, got %d", info.Rank)
	}
	
	info = rt.QueryRankInfo(3)
	if info == nil || info.Rank != 2 {
		t.Errorf("Expected rank 2 for uid 3, got %d", info.Rank)
	}
	
	// 测试范围查询
	infos := rt.QueryByRankRange(1, 2)
	if len(infos) != 2 {
		t.Errorf("Expected 2 items in rank range 1-2, got %d", len(infos))
	}
	
	// 测试更新排名
	rt.UpdateRankInfo(1, 250, time.Now().UnixNano())
	info = rt.QueryRankInfo(1)
	if info == nil || info.Rank != 1 {
		t.Errorf("Expected rank 1 for uid 1 after update, got %d", info.Rank)
	}
	
	// 测试删除排名
	if !rt.RemoveRankInfo(2) {
		t.Error("Failed to remove uid 2")
	}
	info = rt.QueryRankInfo(2)
	if info != nil {
		t.Error("Expected nil for removed uid 2")
	}
	
	// 测试获取排名长度
	if rt.RankLength() != 2 {
		t.Errorf("Expected rank length 2, got %d", rt.RankLength())
	}
}
