package util

import (
	"reflect"
	"sync"
)

var DefaultCallFunc *CallFunction = new(CallFunction)

type CallFunction struct {
	fn      sync.Map        // 数据key/list
	retCh   []reflect.Value //返回值
	bRet    bool            //设置返回值
	retWait sync.WaitGroup  //等待器
}

//注册回调函数
func (self *CallFunction) Register(key interface{}, fn interface{}) {
	var fnlist []interface{} = nil
	if l, has := self.fn.Load(key); has {
		fnlist = l.([]interface{})
	} else {
		fnlist = make([]interface{}, 0)
	}
	fnlist = append(fnlist, fn)
	self.fn.Store(key, fnlist)
}

//需要取返回值的数据，所以这里需要特殊处理
func (self *CallFunction) SetDoRet() {
	self.retWait.Wait()
	self.retWait.Add(1)
	self.retCh = make([]reflect.Value, 0)
	self.bRet = true
}
func (self *CallFunction) GetRet() []reflect.Value {
	defer self.retWait.Done()
	self.bRet = false
	return self.retCh
}

//使用回调函数
func (self *CallFunction) Do(key interface{}, values ...interface{}) bool {
	if l, has := self.fn.Load(key); has {
		fnlist := l.([]interface{})
		//转化函数参数
		args := []reflect.Value{}
		for _, value := range values {
			args = append(args, reflect.ValueOf(value))
		}
		for _, fn := range fnlist {
			//获取函数
			method := reflect.ValueOf(fn)
			//调用
			l := method.Call(args)
			if self.bRet {
				self.retCh = l
			}
		}
		return true
	}
	return false
}
