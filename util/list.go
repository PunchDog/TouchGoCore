package util

import (
	"reflect"
	"sync"
	"time"
	"touchgocore/vars"
)

// 节点接口
type INode interface {
	GetId() int64
	GetData() interface{}
	InsertAfter(data interface{}) INode
	InsertBefore(data interface{}) INode
	Remove()
	GetNode() *ListNode
}

// 实现一个双向链表，支持增删改查
// 链表节点
type ListNode struct {
	id   int64       //节点id
	pre  INode       //上一个节点
	next INode       //下一个节点
	data interface{} //数据
	list *List       //所属链表
	cls  INode       //节点类型
}

// 获取节点
func (this *ListNode) GetNode() *ListNode {
	return this
}

// 获取id
func (this *ListNode) GetId() int64 {
	return this.id
}

// 获取数据
func (this *ListNode) GetData() interface{} {
	return this.data
}

func (this *ListNode) new() INode {
	if this.cls == nil {
		return nil
	}
	newNode := reflect.New(reflect.TypeOf(this.cls).Elem()).Interface().(INode)
	newnode := newNode.GetNode()
	if newnode == nil {
		return nil
	}
	newnode.cls = this.cls // 设置相同的类型
	return newNode
}

// 在当前节点后插入一个节点
func (this *ListNode) InsertAfter(data interface{}) (newNode INode) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("", err)
			newNode = nil
		}
	}()
	newNode = this.new()
	if newNode == nil {
		return nil
	}
	newnode := newNode.GetNode()
	newnode.id = this.list.getMaxId()
	newnode.pre = this
	newnode.next = this.next
	newnode.data = data
	newnode.list = this.list

	if this.next == nil {
		this.list.tail = newNode
	} else {
		this.next.GetNode().pre = newNode
	}
	this.next = newNode
	this.list.len++
	return
}

// 在当前节点前插入一个节点
func (this *ListNode) InsertBefore(data interface{}) (newNode INode) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("", err)
			newNode = nil
		}
	}()
	newNode = this.new()
	if newNode == nil {
		return nil
	}
	newnode := newNode.GetNode()
	newnode.id = this.list.getMaxId()
	newnode.pre = this.pre
	newnode.next = this
	newnode.data = data
	newnode.list = this.list

	if this.pre == nil {
		this.list.head = newNode
	} else {
		this.pre.GetNode().next = newNode
	}
	this.pre = newNode
	this.list.len++
	return
}

// 删除当前节点
func (this *ListNode) Remove() {
	if this.list == nil {
		return
	}

	//如果是链表遍历期间，需要删除的节点先缓存下来，等遍历结束后再删除
	if this.list.dellock {
		this.list.rangeDelList = append(this.list.rangeDelList, this)
		return
	}

	//删除节点
	if this.pre == nil {
		this.list.head = this.next
	} else {
		this.pre.GetNode().next = this.next
	}
	if this.next == nil {
		this.list.tail = this.pre
	} else {
		this.next.GetNode().pre = this.pre
	}
	this.list.len--
	this.list = nil
	this.pre = nil
	this.next = nil
	// this.data = nil
	// this.cls = nil
	this.id = 0
	return
}

// 链表
type List struct {
	head         INode   //头节点
	tail         INode   //尾节点
	len          int     //长度
	rangeDelList []INode //删除列表
	dellock      bool    //删除锁
	nextID       int64   //下一个节点ID
	idMux        sync.Mutex //ID生成锁
}

// 创建一个链表
func NewList() *List {
	return &List{
		head: nil,
		tail: nil,
		len:  0,
		nextID: 0,
		// idMux zero value is usable
	}
}



// 获取一个ID，并且nextID++
func (this *List) getMaxId() int64 {
	this.idMux.Lock()
	defer this.idMux.Unlock()

	if this.nextID == 0 || this.nextID > time.Now().UnixNano()+1 {
		this.nextID = time.Now().UnixNano() + 1
	} else {
		this.nextID++
	}
	return this.nextID
}

// 长度
func (this *List) Length() int {
	return this.len
}

// 添加一个节点，如果cls为nil，则用默认的ListNode创建
func (this *List) AddNew(data interface{}, cls INode) INode {
	var newnode INode
	if cls == nil {
		newnode = new(ListNode)
		cls = newnode
	} else {
		newnode = reflect.New(reflect.TypeOf(cls).Elem()).Interface().(INode)
	}

	obj := newnode.GetNode()
	if obj == nil {
		return nil
	}
	obj.data = data
	obj.cls = cls

	if this.Add(newnode) {
		return newnode
	}
	return nil
}

// 插入一个老的节点
func (this *List) Add(node INode) (bret bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("", err)
			bret = false
		}
	}()

	if node == nil {
		bret = false
		return
	}
	//删除老的链接
	node.GetNode().Remove()

	obj := node.GetNode()
	if obj == nil {
		bret = false
		return
	}

	if obj.cls == nil {
		obj.cls = node
	}

	obj.id = this.getMaxId()
	obj.list = this
	obj.pre = nil
	obj.next = nil

	//添加新的链接
	if this.head == nil {
		this.head = node
		this.tail = node
	} else {
		this.tail.GetNode().next = node
		node.GetNode().pre = this.tail
		this.tail = node
	}
	this.len++
	bret = true
	return
}

// 获取头节点
func (this *List) Head() INode {
	return this.head
}

// 获取尾节点
func (this *List) Tail() INode {
	return this.tail
}

// 获取一个节点
func (this *List) Get(id int64) INode {
	var node INode = nil
	this.Range(func(n INode) bool {
		if n.GetId() == id {
			node = n
			return false
		}
		return true
	})
	return node
}

// 遍历
func (this *List) Range(f func(INode) bool) {
	this.dellock = true
	defer func() {
		this.dellock = false
		for _, node := range this.rangeDelList {
			node.Remove()
		}
		this.rangeDelList = nil // 清空引用，防止内存泄漏
	}()

	node := this.head
	for node != nil {
		if condition := f(node); !condition {
			break
		}
		node = node.GetNode().next
	}
}

// 清空
func (this *List) Clear() {
	// 遍历所有节点并删除，防止内存泄漏
	node := this.head
	for node != nil {
		next := node.GetNode().next
		node.Remove()
		node = next
	}
	// 确保状态一致
	this.head = nil
	this.tail = nil
	this.len = 0
	this.rangeDelList = nil
}
