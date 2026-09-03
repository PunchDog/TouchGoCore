package list

import (
	"reflect"
	"touchgocore/vars"
)

// 节点接口
type INode interface {
	GetId() int64
	GetData() interface{}
	InsertAfter(data interface{}) INode
	InsertBefore(data interface{}) INode
	Remove()
	GetNode() *Node
}

// 实现一个双向链表，支持增删改查
// 链表节点
type Node struct {
	id       int64       //节点id
	pre      INode       //上一个节点
	next     INode       //下一个节点
	data     interface{} //数据
	list     *List       //所属链表
	nodeType INode       //节点类型
}

// 获取节点
func (n *Node) GetNode() *Node {
	return n
}

// 获取id
func (n *Node) GetId() int64 {
	return n.id
}

// 获取数据
func (n *Node) GetData() interface{} {
	return n.data
}

func (n *Node) new() INode {
	if n.nodeType == nil {
		return nil
	}
	newNode := reflect.New(reflect.TypeOf(n.nodeType).Elem()).Interface().(INode)
	newnode := newNode.GetNode()
	if newnode == nil {
		return nil
	}
	newnode.nodeType = n.nodeType // 设置相同的类型
	return newNode
}

// 在当前节点后插入一个节点
func (n *Node) InsertAfter(data interface{}) (newNode INode) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("%v", err)
			newNode = nil
		}
	}()
	newNode = n.new()
	if newNode == nil {
		return nil
	}

	n.list.mu.Lock()
	defer n.list.mu.Unlock()

	newnode := newNode.GetNode()
	newnode.id = n.list.generateNextID()
	newnode.pre = n
	newnode.next = n.next
	newnode.data = data
	newnode.list = n.list

	if n.next == nil {
		n.list.tail = newNode
	} else {
		n.next.GetNode().pre = newNode
	}
	n.next = newNode
	n.list.len++
	n.list.nodeMap[newnode.id] = newNode // 添加到 map 索引
	return
}

// 在当前节点前插入一个节点
func (n *Node) InsertBefore(data interface{}) (newNode INode) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("%v", err)
			newNode = nil
		}
	}()
	newNode = n.new()
	if newNode == nil {
		return nil
	}
	n.list.mu.Lock()
	defer n.list.mu.Unlock()

	newnode := newNode.GetNode()
	newnode.id = n.list.generateNextID()
	newnode.pre = n.pre
	newnode.next = n
	newnode.data = data
	newnode.list = n.list

	if n.pre == nil {
		n.list.head = newNode
	} else {
		n.pre.GetNode().next = newNode
	}
	n.pre = newNode
	n.list.len++
	n.list.nodeMap[newnode.id] = newNode // 添加到 map 索引
	return
}

// Remove 删除当前节点并归还对象池
func (n *Node) Remove() {
	n.remove(true)
}

// detach 从链表摘下但不归还对象池，供 Add 复用同一节点
func (n *Node) detach() {
	n.remove(false)
}

func (n *Node) remove(release bool) {
	if n == nil {
		return
	}

	list := n.list
	if list == nil {
		return
	}

	list.mu.Lock()
	defer list.mu.Unlock()

	if n.list != list {
		return
	}

	if release && list.rangeCount.Load() > 0 {
		list.rangeDelList = append(list.rangeDelList, n)
		return
	}

	list.removeNodeLocked(n, release)
}

// 添加一个节点，如果nodeType为nil，则用默认的ListNode创建
func NewNode(data interface{}, nodeType INode) INode {
	var newnode INode

	// 尝试从节点池获取节点对象
	if nodeType == nil {
		// 对于默认类型，尝试从池中获取
		node := acquireNode()
		if node != nil {
			newnode = node
			nodeType = newnode
		} else {
			// 池为空或获取失败，创建新节点
			newnode = new(Node)
			nodeType = newnode
		}
	} else {
		// 对于自定义类型，保持原有反射创建逻辑
		newnode = reflect.New(reflect.TypeOf(nodeType).Elem()).Interface().(INode)
	}

	obj := newnode.GetNode()
	if obj == nil {
		return nil
	}
	obj.data = data
	obj.nodeType = nodeType
	return newnode
}
