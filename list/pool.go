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
	return nodePool.Get().(*Node)
}

// releaseNode 将节点归还到池中
func releaseNode(node *Node) {
	// 重置节点状态
	node.id = 0
	node.pre = nil
	node.next = nil
	node.data = nil
	node.list = nil
	node.nodeType = nil
	nodePool.Put(node)
}
