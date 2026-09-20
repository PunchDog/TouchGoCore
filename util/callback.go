package util

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

// callbackEntry 一次注册的回调：ID 用于精确注销（func 值不可比较，DeepEqual 对闭包恒 false）
type callbackEntry struct {
	id uint64
	fn any
}

var callbackID atomic.Uint64

// 注册回调函数，返回可用于注销的 ID
func (c *CallFunction) Register(key any, fn any) uint64 {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	id := callbackID.Add(1)
	var entries []callbackEntry
	if l, has := c.fn.Load(key); has {
		old := l.([]callbackEntry)
		entries = make([]callbackEntry, len(old), len(old)+1)
		copy(entries, old)
	}
	entries = append(entries, callbackEntry{id: id, fn: fn})
	c.fn.Store(key, entries)
	return id
}

// 取消注册回调函数：idOrFn 可传 Register 返回的 ID(uint64)，也可传函数值本身（按代码地址匹配，删除最先命中的一条）
func (c *CallFunction) Unregister(key any, idOrFn any) bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	l, has := c.fn.Load(key)
	if !has {
		return false
	}
	entries := l.([]callbackEntry)
	out := make([]callbackEntry, 0, len(entries))
	removed := false
	switch arg := idOrFn.(type) {
	case uint64:
		for _, e := range entries {
			if !removed && e.id == arg {
				removed = true
				continue
			}
			out = append(out, e)
		}
	default:
		v := reflect.ValueOf(idOrFn)
		if v.Kind() != reflect.Func {
			return false
		}
		target := v.Pointer()
		for _, e := range entries {
			if !removed && reflect.ValueOf(e.fn).Pointer() == target {
				removed = true
				continue
			}
			out = append(out, e)
		}
	}
	if !removed {
		return false
	}
	if len(out) == 0 {
		c.fn.Delete(key)
	} else {
		c.fn.Store(key, out)
	}
	return true
}

// callOneCallback 执行单个回调并隔离 panic，返回 (返回值, panic值)
func callOneCallback(fn any, args []reflect.Value) (rets []reflect.Value, panicked any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = r
			rets = nil
		}
	}()
	return reflect.ValueOf(fn).Call(args), nil
}

const (
	CallStart        = "StartFunc" //项目启动时加载数据
	CallStop         = "StopFunc"  //关闭项目时执行保存之类的
	CallDispatch     = "Dispatch"
	CallWebSocketMsg = "WebSocketMsg"
	CallRpcMsg       = "RpcMsg"
	CallTelegramMsg  = "TelegramMsg"
	CallLoadIni      = "loadini"
)

var DefaultCallFunc = &CallFunction{
	fn: syncmap.NewAny(),
}

type CallFunction struct {
	fn *syncmap.MapAny // key -> []callbackEntry 映射
	// writeMu 串行化 Register/Unregister 的读-改-写，防止并发注册丢更新；
	// Do 路径仍走 syncmap 无锁读（copy-on-write 保证快照一致）。
	writeMu sync.Mutex
	// Deprecated: retCh/retMu/bRet 存在竞态条件，请使用 DoWithRet 替代
	retCh []reflect.Value // 返回值收集（已废弃，保留向后兼容）
	retMu sync.Mutex      // 返回值保护锁（已废弃）
	bRet  atomic.Bool     // 是否收集返回值（已废弃）
}

// SetDoRet 标记后续 Do 调用需要收集返回值
//
// Deprecated: 存在竞态条件，并发调用 Do 时返回值可能错乱。
// 请使用 DoWithRet 替代，DoWithRet 是线程安全的。
func (c *CallFunction) SetDoRet() {
	c.retMu.Lock()
	c.retCh = make([]reflect.Value, 0, 16)
	c.retMu.Unlock()
	c.bRet.Store(true)
}

// GetRet 获取收集到的返回值
//
// Deprecated: 存在竞态条件，请使用 DoWithRet 替代。
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
//
// 注意：当 bRet 为 true 时，Do 会将返回值写入共享的 retCh，
// 这在并发场景下不安全。推荐使用 DoWithRet 获取返回值。
func (c *CallFunction) Do(key any, values ...any) (ok bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("调用回调函数失败: %v", err)
			ok = false
		}
	}()

	if l, has := c.fn.Load(key); has {
		entries := l.([]callbackEntry)
		for _, e := range entries {
			args, err := callFunctionArgs(e.fn, values...)
			if err != nil {
				vars.Debug("参数转换失败: %v", err)
				continue
			}
			ret, panicked := callOneCallback(e.fn, args)
			if panicked != nil {
				vars.Error("调用回调函数失败 key=%v: %v", key, panicked)
				ok = false
				continue
			}
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
			vars.Error("调用回调函数失败: %v", err)
		}
	}()

	var results []reflect.Value

	if l, has := c.fn.Load(key); has {
		entries := l.([]callbackEntry)
		for _, e := range entries {
			args, err := callFunctionArgs(e.fn, values...)
			if err != nil {
				vars.Debug("参数转换失败: %v", err)
				continue
			}
			ret, panicked := callOneCallback(e.fn, args)
			if panicked != nil {
				vars.Error("调用回调函数失败 key=%v: %v", key, panicked)
				continue
			}
			results = append(results, ret...)
		}
		return results, true
	}

	return nil, false
}

func injectContext(ctx context.Context, fn interface{}, values []any) []any {
	if ctx == nil {
		return values
	}
	ft := reflect.TypeOf(fn)
	if ft == nil || ft.Kind() != reflect.Func || ft.NumIn() == 0 {
		return values
	}
	if ft.In(0) == reflect.TypeOf((*context.Context)(nil)).Elem() {
		out := make([]any, 0, len(values)+1)
		out = append(out, ctx)
		out = append(out, values...)
		return out
	}
	return values
}

// DoWithRetCtx 在 ctx 取消时立即返回；若回调首参为 context.Context 则自动注入。
func (c *CallFunction) DoWithRetCtx(ctx context.Context, key any, values ...any) ([]reflect.Value, bool) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
	}

	defer func() {
		if err := recover(); err != nil {
			vars.Error("调用回调函数失败: %v", err)
		}
	}()

	var results []reflect.Value
	if l, has := c.fn.Load(key); has {
		entries := l.([]callbackEntry)
		for _, e := range entries {
			if ctx != nil {
				select {
				case <-ctx.Done():
					return results, len(results) > 0
				default:
				}
			}
			args, err := callFunctionArgs(e.fn, injectContext(ctx, e.fn, values)...)
			if err != nil {
				vars.Debug("参数转换失败: %v", err)
				continue
			}
			ret, panicked := callOneCallback(e.fn, args)
			if panicked != nil {
				vars.Error("调用回调函数失败 key=%v: %v", key, panicked)
				continue
			}
			results = append(results, ret...)
		}
		return results, true
	}
	return nil, false
}
