package list

import "sync"

// 节点池，用于重用 ListNode 对象，减少 GC 压力
var nodePool = sync.Pool{
	New: func() interface{} {
		return &Node{}
	},
}

// acquireNode 从池中获取节点
func acquireNode() *Node {
	n := nodePool.Get().(*Node)
	n.fromPool = true // 标记来源，只有池分配的节点才允许归还
	return n
}

// releaseNode 将节点归还到池中。
// 注意：内嵌在宿主结构（如 `type Timer struct{ Node }`）里的节点不是池分配的，
// 归还后会被当作独立节点复用，导致宿主对象内存被踩踏，因此这里直接丢弃。
func releaseNode(node *Node) {
	// 重置节点状态
	node.id = 0
	node.pre = nil
	node.next = nil
	node.data = nil
	node.list = nil
	node.nodeType = nil
	node.delPending = false

	if !node.fromPool {
		return
	}
	node.fromPool = false
	nodePool.Put(node)
}
