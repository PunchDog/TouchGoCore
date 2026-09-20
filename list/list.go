package list

import (
	"sync"
	"sync/atomic"
	"touchgocore/util"
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
	nodeMap      map[int64]INode //节点ID映射，支持O(1)查询；indexed=false 时始终为 nil
	// indexed 决定是否维护 ID 索引。取号（CAS + 时钟）与每次 Add/Remove 的
	// map 写删只在需要按 ID 查询时才有意义，时间轮这类只走 Range/Remove 的
	// 热路径纯属白付。构造后不可变，因此读取无需加锁。
	indexed bool
}

// 创建一个链表
func NewList() *List {
	return &List{
		head:    nil,
		tail:    nil,
		len:     0,
		nodeMap: make(map[int64]INode),
		indexed: true,
	}
}

// NewUnindexedList 创建不维护 ID 索引的链表。
//
// 适用于只按顺序遍历/删除、从不按 ID 取节点的场景（时间轮的每一档就是这种）。
// 代价与约束：Get 恒返回 nil，节点的 GetId 恒为 0；需要按 ID 查询请用 NewList。
func NewUnindexedList() *List {
	return &List{}
}

// generateNextID 生成本链表内单调递增的节点 ID。
//
// 旧实现按「距上次取号超过 1 秒就改用当前时间」推进：把纳秒值当毫秒除，
// 且先 Store 再 Add 两步之间没有原子性，两个并发取号能拿到同一个 ID，
// 后写者覆盖 nodeMap 里的既有键 → 前一个节点永久查不到（幽灵条目）。
// 现在用 CAS 推进的「纳秒时钟下限 + 冲突递增」：同纳秒内多次取号也各自唯一。
func (l *List) generateNextID() int64 {
	for {
		cur := l.nextID.Load()
		next := util.CurrentTime().UnixNano()
		if next <= cur {
			next = cur + 1
		}
		if l.nextID.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// assignIDLocked 为入链节点取号并登记到索引，调用者必须已持有 mu 锁。
// 不维护索引的链表直接跳过：节点 id 保持 0，Get 也查不到任何东西。
func (l *List) assignIDLocked(obj *Node, node INode) int64 {
	if !l.indexed {
		return 0
	}
	// 重新入链的节点可能仍挂在旧 ID 上（遍历期间删除请求被挂起，旧键尚未摘除）：
	// 不先摘掉就会在 nodeMap 里永久残留一个指向同一节点的幽灵条目。
	if old := obj.id; old != 0 {
		if prev, ok := l.nodeMap[old]; ok && prev == node {
			delete(l.nodeMap, old)
		}
	}
	id := l.generateNextID()
	if prev, ok := l.nodeMap[id]; ok && prev != node {
		// 取号本身不会撞，撞了说明外部直接改过 id 字段。静默覆盖会让 prev
		// 永久查不到，因此留痕后再取一个号，宁可多一次探测也不能丢索引。
		vars.Error("list 节点 ID 冲突: id=%d, 既有节点将被跳过", id)
		id = l.generateNextID()
	}
	obj.id = id
	l.nodeMap[id] = node
	return id
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
	// 从旧链表摘下但不回池，避免 Add 复用节点时被 sync.Pool 改写
	node.GetNode().detach()

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

	l.assignIDLocked(obj, node)
	obj.list.Store(l)
	obj.pre = nil
	obj.next = nil
	// 节点重新入链：撤销遍历期间挂起的删除请求
	obj.delPending = false

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

// 获取一个节点；不维护索引的链表（NewUnindexedList）恒返回 nil。
func (l *List) Get(id int64) INode {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.nodeMap[id]
}

// rangeBufPool 复用 Range 的快照切片。
// 毫秒时间轮每秒 Range 1000 次，每次 make 新快照会产生大量短命垃圾。
// sync.Pool 在 GC 周期间会被清理，不会长期占住峰值容量。
var rangeBufPool = sync.Pool{
	New: func() any {
		s := make([]INode, 0, 64)
		return &s
	},
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
				n := node.GetNode()
				if n == nil {
					continue
				}
				// 只清理「仍然处于待删除状态」的节点：
				// 遍历期间被重新 Add 回链表的节点其 delPending 已被撤销，必须保留
				if n.delPending {
					l.removeNodeLocked(n, true)
				}
			}
			l.rangeDelList = nil // 清空引用，防止内存泄漏
		}
	}()

	// 获取快照：在锁内复制节点列表，锁外遍历
	l.mu.RLock()
	p := rangeBufPool.Get().(*[]INode)
	snapshot := (*p)[:0]
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

	// 归还前清空引用，否则池里的切片会长期持有已删除的节点。
	// 回调 panic 时跳过归还，buffer 由 GC 回收，不会累积。
	clear(snapshot)
	*p = snapshot
	rangeBufPool.Put(p)
}

// 清空
//
// 遍历进行中（rangeCount>0）时不能把节点归还节点池：并发 Range 已经取了快照，
// 快照里仍持有这些节点指针，而池中刚进去的节点会立刻被下一次 NewNode 发给
// 别人 —— 同一块内存一边被清空复用、一边被遍历读取，链表结构随即错乱。
// 因此这一轮只做摘链不入池，节点由 GC 回收；与 Node.remove 的延迟删除同源，
// 只是 Clear 必须立即交出空表，无法把回收推迟到遍历收尾，代价是少一次复用。
func (l *List) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()

	release := l.rangeCount.Load() == 0

	// 遍历所有节点并删除，防止内存泄漏
	node := l.head
	for node != nil {
		next := node.GetNode().next
		l.removeNodeLocked(node.GetNode(), release)
		node = next
	}
	// 确保状态一致
	l.head = nil
	l.tail = nil
	l.len = 0
	l.rangeDelList = nil
	if l.indexed {
		l.nodeMap = make(map[int64]INode) // 重建 map，清理所有引用
	}
}

// removeNodeLocked 从链表中删除节点，调用者必须已持有 mu 锁。
// release 为 true 时才归还对象池；Add 复用节点时必须传 false。
func (l *List) removeNodeLocked(node *Node, release bool) {
	if node == nil || node.list.Load() != l {
		return
	}

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
	if l.indexed {
		if cur, ok := l.nodeMap[node.id]; ok && cur.GetNode() == node {
			delete(l.nodeMap, node.id)
		}
	}

	node.list.Store(nil)
	node.pre = nil
	node.next = nil
	node.id = 0
	node.delPending = false

	if !release {
		return
	}

	node.data = nil
	node.nodeType = nil
	releaseNode(node)
}
