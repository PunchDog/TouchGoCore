package list

import (
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/vars"
)

// 链表
type List struct {
	mu           sync.Mutex // 保护并发修改
	head         INode      //头节点
	tail         INode      //尾节点
	len          int        //长度
	rangeDelList []INode    //删除列表
	dellock      bool       //删除锁
	nextID       int64      //下一个节点ID
}

// 创建一个链表
func NewList() *List {
	return &List{
		head:   nil,
		tail:   nil,
		len:    0,
		nextID: 0,
		// idMux zero value is usable
	}
}

// generateNextID 生成下一个节点ID，使用原子操作保证并发安全
func (l *List) generateNextID() int64 {
	for {
		old := atomic.LoadInt64(&l.nextID)
		now := time.Now().UnixNano()
		var newID int64
		if old == 0 || old > now+1 {
			newID = now + 1
		} else {
			newID = old + 1
		}
		if atomic.CompareAndSwapInt64(&l.nextID, old, newID) {
			return newID
		}
		// CAS失败，重试
	}
}

// 长度
func (l *List) Length() int {
	return l.len
}

// 插入一个老的节点
func (l *List) Add(node INode) (bret bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("%v", err)
			bret = false
		}
	}()

	if node == nil {
		bret = false
		return
	}
	//删除老的链接
	node.GetNode().Remove()

	l.mu.Lock()
	defer l.mu.Unlock()

	obj := node.GetNode()
	if obj == nil {
		bret = false
		return
	}

	if obj.nodeType == nil {
		obj.nodeType = node
	}

	obj.id = l.generateNextID()
	obj.list = l
	obj.pre = nil
	obj.next = nil

	//添加新的链接
	if l.head == nil {
		l.head = node
		l.tail = node
	} else {
		l.tail.GetNode().next = node
		node.GetNode().pre = l.tail
		l.tail = node
	}
	l.len++
	bret = true
	return
}

// 获取头节点
func (l *List) Head() INode {
	return l.head
}

// 获取尾节点
func (l *List) Tail() INode {
	return l.tail
}

// 获取一个节点
func (l *List) Get(id int64) INode {
	var node INode = nil
	l.Range(func(n INode) bool {
		if n.GetId() == id {
			node = n
			return false
		}
		return true
	})
	return node
}

// 遍历
func (l *List) Range(f func(INode) bool) {
	l.mu.Lock()
	l.dellock = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.dellock = false
		for _, node := range l.rangeDelList {
			if n := node.GetNode(); n != nil {
				l.removeNodeLocked(n)
			}
		}
		l.rangeDelList = nil // 清空引用，防止内存泄漏
		l.mu.Unlock()
	}()

	node := l.head
	for node != nil {
		if condition := f(node); !condition {
			break
		}
		node = node.GetNode().next
	}
}

// 清空
func (l *List) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 遍历所有节点并删除，防止内存泄漏
	node := l.head
	for node != nil {
		next := node.GetNode().next
		// node.Remove()
		l.removeNodeLocked(node.GetNode())
		node = next
	}
	// 确保状态一致
	l.head = nil
	l.tail = nil
	l.len = 0
	l.rangeDelList = nil
}

// removeNodeLocked 从链表中删除节点，调用者必须已持有 mu 锁
func (l *List) removeNodeLocked(node *ListNode) {
	if node.list != l {
		return // 不属于此链表
	}

	// 删除节点
	if node.pre == nil {
		l.head = node.next
	} else {
		node.pre.GetNode().next = node.next
	}
	if node.next == nil {
		l.tail = node.pre
	} else {
		node.next.GetNode().pre = node.pre
	}
	l.len--
	// 清理节点引用
	node.list = nil
	node.pre = nil
	node.next = nil
	node.id = 0
}
