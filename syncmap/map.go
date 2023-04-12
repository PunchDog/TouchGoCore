package syncmap

import "sync"

type Map struct {
	mp   map[interface{}]interface{} //数据
	num  int                         //数量
	lock sync.RWMutex                //读写锁
}

func (this *Map) load(k interface{}) (d interface{}, ok bool) {
	if this.mp != nil {
		d, ok = this.mp[k]
		if !ok {
			d = nil
		}
	} else {
		ok = false
		d = nil
	}
	return
}

func (this *Map) store(k, v interface{}) {
	_, ok := this.load(k)
	if !ok {
		if this.mp == nil {
			this.mp = make(map[interface{}]interface{})
		}
		this.mp[k] = v
		this.num++
	}
}

func (this *Map) delete(k interface{}) {
	_, ok := this.load(k)
	if ok {
		delete(this.mp, k)
		this.num--
	}
}

// 数据长点
func (this *Map) Length() int {
	return this.num
}

// 读取数据
func (this *Map) Load(k interface{}) (d interface{}, ok bool) {
	this.lock.RLock()
	defer this.lock.RUnlock()
	return this.load(k)
}

// 添加数据
func (this *Map) Store(k, v interface{}) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.store(k, v)
}

// 删除数据
func (this *Map) Delete(k interface{}) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.delete(k)
}

// 清空所有数据（不可以在fn内有对this的Store或者Delete操作）
func (this *Map) ClearAll(fn func(k, v interface{}) bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	if fn != nil { //清空数据前，先执行操作
		for i, i2 := range this.mp {
			if !fn(i, i2) {
				break
			}
		}
	}
	this.mp = make(map[interface{}]interface{})
	this.num = 0
}

// 循环（不可以在fn内有对this的Store或者Delete操作）
func (this *Map) Range(fn func(k, v interface{}) bool) {
	this.lock.RLock()
	defer this.lock.RUnlock()
	for i, i2 := range this.mp {
		if !fn(i, i2) {
			break
		}
	}
}

// 添加或读取
func (this *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	actual, loaded = this.load(key)
	if !loaded {
		this.store(key, value)
		actual = value
	}
	return
}

// 读取并操作(fn是操作函数,storefn是在这个map中进行插入，key就是LoadAndFunction传入的k,v1是值;delfn是删除LoadAndFunction传入的k)
func (this *Map) LoadAndFunction(k interface{}, fn func(v interface{}, storefn func(v1 interface{}), delfn func())) {
	this.lock.Lock()
	defer this.lock.Unlock()
	actual, ok := this.load(k)
	if !ok {
		actual = nil
	}
	fn(actual, func(v1 interface{}) { this.store(k, v1) }, func() { this.delete(k) })
}
