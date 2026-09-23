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
	// CallWhatsappMsg / CallUstdMsg / CallTonMsg 是三条资金通道的回调前缀。
	// 下游按 <前缀>+<动作> 注册，例如 WhatsappMsg+"Recharge"（载荷 *pay.PayResult）；
	// 通道包本身不落库，结果只经这里交出去。
	CallWhatsappMsg = "WhatsappMsg"
	CallUstdMsg     = "UstdMsg"
	CallTonMsg      = "TonMsg"
	// CallPaySDKMsg + pay_sdks 的段名 = 该段签名密钥的注入钩子，载荷是 *string
	// （指向该段的 secret_key 字段本身）。
	//
	// 钩子按 SDK 段而不是按通道命名：同一供应商的凭证只有一份，常被两三条通道共用。
	// 若让下游为 whatsapp/ustd/ton 各注册一次同一个密钥，迟早出现「只注入了两家、
	// 第三家用空密钥出款」这种只有供应商会发现的错。
	CallPaySDKMsg = "PaySDK"
	CallLoadIni   = "loadini"
)

var DefaultCallFunc = &CallFunction{
	fn: syncmap.NewAny(),
}

type CallFunction struct {
	fn *syncmap.MapAny // key -> []callbackEntry 映射
	// writeMu 串行化 Register/Unregister 的读-改-写，防止并发注册丢更新；
	// Do 路径仍走 syncmap 无锁读（copy-on-write 保证快照一致）。
	writeMu sync.Mutex
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

// 使用回调函数，只表达「回调组是否全部正常执行」，不收集返回值。
//
// 需要返回值一律用 DoWithRet / DoWithRetCtx：它们把结果放在调用栈上，
// 并发触发同一 key 时不会互相串结果。
func (c *CallFunction) Do(key any, values ...any) (ok bool) {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("调用回调函数失败: %v", err)
			ok = false
		}
	}()

	if l, has := c.fn.Load(key); has {
		// 只要有一个回调崩掉就不能算成功，否则调用方按「无人处理」的分支误报
		ok = true
		entries := l.([]callbackEntry)
		for _, e := range entries {
			args, err := callFunctionArgs(e.fn, values...)
			if err != nil {
				vars.Debug("参数转换失败: %v", err)
				continue
			}
			if _, panicked := callOneCallback(e.fn, args); panicked != nil {
				vars.Error("调用回调函数失败 key=%v: %v", key, panicked)
				ok = false
			}
		}
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
