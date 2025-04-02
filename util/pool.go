package util

import (
	"sync"
	"time"
	"touchgocore/syncmap"
)

const (
	// 单个内存池的容量
	POOL_SIZE = 1000
)

// 用于制造内存池的节点
type PoolNode struct {
	ListNode
	pool *Pool
}

// 处理删除节点时很多需要重新加回去的事情
func (self *PoolNode) Remove() {
	// self.pool.mp.Delete(self.pool.GetId())
	self.ListNode.Remove()
	self.pool.unused.Add(self) //重新加回空列表
	//pool重新放回有空位的内存池
	self.pool.Remove()
	self.pool.mgr.used.Add(self.pool)
}

// 单个内存池
type Pool struct {
	ListNode
	//uid->client
	mp syncmap.Map
	//正在使用的链表
	used *List
	//未使用的链表
	unused *List
	//管理器
	mgr *PoolManager
	//过期时间
	expire int64
	//互斥锁
	mu sync.Mutex
}

func (self *Pool) get(cls INode) INode {
	var node INode = nil
	if self.unused.Length() > 0 {
		node = self.unused.Head()
		node.Remove()
		self.used.Add(node)
	} else {
		node = self.used.AddNew(nil, cls)
		self.mp.Store(node.GetId(), node)
	}
	return node
}

// 管理器
type PoolManager struct {
	Timer
	//有空位的内存池
	used *List
	//没有空位的内存池
	unused *List
}

func (self *PoolManager) Get(cls INode) INode {
	var node INode = nil
	var pool *Pool = nil
	if self.used.Length() > 0 {
		pool = self.used.Head().(*Pool)
		pool.Remove()        //从链表中移除
		node = pool.get(cls) //获取内存池中的内存
	} else {
		pool = self.used.AddNew(nil, &Pool{}).(*Pool)
		pool.used = NewList()
		pool.unused = NewList()
		pool.mgr = self
		node = pool.get(cls)
	}

	if pool.mp.Length() == POOL_SIZE && pool.unused.Length() == 0 {
		//过期时间等于今日0点
		pool.expire = Time2Midnight(time.Now()).UTC().UnixMilli() + MILLISECONDS_OF_DAY
		self.unused.Add(pool)
	} else {
		self.used.Add(pool)
	}
	return node
}

func (self *PoolManager) Tick() {
	//释放过期的内存池
	self.unused.Range(func(i INode) bool {
		pool := i.(*Pool)
		if pool.expire < time.Now().UTC().UnixMilli() {
			pool.Remove()
		}
		return true
	})
}

// 创建内存池管理
func NewPoolMgr() *PoolManager {
	mgr := NewTimer(MILLISECONDS_OF_DAY, -1, &PoolManager{}).(*PoolManager)
	mgr.used = NewList()
	mgr.unused = NewList()
	AddTimer(mgr)
	return mgr
}
