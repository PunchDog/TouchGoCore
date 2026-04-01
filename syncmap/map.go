package syncmap

import (
	"sync"
	"sync/atomic"
)

type Map struct {
	mp  map[any]any
	num atomic.Int32 //数量
	mu  sync.RWMutex
}

// 数据长点
func (this *Map) Length() int {
	return int(this.num.Load())
}

// 添加数据
func (this *Map) Store(k, v any) {
	this.mu.Lock()
	defer this.mu.Unlock()
	if this.mp == nil {
		this.mp = make(map[any]any)
	}
	if _, h := this.mp[k]; !h {
		this.num.Add(1)
	}
	this.mp[k] = v
}

// 删除数据
func (this *Map) Delete(k any) {
	this.mu.Lock()
	defer this.mu.Unlock()
	if _, h := this.mp[k]; h {
		delete(this.mp, k)
		this.num.Add(-1)
	}
}

// 清空所有数据（不可以在fn内有对this的Store或者Delete操作）
func (this *Map) ClearAll(fn func(k, v any) bool) {
	this.mu.Lock()
	defer this.mu.Unlock()
	for k, v := range this.mp {
		if !fn(k, v) {
			break
		}
	}
	this.mp = make(map[any]any)
	this.num.Store(0)
}

// 添加或读取
func (this *Map) LoadOrStore(key, value any) (actual any, loaded bool) {
	this.mu.Lock()
	defer this.mu.Unlock()
	if this.mp == nil {
		this.mp = make(map[any]any)
	}

	actual, loaded = this.mp[key]
	if !loaded {
		this.mp[key] = value
		this.num.Add(1)
	}
	return
}

// 读取
func (this *Map) Load(k any) (v any, ok bool) {
	this.mu.RLock()
	defer this.mu.RUnlock()
	v, ok = this.mp[k]
	return
}

// 循环
func (this *Map) Range(fn func(k, v any) bool) {
	this.mu.RLock()
	defer this.mu.RUnlock()
	for k, v := range this.mp {
		if !fn(k, v) {
			break
		}
	}
}
