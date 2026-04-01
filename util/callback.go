package util

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

// 注册回调函数
func (c *CallFunction) Register(key any, fn any) {
	var funcs []any
	if l, has := c.fn.Load(key); has {
		funcs = l.([]any)
	}
	funcs = append(funcs, fn)
	c.fn.Store(key, funcs)
}

// 取消注册回调函数
func (c *CallFunction) Unregister(key any, fn any) bool {
	if l, has := c.fn.Load(key); has {
		funcs := l.([]any)
		for i, f := range funcs {
			if reflect.DeepEqual(f, fn) {
				newFuncs := append(funcs[:i], funcs[i+1:]...)
				if len(newFuncs) == 0 {
					c.fn.Delete(key)
				} else {
					c.fn.Store(key, newFuncs)
				}
				return true
			}
		}
	}
	return false
}

const (
	CallStart        = "StartFunc" //项目启动时加载数据
	CallStop         = "StopFunc"  //关闭项目时执行保存之类的
	CallDispatch     = "Dispatch"
	CallWebSocketMsg = "WebSocketMsg"
	CallRpcMsg       = "RpcMsg"
	CallTelegramMsg  = "TelegramMsg"
)

var DefaultCallFunc = &CallFunction{
	fn: syncmap.NewAny(),
}

type CallFunction struct {
	fn    *syncmap.MapAny // key -> 函数列表映射
	retCh []reflect.Value // 返回值收集
	retMu sync.Mutex      // 返回值保护锁
	bRet  atomic.Bool     // 是否收集返回值（使用原子操作）
}

// 需要取返回值的数据，所以这里需要特殊处理
func (c *CallFunction) SetDoRet() {
	c.retMu.Lock()
	c.retCh = make([]reflect.Value, 0, 16) // 预分配空间
	c.retMu.Unlock()
	c.bRet.Store(true)
}
func (c *CallFunction) GetRet() []reflect.Value {
	c.retMu.Lock()
	defer c.retMu.Unlock()
	c.bRet.Store(false)
	return c.retCh
}

// convertArg 将 argVal 转换为目标类型 targetType。
// 支持直接赋值、reflect.Convert，以及「传入值本身是 interface/pointer，
// 底层值可转换到目标类型」的情形（常见于通过 interface{} 传递命名整型）。
func convertArg(argVal reflect.Value, targetType reflect.Type) (reflect.Value, error) {
	// 解引用 interface 包装
	for argVal.Kind() == reflect.Interface {
		argVal = argVal.Elem()
	}
	if argVal.Type().AssignableTo(targetType) {
		return argVal, nil
	}
	if argVal.Type().ConvertibleTo(targetType) {
		return argVal.Convert(targetType), nil
	}
	return reflect.Value{}, fmt.Errorf("type mismatch: need %s, got %s", targetType, argVal.Type())
}

// callFunctionArgs 将 args 转换为适合通过 method.Call(callArgs) 调用 fn 的参数列表。
//
// 对于 variadic 函数（如 func f(a T, rest ...U)），可变参数部分被逐个转换后
// 追加到 callArgs 末尾，由 reflect.Value.Call 自动打包——这是唯一正确的方式。
// 若使用 method.Call 并把可变参数打包成 reflect.Value([]U)，运行时会 panic。
func callFunctionArgs(fn interface{}, args ...interface{}) ([]reflect.Value, error) {
	f := reflect.ValueOf(fn)
	if f.Kind() != reflect.Func {
		return nil, fmt.Errorf("provided value is not a function")
	}

	ft := f.Type()
	numIn := ft.NumIn()
	isVariadic := ft.IsVariadic()

	// 固定参数数量（variadic 时最后一个形参是 []T，不算在固定参数里）
	fixedArgCount := numIn
	if isVariadic {
		fixedArgCount = numIn - 1
	}

	// 参数数量检查
	if len(args) < fixedArgCount {
		return nil, fmt.Errorf("insufficient arguments: need at least %d, got %d", fixedArgCount, len(args))
	}
	if !isVariadic && len(args) > fixedArgCount {
		return nil, fmt.Errorf("too many arguments: need %d, got %d", fixedArgCount, len(args))
	}

	// 预分配：固定参数 + 可变参数展开后的数量
	callArgs := make([]reflect.Value, 0, len(args))

	// 处理固定参数
	for i := 0; i < fixedArgCount; i++ {
		argVal := reflect.ValueOf(args[i])
		converted, err := convertArg(argVal, ft.In(i))
		if err != nil {
			return nil, fmt.Errorf("argument %d: %w", i, err)
		}
		callArgs = append(callArgs, converted)
	}

	// 处理可变参数：逐个转换后直接追加，让 reflect.Value.Call 负责打包
	// 注意：不能把它们打包成 slice 再作为单个 reflect.Value 传入，
	// 那样 method.Call 会把整个 slice 当成第一个可变元素，导致类型不匹配 panic。
	if isVariadic {
		variadicElemType := ft.In(fixedArgCount).Elem() // []T 的元素类型 T
		for i, arg := range args[fixedArgCount:] {
			argVal := reflect.ValueOf(arg)
			converted, err := convertArg(argVal, variadicElemType)
			if err != nil {
				return nil, fmt.Errorf("variadic argument %d: %w", i, err)
			}
			callArgs = append(callArgs, converted)
		}
	}

	return callArgs, nil
}

// 使用回调函数
func (c *CallFunction) Do(key any, values ...any) (ok bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("调用回调函数失败", "key", key, "err", err)
			ok = false
		}
	}()

	if l, has := c.fn.Load(key); has {
		funcs := l.([]any)
		for _, fn := range funcs {
			args, err := callFunctionArgs(fn, values...)
			if err != nil {
				vars.Debug("参数转换失败", "函数", fn, "参数", values, "error", err)
				continue
			}
			method := reflect.ValueOf(fn)
			ret := method.Call(args)
			if c.bRet.Load() {
				c.retMu.Lock()
				c.retCh = append(c.retCh, ret...)
				c.retMu.Unlock()
			}
		}
		ok = true
	}
	return
}

// DoWithRet 执行回调并直接返回所有返回值，避免使用全局状态
// 这是线程安全的替代方案，推荐在需要返回值的场景使用
func (c *CallFunction) DoWithRet(key any, values ...any) ([]reflect.Value, bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("调用回调函数失败", "key", key, "err", err)
		}
	}()

	var results []reflect.Value

	if l, has := c.fn.Load(key); has {
		funcs := l.([]any)
		for _, fn := range funcs {
			args, err := callFunctionArgs(fn, values...)
			if err != nil {
				vars.Debug("参数转换失败", "函数", fn, "参数", values, "error", err)
				continue
			}
			method := reflect.ValueOf(fn)
			ret := method.Call(args)
			results = append(results, ret...)
		}
		return results, true
	}

	return nil, false
}
