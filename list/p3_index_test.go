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
