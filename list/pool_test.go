package list

import (
	"reflect"
	"testing"
)

// TestPoolIntegration 测试对象池集成
func TestPoolIntegration(t *testing.T) {
	// 创建并删除多个节点，验证对象池是否工作
	for i := 0; i < 100; i++ {
		l := NewList()
		nodes := make([]INode, 10)

		// 添加节点
		for j := 0; j < 10; j++ {
			nodes[j] = NewNode(j, nil)
			if !l.Add(nodes[j]) {
				t.Errorf("failed to add node %d", j)
			}
		}

		// 验证节点被添加
		if l.Length() != 10 {
			t.Errorf("expected length 10, got %d", l.Length())
		}

		// 删除节点
		for j := 0; j < 10; j++ {
			nodes[j].Remove()
		}

		// 验证链表为空
		if l.Length() != 0 {
			t.Errorf("expected length 0 after removal, got %d", l.Length())
		}
	}

	// 多次执行后，对象池应该已经缓存了一些节点
	// 再次创建节点应该能够复用池中的对象
	l := NewList()
	for i := 0; i < 1000; i++ {
		node := NewNode(i, nil)
		l.Add(node)
	}

	if l.Length() != 1000 {
		t.Errorf("expected length 1000, got %d", l.Length())
	}
}

// TestPoolWithCustomType 测试对象池与自定义类型
func TestPoolWithCustomType(t *testing.T) {
	// 定义自定义类型
	type CustomNode struct {
		ListNode
		customData string
	}

	// 创建自定义类型节点
	baseNode := &CustomNode{}
	l := NewList()

	// 使用反射创建自定义节点
	for i := 0; i < 10; i++ {
		newNode := reflect.New(reflect.TypeOf(baseNode).Elem()).Interface().(INode)
		obj := newNode.GetNode()
		obj.data = i
		l.Add(newNode)
	}

	if l.Length() != 10 {
		t.Errorf("expected length 10, got %d", l.Length())
	}

	// 验证 Get 操作
	l.Range(func(node INode) bool {
		if node.GetData() == nil {
			t.Error("node data should not be nil")
		}
		return true
	})
}
