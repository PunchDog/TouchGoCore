package list

import (
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/vars"
)

// 链表
type List struct {
	mu           sync.RWMutex    // 读写锁，提高读操作并发性能
	head         INode           //头节点
	tail         INode           //尾节点
	len          int             //长度
	rangeDelList []INode         //删除列表
	rangeCount   atomic.Int32    //正在遍历的goroutine计数（替代 dellock bool，线程安全）
	nextID       atomic.Int64    //下一个节点ID（使用原子操作）
	nodeMap      map[int64]INode //节点ID映射，支持O(1)查询
}

// 创建一个链表
func NewList() *List {
	return &List{
		head:    nil,
		tail:    nil,
		len:     0,
		nodeMap: make(map[int64]INode),
	}
}

// generateNextID 生成下一个节点ID，使用原子操作保证并发安全
func (l *List) generateNextID() int64 {
	//如果nextID和当前时间相差1秒，就用当前时间作为新的iD,否则+1
	if time.Now().UnixMilli()-l.nextID.Load()/int64(time.Millisecond) >= 1000 {
		l.nextID.Store(time.Now().UnixNano())
	}
	return l.nextID.Add(1)
}

// 长度
func (l *List) Length() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
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
	l.nodeMap[obj.id] = node // 添加到 map 索引
	bret = true
	return
}

// 获取头节点
func (l *List) Head() INode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.head
}

// 获取尾节点
func (l *List) Tail() INode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.tail
}

// 获取一个节点
func (l *List) Get(id int64) INode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.nodeMap[id]
}

// 遍历（优化：使用快照遍历，避免遍历期间持锁导致死锁和竞态条件）
func (l *List) Range(f func(INode) bool) {
	// 标记遍历进行中（原子计数，支持嵌套/并发遍历）
	l.rangeCount.Add(1)
	defer func() {
		// 遍历结束，处理延迟删除列表
		if l.rangeCount.Add(-1) == 0 {
			l.mu.Lock()
			defer l.mu.Unlock()
			for _, node := range l.rangeDelList {
				if n := node.GetNode(); n != nil {
					l.removeNodeLocked(n)
				}
			}
			l.rangeDelList = nil // 清空引用，防止内存泄漏
		}
	}()

	// 获取快照：在锁内复制节点列表，锁外遍历
	l.mu.RLock()
	snapshot := make([]INode, 0, l.len)
	for node := l.head; node != nil; node = node.GetNode().next {
		snapshot = append(snapshot, node)
	}
	l.mu.RUnlock()

	// 在无锁状态下遍历快照，避免死锁和竞态
	for _, node := range snapshot {
		if condition := f(node); !condition {
			break
		}
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
		l.removeNodeLocked(node.GetNode())
		node = next
	}
	// 确保状态一致
	l.head = nil
	l.tail = nil
	l.len = 0
	l.rangeDelList = nil
	l.nodeMap = make(map[int64]INode) // 重建 map，清理所有引用
}

// removeNodeLocked 从链表中删除节点，调用者必须已持有 mu 锁
func (l *List) removeNodeLocked(node *Node) {
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
	delete(l.nodeMap, node.id) // 从 map 索引中删除

	// 清理节点引用并归还到池中
	node.list = nil
	node.pre = nil
	node.next = nil
	node.id = 0
	node.data = nil
	node.nodeType = nil

	// 将节点归还到对象池，只有默认 ListNode 类型才放入池
	// 自定义类型不应放入池中，因为类型不确定
	releaseNode(node)
}
