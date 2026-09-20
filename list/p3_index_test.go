package list

import "testing"

// TestUnindexedListKeepsListSemantics 回归（S63）：关掉 ID 索引后，
// 增删遍历与长度统计必须和索引版完全一致，只是不再取号、不再可按 ID 查询。
func TestUnindexedListKeepsListSemantics(t *testing.T) {
	l := NewUnindexedList()
	nodes := make([]INode, 0, 4)
	for i := 0; i < 4; i++ {
		n := NewNode(i, nil)
		if !l.Add(n) {
			t.Fatalf("✘ Add 失败: %d", i)
		}
		nodes = append(nodes, n)
	}
	if l.Length() != 4 {
		t.Fatalf("✘ 长度不符: %d", l.Length())
	}
	if got := nodes[0].GetId(); got != 0 {
		t.Fatalf("✘ 无索引链表仍在取号: id=%d", got)
	}
	if l.Get(nodes[2].GetId()) != nil {
		t.Fatalf("✘ 无索引链表不应查得到节点")
	}

	// 中间节点摘除后链序必须完整
	nodes[1].Remove()
	if l.Length() != 3 {
		t.Fatalf("✘ Remove 后长度不符: %d", l.Length())
	}
	var seen []int
	l.Range(func(node INode) bool {
		seen = append(seen, node.GetData().(int))
		return true
	})
	if len(seen) != 3 || seen[0] != 0 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("✘ 链序错乱: %v", seen)
	}

	l.Clear()
	if l.Length() != 0 {
		t.Fatalf("✘ Clear 后长度不为 0: %d", l.Length())
	}
}

// TestUnindexedListClearsStaleID 契约守护（阶段12 复核 F6）：节点从索引链表迁入
// 无索引链表后必须满足「GetId 恒为 0」，迁回索引表则必须重新取号。
//
// 评审提出的「迁入带着旧号」实测不成立：Add 先 detach，而 removeNodeLocked 会把
// id 归零，无索引分支不取号也就无需再清。既然结论依赖另一处实现的副作用，就更该
// 有用例钉住——否则哪天清零被挪走，NewUnindexedList 的注释承诺会静默失效。
func TestUnindexedListClearsStaleID(t *testing.T) {
	indexed := NewList()
	n := NewNode("payload", nil)
	if !indexed.Add(n) {
		t.Fatal("✘ 索引表 Add 失败")
	}
	id := n.GetId()
	if id == 0 {
		t.Fatal("✘ 索引表未取号")
	}

	plain := NewUnindexedList()
	if !plain.Add(n) {
		t.Fatal("✘ 无索引表 Add 失败")
	}
	if got := n.GetId(); got != 0 {
		t.Fatalf("✘ 迁入无索引表后仍带旧号: id=%d，该号在任何表里都查不到", got)
	}
	if indexed.Get(id) != nil {
		t.Fatal("✘ 迁移后原索引表仍留着旧键")
	}
	if plain.Get(0) != nil {
		t.Fatal("✘ 无索引表不该查得到节点")
	}

	if !indexed.Add(n) {
		t.Fatal("✘ 迁回索引表 Add 失败")
	}
	if n.GetId() == 0 {
		t.Fatal("✘ 迁回索引表没有重新取号")
	}
	if indexed.Get(n.GetId()) != n {
		t.Fatal("✘ 迁回索引表后按新号查不到")
	}
}

// TestUnindexedListInsertAfterKeepsNoID InsertAfter/InsertBefore 走同一个取号入口，
// 无索引表上它们同样不能留下号；顺手确认插入本身仍然可用（时间轮不用，但宿主会把
// 自建无索引表配合 InsertAfter 用）。
func TestUnindexedListInsertAfterKeepsNoID(t *testing.T) {
	l := NewUnindexedList()
	first := NewNode(1, nil)
	if !l.Add(first) {
		t.Fatal("✘ Add 失败")
	}
	after := first.InsertAfter(2)
	if after == nil {
		t.Fatal("✘ InsertAfter 返回 nil")
	}
	if after.GetId() != 0 {
		t.Fatalf("✘ 无索引表的插入路径仍在取号: id=%d", after.GetId())
	}
	if l.Length() != 2 {
		t.Fatalf("✘ 插入后长度不符: %d", l.Length())
	}
}

// TestIndexedListKeepsIDLookup 对照：默认 NewList 的按 ID 查询语义不变。
func TestIndexedListKeepsIDLookup(t *testing.T) {
	l := NewList()
	n := NewNode("payload", nil)
	if !l.Add(n) {
		t.Fatal("Add 失败")
	}
	id := n.GetId()
	if id == 0 {
		t.Fatal("✘ 索引链表未取号")
	}
	if l.Get(id) != n {
		t.Fatalf("✘ 按 ID 取不到节点: id=%d", id)
	}
	n.Remove()
	if l.Get(id) != nil {
		t.Fatal("✘ 节点删除后索引残留")
	}
}

// BenchmarkAddRemoveIndexed 与 BenchmarkAddRemoveUnindexed 对照 S63 的收益：
// 索引版每次入链要 CAS 取号（含一次时钟读取）并写 map，摘链再查删一次。
func BenchmarkAddRemoveIndexed(b *testing.B) {
	benchmarkAddRemove(b, NewList())
}

func BenchmarkAddRemoveUnindexed(b *testing.B) {
	benchmarkAddRemove(b, NewUnindexedList())
}

func benchmarkAddRemove(b *testing.B, l *List) {
	b.ReportAllocs()
	n := NewNode(nil, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !l.Add(n) {
			b.Fatal("Add 失败")
		}
		n.Remove()
	}
}

// BenchmarkRangeWheelLike 模拟时间轮每 tick 的一次整表遍历。
func BenchmarkRangeWheelLike(b *testing.B) {
	for _, tc := range []struct {
		name string
		l    *List
	}{{"indexed", NewList()}, {"unindexed", NewUnindexedList()}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < 64; i++ {
				n := NewNode(i, nil)
				if !tc.l.Add(n) {
					b.Fatal("Add 失败")
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tc.l.Range(func(INode) bool { return true })
			}
		})
	}
}
