package syncmap

import "sync"

type Map struct {
	sync.Map
	num  int          //数量
	lock sync.RWMutex //读写锁
}

// 数据长点
func (this *Map) Length() int {
	this.lock.RLock()
	defer this.lock.RUnlock()
	return this.num
}

// 添加数据
func (this *Map) Store(k, v interface{}) {
	this.LoadOrStore(k, v)
}

// 删除数据
func (this *Map) Delete(k interface{}) {
	this.lock.Lock()
	defer this.lock.Unlock()
	if _, h := this.Load(k); h {
		this.Map.Delete(k)
		this.num--
	}
}

// 清空所有数据（不可以在fn内有对this的Store或者Delete操作）
func (this *Map) ClearAll(fn func(k, v interface{}) bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	if fn != nil {
		this.Map.Range(func(k, v interface{}) bool {
			return fn(k, v)
		})
	}
	this.num = 0
	this.Map = sync.Map{}
}

// 添加或读取
func (this *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	actual, loaded = this.Map.LoadOrStore(key, value)
	if !loaded {
		this.num++
	}
	return
}
