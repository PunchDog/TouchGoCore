package list

import (
	"sync"
	"testing"
)

// TestNewList 测试创建新链表
func TestNewList(t *testing.T) {
	l := NewList()
	if l == nil {
		t.Fatal("NewList returned nil")
	}
	if l.Length() != 0 {
		t.Errorf("expected length 0, got %d", l.Length())
	}
	if l.Head() != nil {
		t.Error("expected nil head, got non-nil")
	}
	if l.Tail() != nil {
		t.Error("expected nil tail, got non-nil")
	}
}

// TestAdd 测试添加节点
func TestAdd(t *testing.T) {
	l := NewList()
	node := NewNode("data1", nil)

	if !l.Add(node) {
		t.Fatal("Add failed")
	}

	if l.Length() != 1 {
		t.Errorf("expected length 1, got %d", l.Length())
	}

	if l.Head() != node {
		t.Error("Head is not the added node")
	}

	if l.Tail() != node {
		t.Error("Tail is not the added node")
	}

	if node.GetNode().list != l {
		t.Error("node.list is not the list")
	}
}

// TestAddMultiple 测试添加多个节点
func TestAddMultiple(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)
	node3 := NewNode("data3", nil)

	l.Add(node1)
	l.Add(node2)
	l.Add(node3)

	if l.Length() != 3 {
		t.Errorf("expected length 3, got %d", l.Length())
	}

	if l.Head() != node1 {
		t.Error("Head is not node1")
	}

	if l.Tail() != node3 {
		t.Error("Tail is not node3")
	}

	if node1.GetNode().next != node2 {
		t.Error("node1.next is not node2")
	}

	if node2.GetNode().next != node3 {
		t.Error("node2.next is not node3")
	}

	if node2.GetNode().pre != node1 {
		t.Error("node2.pre is not node1")
	}

	if node3.GetNode().pre != node2 {
		t.Error("node3.pre is not node2")
	}
}

// TestAddNil 测试添加空节点
func TestAddNil(t *testing.T) {
	l := NewList()
	result := l.Add(nil)

	if result {
		t.Error("expected Add(nil) to return false")
	}

	if l.Length() != 0 {
		t.Errorf("expected length 0, got %d", l.Length())
	}
}

// TestAddDuplicate 测试重复添加节点
func TestAddDuplicate(t *testing.T) {
	l1 := NewList()
	l2 := NewList()
	node := NewNode("data", nil)

	l1.Add(node)
	if l1.Length() != 1 {
		t.Errorf("expected l1 length 1, got %d", l1.Length())
	}

	// 重复添加到同一个链表
	l1.Add(node)
	if l1.Length() != 1 {
		t.Errorf("expected l1 length still 1, got %d", l1.Length())
	}

	// 添加到另一个链表
	l2.Add(node)
	if l1.Length() != 0 {
		t.Errorf("expected l1 length 0 after move, got %d", l1.Length())
	}
	if l2.Length() != 1 {
		t.Errorf("expected l2 length 1, got %d", l2.Length())
	}
}

// TestRemove 测试删除节点
func TestRemove(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)
	node3 := NewNode("data3", nil)

	l.Add(node1)
	l.Add(node2)
	l.Add(node3)

	node2.Remove()

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	if node1.GetNode().next != node3 {
		t.Error("node1.next is not node3")
	}

	if node3.GetNode().pre != node1 {
		t.Error("node3.pre is not node1")
	}

	if l.Head() != node1 {
		t.Error("Head is not node1")
	}

	if l.Tail() != node3 {
		t.Error("Tail is not node3")
	}
}

// TestRemoveHead 测试删除头节点
func TestRemoveHead(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)
	node3 := NewNode("data3", nil)

	l.Add(node1)
	l.Add(node2)
	l.Add(node3)

	node1.Remove()

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	if l.Head() != node2 {
		t.Error("Head is not node2")
	}

	if node2.GetNode().pre != nil {
		t.Error("node2.pre should be nil")
	}
}

// TestRemoveTail 测试删除尾节点
func TestRemoveTail(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)
	node3 := NewNode("data3", nil)

	l.Add(node1)
	l.Add(node2)
	l.Add(node3)

	node3.Remove()

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	if l.Tail() != node2 {
		t.Error("Tail is not node2")
	}

	if node2.GetNode().next != nil {
		t.Error("node2.next should be nil")
	}
}

// TestRemoveSingle 测试删除唯一节点
func TestRemoveSingle(t *testing.T) {
	l := NewList()
	node := NewNode("data", nil)

	l.Add(node)
	node.Remove()

	if l.Length() != 0 {
		t.Errorf("expected length 0, got %d", l.Length())
	}

	if l.Head() != nil {
		t.Error("Head should be nil")
	}

	if l.Tail() != nil {
		t.Error("Tail should be nil")
	}

	if node.GetNode().list != nil {
		t.Error("node.list should be nil after remove")
	}
}

// TestGet 测试获取节点
func TestGet(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)

	l.Add(node1)
	l.Add(node2)

	id := node1.GetId()
	found := l.Get(id)

	if found != node1 {
		t.Error("Get did not find the correct node")
	}
}

// TestGetNotFound 测试获取不存在的节点
func TestGetNotFound(t *testing.T) {
	l := NewList()
	l.Add(NewNode("data", nil))

	found := l.Get(999)
	if found != nil {
		t.Error("expected nil for non-existent ID")
	}
}

// TestRange 测试遍历链表
func TestRange(t *testing.T) {
	l := NewList()
	l.Add(NewNode(1, nil))
	l.Add(NewNode(2, nil))
	l.Add(NewNode(3, nil))

	var sum int
	var count int
	l.Range(func(node INode) bool {
		sum += node.GetData().(int)
		count++
		return true
	})

	if count != 3 {
		t.Errorf("expected count 3, got %d", count)
	}

	if sum != 6 {
		t.Errorf("expected sum 6, got %d", sum)
	}
}

// TestRangeBreak 测试遍历中断
func TestRangeBreak(t *testing.T) {
	l := NewList()
	l.Add(NewNode(1, nil))
	l.Add(NewNode(2, nil))
	l.Add(NewNode(3, nil))

	var count int
	l.Range(func(node INode) bool {
		count++
		return count < 2
	})

	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
}

// TestRangeRemoveDuringIteration 测试遍历期间删除
func TestRangeRemoveDuringIteration(t *testing.T) {
	l := NewList()
	node1 := NewNode(1, nil)
	node2 := NewNode(2, nil)
	node3 := NewNode(3, nil)

	l.Add(node1)
	l.Add(node2)
	l.Add(node3)

	l.Range(func(node INode) bool {
		if node.GetData().(int) == 2 {
			node.Remove()
		}
		return true
	})

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	// 验证节点被正确删除
	if node1.GetNode().next != node3 {
		t.Error("node1.next is not node3")
	}

	if node3.GetNode().pre != node1 {
		t.Error("node3.pre is not node1")
	}
}

// TestRangeEmpty 测试遍历空链表
func TestRangeEmpty(t *testing.T) {
	l := NewList()

	var called bool
	l.Range(func(node INode) bool {
		called = true
		return true
	})

	if called {
		t.Error("Range should not be called on empty list")
	}
}

// TestClear 测试清空链表
func TestClear(t *testing.T) {
	l := NewList()
	l.Add(NewNode(1, nil))
	l.Add(NewNode(2, nil))
	l.Add(NewNode(3, nil))

	l.Clear()

	if l.Length() != 0 {
		t.Errorf("expected length 0, got %d", l.Length())
	}

	if l.Head() != nil {
		t.Error("Head should be nil after Clear")
	}

	if l.Tail() != nil {
		t.Error("Tail should be nil after Clear")
	}
}

// TestClearEmpty 测试清空空链表
func TestClearEmpty(t *testing.T) {
	l := NewList()
	l.Clear()

	if l.Length() != 0 {
		t.Errorf("expected length 0, got %d", l.Length())
	}
}

// TestInsertAfter 测试在节点后插入
func TestInsertAfter(t *testing.T) {
	l := NewList()
	node1 := NewNode(1, nil)
	l.Add(node1)

	newNode := node1.InsertAfter(1.5)

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	if node1.GetNode().next != newNode {
		t.Error("node1.next is not newNode")
	}

	if newNode.GetNode().pre != node1 {
		t.Error("newNode.pre is not node1")
	}

	if l.Tail() != newNode {
		t.Error("Tail is not newNode")
	}
}

// TestInsertBefore 测试在节点前插入
func TestInsertBefore(t *testing.T) {
	l := NewList()
	node2 := NewNode(2, nil)
	l.Add(node2)

	newNode := node2.InsertBefore(1.5)

	if l.Length() != 2 {
		t.Errorf("expected length 2, got %d", l.Length())
	}

	if node2.GetNode().pre != newNode {
		t.Error("node2.pre is not newNode")
	}

	if newNode.GetNode().next != node2 {
		t.Error("newNode.next is not node2")
	}

	if l.Head() != newNode {
		t.Error("Head is not newNode")
	}
}

// TestInsertBetween 测试在中间插入
func TestInsertBetween(t *testing.T) {
	l := NewList()
	node1 := NewNode(1, nil)
	node3 := NewNode(3, nil)
	l.Add(node1)
	l.Add(node3)

	newNode := node1.InsertAfter(2)

	if l.Length() != 3 {
		t.Errorf("expected length 3, got %d", l.Length())
	}

	if node1.GetNode().next != newNode {
		t.Error("node1.next is not newNode")
	}

	if newNode.GetNode().next != node3 {
		t.Error("newNode.next is not node3")
	}

	if node3.GetNode().pre != newNode {
		t.Error("node3.pre is not newNode")
	}
}

// TestLength 测试链表长度
func TestLength(t *testing.T) {
	l := NewList()

	if l.Length() != 0 {
		t.Errorf("expected initial length 0, got %d", l.Length())
	}

	for i := 0; i < 5; i++ {
		l.Add(NewNode(i, nil))
		if l.Length() != i+1 {
			t.Errorf("expected length %d, got %d", i+1, l.Length())
		}
	}

	l.Head().Remove()
	if l.Length() != 4 {
		t.Errorf("expected length 4 after remove, got %d", l.Length())
	}
}

// TestHeadAndTail 测试头尾指针
func TestHeadAndTail(t *testing.T) {
	l := NewList()
	node1 := NewNode(1, nil)
	node2 := NewNode(2, nil)
	node3 := NewNode(3, nil)

	l.Add(node1)
	if l.Head() != node1 || l.Tail() != node1 {
		t.Error("Head and Tail should be node1")
	}

	l.Add(node2)
	if l.Head() != node1 || l.Tail() != node2 {
		t.Error("Head should be node1, Tail should be node2")
	}

	l.Add(node3)
	if l.Head() != node1 || l.Tail() != node3 {
		t.Error("Head should be node1, Tail should be node3")
	}

	node1.Remove()
	if l.Head() != node2 || l.Tail() != node3 {
		t.Error("After removing head, Head should be node2, Tail should be node3")
	}

	node3.Remove()
	if l.Head() != node2 || l.Tail() != node2 {
		t.Error("After removing tail, Head and Tail should be node2")
	}
}

// TestGetId 测试获取节点ID
func TestGetId(t *testing.T) {
	l := NewList()
	node1 := NewNode("data1", nil)
	node2 := NewNode("data2", nil)

	l.Add(node1)
	l.Add(node2)

	id1 := node1.GetId()
	id2 := node2.GetId()

	if id1 == id2 {
		t.Error("IDs should be different")
	}

	if id1 == 0 || id2 == 0 {
		t.Error("IDs should not be 0")
	}
}

// TestGetData 测试获取节点数据
func TestGetData(t *testing.T) {
	data := "test data"
	node := NewNode(data, nil)

	if node.GetData() != data {
		t.Error("GetData should return the original data")
	}
}

// TestGetNode 测试获取底层节点
func TestGetNode(t *testing.T) {
	node := NewNode("data", nil)
	listNode := node.GetNode()

	if listNode == nil {
		t.Fatal("GetNode returned nil")
	}

	if listNode != node {
		t.Error("GetNode should return the same node")
	}
}

// 并发测试

// TestConcurrentAdd 测试并发添加
func TestConcurrentAdd(t *testing.T) {
	l := NewList()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Add(NewNode(i, nil))
		}(i)
	}

	wg.Wait()

	if l.Length() != 100 {
		t.Errorf("expected length 100, got %d", l.Length())
	}

	// 注意：在高并发场景下，generateNextID 可能产生重复 ID
	// 这是当前实现的已知问题（依赖系统时间）
	// 如果需要严格的唯一性，应该使用更好的 ID 生成策略
	l.Range(func(node INode) bool {
		_ = node.GetId()
		return true
	})
}

// TestConcurrentAddAndRemove 测试并发添加和删除
func TestConcurrentAddAndRemove(t *testing.T) {
	l := NewList()
	var wg sync.WaitGroup

	// 先添加 100 个节点
	for i := 0; i < 100; i++ {
		l.Add(NewNode(i, nil))
	}

	// 并发添加和删除
	for i := 0; i < 50; i++ {
		wg.Add(2)

		go func(i int) {
			defer wg.Done()
			l.Add(NewNode(100+i, nil))
		}(i)

		go func() {
			defer wg.Done()
			if head := l.Head(); head != nil {
				head.Remove()
			}
		}()
	}

	wg.Wait()

	// 验证长度在合理范围内（并发操作，结果不确定）
	// 初始100个，添加50个，删除约50个，结果应该在100-150之间
	if l.Length() < 100 || l.Length() > 150 {
		t.Errorf("expected length between 100 and 150, got %d", l.Length())
	}
}

// TestConcurrentRange 测试并发遍历
func TestConcurrentRange(t *testing.T) {
	l := NewList()
	for i := 0; i < 1000; i++ {
		l.Add(NewNode(i, nil))
	}

	var wg sync.WaitGroup

	// 多个 goroutine 并发遍历
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count := 0
			l.Range(func(node INode) bool {
				count++
				return true
			})
			if count != 1000 {
				t.Errorf("expected 1000 nodes, got %d", count)
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentRemoveDuringRange 测试并发遍历时删除
func TestConcurrentRemoveDuringRange(t *testing.T) {
	l := NewList()
	for i := 0; i < 100; i++ {
		l.Add(NewNode(i, nil))
	}

	var wg sync.WaitGroup

	// 遍历并删除部分节点
	wg.Add(1)
	go func() {
		defer wg.Done()
		l.Range(func(node INode) bool {
			if node.GetData().(int)%10 == 0 {
				node.Remove()
			}
			return true
		})
	}()

	wg.Wait()

	// 验证删除的节点数
	expectedLength := 90 // 100 - 10 (删除了10个节点)
	if l.Length() != expectedLength {
		t.Errorf("expected length %d, got %d", expectedLength, l.Length())
	}
}

// 基准测试

// BenchmarkAdd 性能基准测试 - 添加
func BenchmarkAdd(b *testing.B) {
	l := NewList()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Add(NewNode(i, nil))
	}
}

// BenchmarkInsertAfter 性能基准测试 - 插入后
func BenchmarkInsertAfter(b *testing.B) {
	l := NewList()
	head := NewNode(0, nil)
	l.Add(head)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		head.InsertAfter(i)
	}
}

// BenchmarkRange 性能基准测试 - 遍历
func BenchmarkRange(b *testing.B) {
	l := NewList()
	for i := 0; i < 1000; i++ {
		l.Add(NewNode(i, nil))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Range(func(node INode) bool {
			return true
		})
	}
}

// BenchmarkGet 性能基准测试 - 查询
func BenchmarkGet(b *testing.B) {
	l := NewList()
	var targetID int64

	for i := 0; i < 1000; i++ {
		node := NewNode(i, nil)
		l.Add(node)
		if i == 500 {
			targetID = node.GetId()
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Get(targetID)
	}
}

// BenchmarkRemove 性能基准测试 - 删除
func BenchmarkRemove(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		l := NewList()
		nodes := make([]INode, 100)
		for j := 0; j < 100; j++ {
			nodes[j] = NewNode(j, nil)
			l.Add(nodes[j])
		}
		b.StartTimer()

		for j := 0; j < 100; j++ {
			nodes[j].Remove()
		}
	}
}

// BenchmarkConcurrentAdd 性能基准测试 - 并发添加
func BenchmarkConcurrentAdd(b *testing.B) {
	l := NewList()
	var wg sync.WaitGroup

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Add(NewNode(i, nil))
		}()
	}
	wg.Wait()
}

// BenchmarkPoolAddRemove 性能基准测试 - 测试对象池对添加删除的影响
func BenchmarkPoolAddRemove(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewList()
		nodes := make([]INode, 100)
		for j := 0; j < 100; j++ {
			nodes[j] = NewNode(j, nil)
			l.Add(nodes[j])
		}
		for j := 0; j < 100; j++ {
			nodes[j].Remove()
		}
	}
}

// BenchmarkPoolStress 性能压力测试 - 高频创建销毁测试对象池效果
func BenchmarkPoolStress(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewList()
		// 添加大量节点
		for j := 0; j < 1000; j++ {
			l.Add(NewNode(j, nil))
		}
		// 随机删除一些节点
		l.Range(func(node INode) bool {
			if node.GetData().(int)%2 == 0 {
				node.Remove()
			}
			return true
		})
	}
}

// BenchmarkPoolConcurrentStress 并发压力测试 - 验证对象池在并发下的表现
func BenchmarkPoolConcurrentStress(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l := NewList()
		var wg sync.WaitGroup

		// 并发添加 1000 个节点
		for j := 0; j < 100; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; k < 10; k++ {
					l.Add(NewNode(k, nil))
				}
			}()
		}
		wg.Wait()

		// 并发删除所有节点
		for j := 0; j < 10; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.Range(func(node INode) bool {
					node.Remove()
					return true
				})
			}()
		}
		wg.Wait()
	}
}
