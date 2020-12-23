package syncmap

import "sync"

type Map struct {
	sync.Map
	num int //数量
}

func (this *Map) Length() int {
	return this.num
}

//添加数据
func (this *Map) Store(k, v interface{}) {
	_, ok := this.Load(k)
	this.Map.Store(k, v)
	if !ok {
		this.num++
	}
}

//删除数据
func (this *Map) Delete(k interface{}) {
	_, ok := this.Load(k)
	this.Map.Delete(k)
	if ok {
		this.num--
	}
}

//添加或读取
func (this *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	if actual, loaded = this.Map.LoadOrStore(key, value); !loaded {
		this.num++
	}
	return
}
