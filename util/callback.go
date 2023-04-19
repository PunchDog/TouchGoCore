package util

import (
	"reflect"
	"sync"
)

const (
	CallStart        string = "StartFunc"
	CallStop         string = "StopFunc"
	CallDispatch     string = "Dispatch"
	CallRegisterRpc  string = "RegisterRpc"
	CallWebSocketMsg string = "WebSocketMsg"
	CallRpcMsg       string = "RpcMsg"
)

var DefaultCallFunc *CallFunction = new(CallFunction)

type CallFunction struct {
	fn      sync.Map        // 数据key/list
	retCh   []reflect.Value //返回值
	bRet    bool            //设置返回值
	retWait sync.WaitGroup  //等待器
}

// 注册回调函数
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

// 需要取返回值的数据，所以这里需要特殊处理
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

// 使用回调函数
func (self *CallFunction) Do(key interface{}, values ...interface{}) (bret bool) {
	defer func() {
		if err := recover(); err != nil {
			bret = false
		}
	}()

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
			method.Type().NumIn()
			//调用
			args1 := []reflect.Value{}
			args1 = append(args1, args...)
			if len(args1) > method.Type().NumIn() { //去掉多余的数据
				args1 = args1[0:method.Type().NumIn()]
			} else if len(args1) < method.Type().NumIn() {
				panic("参数数量小于实际个数")
			}

			//检查数据
			for i := 0; i < method.Type().NumIn(); i++ {
				in := method.Type().In(i)
				if args1[i].Kind() == reflect.Invalid {
					if in.Kind() != reflect.Invalid {
						args1[i] = reflect.New(in)
					} else {
						args1[i] = reflect.ValueOf(&Error{ErrMsg: "错误的无效类型"})
					}
				} else if args1[i].Kind() != in.Kind() {
					// old := args1[i]
					// args1[i] = reflect.New(in)
					// args1[i].Set(old.Interface())
				}
			}
			l := method.Call(args1)
			if self.bRet {
				self.retCh = l
			}
		}
		bret = true
	}
	return
}
