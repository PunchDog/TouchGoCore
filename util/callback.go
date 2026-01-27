package util

import (
	"log"
	"reflect"
	"sync"
	"touchgocore/vars"
)

const (
	CallStart        string = "StartFunc" //项目启动时加载数据
	CallStop         string = "StopFunc"  //关闭项目时执行保存之类的
	CallDispatch     string = "Dispatch"
	CallWebSocketMsg string = "WebSocketMsg"
	CallRpcMsg       string = "RpcMsg"
	CallTelegramMsg  string = "TelegramMsg"
)

var DefaultCallFunc *CallFunction = new(CallFunction)

// callEntry 存储每个键对应的回调函数列表及其锁
type callEntry struct {
	mu   sync.RWMutex
	fns  []reflect.Value
	meta []*funcMeta // 缓存函数元数据以提高性能
}

// funcMeta 缓存函数的反射元数据
type funcMeta struct {
	inCount int            // 参数个数
	inTypes []reflect.Type // 参数类型
}

// CallFunction 是回调管理器
type CallFunction struct {
	entries sync.Map // key -> *callEntry
}

// Register 注册回调函数
func (self *CallFunction) Register(key any, fn any) {
	val := reflect.ValueOf(fn)
	if val.Kind() != reflect.Func {
		panic("Register: fn must be a function")
	}

	// 获取或创建 callEntry
	entry, _ := self.entries.LoadOrStore(key, &callEntry{})
	ce := entry.(*callEntry)

	ce.mu.Lock()
	defer ce.mu.Unlock()

	// 缓存函数元数据
	meta := &funcMeta{
		inCount: val.Type().NumIn(),
		inTypes: make([]reflect.Type, val.Type().NumIn()),
	}
	for i := 0; i < meta.inCount; i++ {
		meta.inTypes[i] = val.Type().In(i)
	}

	ce.fns = append(ce.fns, val)
	ce.meta = append(ce.meta, meta)
}



// Do 执行回调函数
func (self *CallFunction) Do(key any, values ...any) (result []reflect.Value, ok bool) {
	// 恢复可能的 panic
	defer func() {
		if err := recover(); err != nil {
			log.Printf("callback.Do panic: key=%v, error=%v", key, err)
			result = nil
			ok = false
		}
	}()

	entry, loaded := self.entries.Load(key)
	if !loaded {
		return nil, false
	}

	ce := entry.(*callEntry)
	ce.mu.RLock()
	fns := ce.fns
	metas := ce.meta
	ce.mu.RUnlock()

	if len(fns) == 0 {
		return nil, false
	}

	// 准备参数值
	args := make([]reflect.Value, len(values))
	for i, v := range values {
		args[i] = reflect.ValueOf(v)
	}

	// 执行每个回调函数
	var lastResult []reflect.Value
	for i, fn := range fns {
		meta := metas[i]

		// 检查参数数量
		if len(args) < meta.inCount {
			log.Printf("callback.Do: insufficient arguments for key=%v, need %d got %d",
				key, meta.inCount, len(args))
			continue
		}

		// 截取所需数量的参数
		callArgs := args
		if len(callArgs) > meta.inCount {
			callArgs = callArgs[:meta.inCount]
		}

		// 尝试转换参数类型
		convertedArgs := make([]reflect.Value, meta.inCount)
		for j := 0; j < meta.inCount; j++ {
			arg := callArgs[j]
			targetType := meta.inTypes[j]

			// 如果类型可赋值，直接使用
			if arg.IsValid() && arg.Type().AssignableTo(targetType) {
				convertedArgs[j] = arg
				continue
			}

			// 尝试转换
			if arg.IsValid() && arg.Type().ConvertibleTo(targetType) {
				convertedArgs[j] = arg.Convert(targetType)
				continue
			}

			// 无法转换，创建零值
			vars.Info("callback.Do: argument type mismatch for key=%v, param %d: got %v, need %v",
				key, j, arg.Type(), targetType)
			convertedArgs[j] = reflect.Zero(targetType)
		}

		// 调用函数
		results := fn.Call(convertedArgs)
		lastResult = results
	}

	if lastResult != nil {
		return lastResult, true
	}
	return nil, false
}
