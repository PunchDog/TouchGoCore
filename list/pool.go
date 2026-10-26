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

// releaseNode 把节点归还到节点池。字段复位与归属断开已在 removeNodeLocked 里、
// 持链锁期间完成——这里决不能再写任何节点字段：
//
//   - 内嵌在宿主结构（如 `type Timer struct{ Node }`）里的节点不是池分配的，且可能
//     正被并发的 Add 重挂到另一条链（时间轮跨档迁移）。旧实现在这里复位字段，而
//     另一条链的 Add 用另一把链锁写同一批字段——两把锁互相拦不住，就是数据竞争
//     （实测 golua/localtimer 的 -race 告警：nodeType/delPending 被并发改写）。
//     这类节点的生命周期归宿主自己，这里直接返回。
//   - 池分配的普通节点在入池前字段已被 removeNodeLocked 清空、list 已断开，
//     这里只需翻回 fromPool 标记并放回池里。
func releaseNode(node *Node) {
	if node == nil || !node.fromPool {
		return
	}
	node.fromPool = false
	nodePool.Put(node)
}
