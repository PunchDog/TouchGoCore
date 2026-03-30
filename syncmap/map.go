package syncmap

import (
	"sync"
	"sync/atomic"
)

type Map struct {
	sync.Map
	num int32 //数量
}

// 数据长点
func (this *Map) Length() int {
	return int(atomic.LoadInt32(&this.num))
}

// 添加数据
func (this *Map) Store(k, v interface{}) {
	if _, h := this.Load(k); !h {
		atomic.AddInt32(&this.num, 1)
	}
	this.Map.Store(k, v)
}

// 删除数据
func (this *Map) Delete(k interface{}) {
	if _, h := this.Load(k); h {
		this.Map.Delete(k)
		atomic.AddInt32(&this.num, -1)
	}
}

// 清空所有数据（不可以在fn内有对this的Store或者Delete操作）
func (this *Map) ClearAll(fn func(k, v interface{}) bool) {
	if fn != nil {
		this.Map.Range(func(k, v interface{}) bool {
			return fn(k, v)
		})
	}
	atomic.StoreInt32(&this.num, 0)
	this.Map = sync.Map{}
}

// 添加或读取
func (this *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	actual, loaded = this.Map.LoadOrStore(key, value)
	if !loaded {
		atomic.AddInt32(&this.num, 1)
	}
	return
}
