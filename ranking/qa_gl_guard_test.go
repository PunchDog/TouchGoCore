package ranking

import (
	"sync"
	"sync/atomic"
	"testing"
)

// QA-GL: RankTree 查询防护与「返回值拷贝」契约回归。
//
// 旧实现问题：
//  1. QueryByRank / QueryByRankRange / RankLength 缺 rt.Sl == nil 防护，
//     未初始化的 RankTree 直接 nil 解引用 panic（确定性红）；
//  2. QueryRankInfo 在读锁下写共享条目的 info.Rank（读锁下写 = 数据竞争），
//     且返回内部指针，调用方可无锁改内部状态（确定性红：改返回值污染树）；
//  3. searchByRankRange 在读锁下写 i.Value.Rank（同类竞态，一并修复）。

func TestQaGL_ZeroValueRankTreeNoPanic(t *testing.T) {
	var rt RankTree // Sl == nil，未初始化

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("零值 RankTree 查询 panic: %v", rec)
		}
	}()

	if got := rt.QueryByRank(1); got != nil {
		t.Errorf("QueryByRank = %+v, 期望 nil", got)
	}
	if got := rt.QueryByRankRange(1, 2); got != nil {
		t.Errorf("QueryByRankRange = %+v, 期望 nil", got)
	}
	if got := rt.RankLength(); got != 0 {
		t.Errorf("RankLength = %d, 期望 0", got)
	}
	if got := rt.QueryRankInfo(1); got != nil {
		t.Errorf("QueryRankInfo = %+v, 期望 nil", got)
	}

	var rtNil *RankTree // nil 接收者同样不得 panic
	if got := rtNil.QueryByRank(1); got != nil {
		t.Errorf("nil QueryByRank = %+v, 期望 nil", got)
	}
	if got := rtNil.RankLength(); got != 0 {
		t.Errorf("nil RankLength = %d, 期望 0", got)
	}
}

func TestQaGL_QueryRankInfoReturnsCopy(t *testing.T) {
	rt := NewRankTree()
	rt.AddRankInfo(1, 100, 5)
	rt.AddRankInfo(2, 200, 5)

	info := rt.QueryRankInfo(1)
	if info == nil {
		t.Fatal("QueryRankInfo(1) = nil")
	}
	if info.Rank != 2 || info.Value != 100 {
		t.Fatalf("QueryRankInfo(1) = %+v, 期望 Rank=2 Value=100", info)
	}

	// 调用方篡改返回值不得污染树内状态（旧实现返回内部指针 → 红）
	info.Value = 999
	info.Rank = 42
	info.Timestamp = 1

	again := rt.QueryRankInfo(1)
	if again == nil {
		t.Fatal("再次 QueryRankInfo(1) = nil")
	}
	if again.Value != 100 || again.Timestamp != 5 || again.Rank != 2 {
		t.Fatalf("返回值篡改污染了内部状态: %+v, 期望 Value=100 Timestamp=5 Rank=2", again)
	}

	// 树结构未被破坏：仍能正常更新与删除
	rt.UpdateRankInfo(1, 300, 6)
	if got := rt.QueryRankInfo(1); got == nil || got.Rank != 1 {
		t.Fatalf("更新后 QueryRankInfo(1) = %+v, 期望 Rank=1", got)
	}
}

func TestQaGL_QueryByRankAndRangeReturnCopies(t *testing.T) {
	rt := NewRankTree()
	rt.AddRankInfo(1, 100, 5)
	rt.AddRankInfo(2, 200, 5)
	rt.AddRankInfo(3, 300, 5)

	top := rt.QueryByRank(1)
	if top == nil || top.ID != 3 || top.Value != 300 {
		t.Fatalf("QueryByRank(1) = %+v, 期望 uid=3 val=300", top)
	}
	top.Value = -1 // 篡改返回值

	infos := rt.QueryByRankRange(1, 2)
	if len(infos) != 2 {
		t.Fatalf("QueryByRankRange(1,2) 长度 %d, 期望 2", len(infos))
	}
	if infos[0].Value != 300 {
		t.Fatalf("QueryByRank 的返回值篡改污染内部状态: %+v", infos[0])
	}
	// Rank 写在拷贝上且顺序正确
	if infos[0].Rank != 1 || infos[1].Rank != 2 {
		t.Errorf("Rank = %d,%d, 期望 1,2", infos[0].Rank, infos[1].Rank)
	}
}

func TestQaGL_SaveRankTreesConcurrentWithWrites(t *testing.T) {
	const rtype = int64(90210) // 专用类型，避免与其它用例串扰
	ResetRankTree(rtype)
	rt := GetRankTree(rtype)
	// 同步保底写入：避免写协程被调度饥饿时树为空、Save 无该类型行造成误报
	for i := int64(0); i < 10; i++ {
		rt.AddRankInfo(i, i*10, i)
	}

	var panics atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
			}
		}()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				rt.AddRankInfo(int64(i%50), int64(i), int64(i))
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
			}
		}()
		for i := 0; i < 200; i++ {
			_ = Save()
		}
		close(stop)
	}()
	wg.Wait()

	if n := panics.Load(); n != 0 {
		t.Fatalf("Save 与写入并发期间发生 %d 次 panic", n)
	}
	infos := Save()
	found := false
	for _, in := range infos {
		if in.Type == rtype {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Save 结果缺少测试树的数据")
	}
}
