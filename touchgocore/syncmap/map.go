package syncmap

import "sync"

type Map struct {
	mp   map[interface{}]interface{} //数据
	num  int                         //数量
	lock sync.Mutex                  //读写锁
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

//数据长点
func (this *Map) Length() int {
	return this.num
}

//读取数据
func (this *Map) Load(k interface{}) (d interface{}, ok bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	return this.load(k)
}

//添加数据
func (this *Map) Store(k, v interface{}) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.store(k, v)
}

//删除数据
func (this *Map) Delete(k interface{}) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.delete(k)
}

//循环
func (this *Map) Range(fn func(k, v interface{}) bool) {
	this.lock.Lock()
	defer this.lock.Unlock()
	for i, i2 := range this.mp {
		if !fn(i, i2) {
			break
		}
	}
}

//添加或读取
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

//读取并操作
func (this *Map) LoadAndFunction(k interface{}, fn func(v interface{}, stfn func(k, v interface{}), delfn func(k interface{}))) {
	this.lock.Lock()
	defer this.lock.Unlock()
	actual, _ := this.load(k)
	fn(actual, this.store, this.delete)
}
