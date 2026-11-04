package lua

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	rt "github.com/arnodel/golua/runtime"
)

// ==================== 反射式宿主函数注册 ====================
//
// RegisterLuaFunc 要求调用方手写 func(*rt.Thread, *rt.GoCont) 并自己从 Etc() 取参，
// 还得记住「1 个具名槽 + etc」这条约定。RegisterGoFunc 让普通 Go 函数一行注册：
// 形参与返回值按反射签名自动编解码，元数取真实值（不再被迫收成 1 槽 + etc）。

var (
	registeredGoFuncs   map[string]*goFuncBinding
	registeredGoFuncsMu sync.RWMutex
)

// errorType 是末位 error 返回值的类型，单独处理（非 nil 时变成 Lua 运行时错误）。
var errorType = reflect.TypeOf((*error)(nil)).Elem()

// goFuncBinding 是一个已注册 Go 函数的全部调用期信息，注册时算好。
type goFuncBinding struct {
	name string
	fn   reflect.Value
	// fixed 是定长段形参类型；变参函数最后那个切片单独记在 varElem
	fixed   []reflect.Type
	varElem reflect.Type
	// out 是返回值类型，不含末位的 error
	out     []reflect.Type
	lastErr bool
}

// nArgs 返回绑定的具名槽数（= 定长段形参个数，不含接收者这种概念）。
func (b *goFuncBinding) nArgs() int { return len(b.fixed) }

// RegisterGoFunc 用反射把任意 Go 函数注册为 Lua 全局函数，对所有后续创建的脚本实例生效。
//
// fn 必须是函数，形参与返回值在支持集合内（标量、string、bool、*LuaTable、
// map/slice/array、struct 与 struct 指针、any、末位 error）。名字与 RegisterLuaFunc
// 共用同一命名空间，也占用不了内置函数名。
//
// 与 RegisterLuaFunc 一样，注册只影响之后创建的实例，不回填已存在的实例。
func RegisterGoFunc(name string, fn interface{}) error {
	binding, err := newGoFuncBinding(name, fn)
	if err != nil {
		return err
	}
	if err := checkGoFuncNameFree(name); err != nil {
		return err
	}

	registeredGoFuncsMu.Lock()
	defer registeredGoFuncsMu.Unlock()
	if registeredGoFuncs == nil {
		registeredGoFuncs = make(map[string]*goFuncBinding)
	}
	// 锁内复检：上面的 checkGoFuncNameFree 在锁外，两个并发注册同名函数会同时通过它
	if _, dup := registeredGoFuncs[name]; dup {
		return fmt.Errorf("函数 '%s' 已由 RegisterGoFunc 注册", name)
	}
	registeredGoFuncs[name] = binding
	return nil
}

// newGoFuncBinding 校验签名并固化反射信息。不支持的类型在注册期就报错，
// 而不是等到某次调用才炸。
func newGoFuncBinding(name string, fn interface{}) (*goFuncBinding, error) {
	if name == "" {
		return nil, fmt.Errorf("函数名不能为空")
	}
	t := reflect.TypeOf(fn)
	if t == nil || t.Kind() != reflect.Func {
		return nil, fmt.Errorf("RegisterGoFunc 需要函数，实际 %T", fn)
	}
	if t.IsVariadic() && t.NumIn() == 0 {
		return nil, fmt.Errorf("函数 '%s' 的签名不合法", name)
	}

	binding := &goFuncBinding{name: name, fn: reflect.ValueOf(fn)}

	numIn := t.NumIn()
	for i := 0; i < numIn; i++ {
		paramType := t.In(i)
		if t.IsVariadic() && i == numIn-1 {
			binding.varElem = paramType.Elem()
			if err := checkLuaParamType(binding.varElem); err != nil {
				return nil, fmt.Errorf("函数 '%s' 的变参元素: %w", name, err)
			}
			continue
		}
		if err := checkLuaParamType(paramType); err != nil {
			return nil, fmt.Errorf("函数 '%s' 的第 %d 个形参: %w", name, i+1, err)
		}
		binding.fixed = append(binding.fixed, paramType)
	}

	numOut := t.NumOut()
	for i := 0; i < numOut; i++ {
		outType := t.Out(i)
		if i == numOut-1 && outType == errorType {
			binding.lastErr = true
			continue
		}
		if err := checkLuaReturnType(outType); err != nil {
			return nil, fmt.Errorf("函数 '%s' 的第 %d 个返回值: %w", name, i+1, err)
		}
		binding.out = append(binding.out, outType)
	}
	return binding, nil
}

// checkLuaParamType 校验形参类型能否从 Lua 值还原。
func checkLuaParamType(t reflect.Type) error {
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return nil
	case reflect.Pointer:
		elem := t.Elem()
		if elem == reflect.TypeOf(LuaTable{}) {
			return nil
		}
		if elem.Kind() == reflect.Struct {
			return nil
		}
		return fmt.Errorf("不支持的指针形参类型 %s（仅支持结构体与 *LuaTable）", t)
	case reflect.Func:
		return fmt.Errorf("不支持函数形参类型 %s（Lua 函数没有 Go 表示）", t)
	default:
		return fmt.Errorf("不支持的形参类型 %s", t)
	}
}

// checkLuaReturnType 校验返回值能否折成 Lua 值。
func checkLuaReturnType(t reflect.Type) error {
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Struct, reflect.Map, reflect.Slice, reflect.Array,
		reflect.Pointer:
		return nil
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return fmt.Errorf("不支持的返回值类型 %s", t)
	default:
		return fmt.Errorf("不支持的返回值类型 %s", t)
	}
}

// checkGoFuncNameFree 检查名字是否被内置函数、RegisterLuaFunc 或本注册表占用。
// 锁序固定 registeredFuncsMu → registeredGoFuncsMu（与 RegisterLuaFunc 一致）。
func checkGoFuncNameFree(name string) error {
	for _, builtin := range defaultFunctionNames {
		if name == builtin {
			return fmt.Errorf("函数名 '%s' 已被 Lua 内置函数占用", name)
		}
	}

	registeredFuncsMu.RLock()
	_, dup := registeredFuncs[name]
	registeredFuncsMu.RUnlock()
	if dup {
		return fmt.Errorf("函数 '%s' 已由 RegisterLuaFunc 注册", name)
	}

	registeredGoFuncsMu.RLock()
	_, dup = registeredGoFuncs[name]
	registeredGoFuncsMu.RUnlock()
	if dup {
		return fmt.Errorf("函数 '%s' 已由 RegisterGoFunc 注册", name)
	}
	return nil
}

// handler 生成绑定到某个脚本实例上下文的 GoFunction 回调。
func (b *goFuncBinding) handler(ctx context.Context) rt.GoFunctionFunc {
	return func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		fixed := len(b.fixed)

		// 必须先查再取：Arg 不做边界检查
		if err := c.CheckNArgs(fixed); err != nil {
			return nil, fmt.Errorf("函数 '%s' 需要 %d 个参数，实际 %d", b.name, fixed, c.NArgs())
		}
		total := c.NArgs() + len(c.Etc())
		if b.varElem == nil && total > fixed {
			return nil, fmt.Errorf("函数 '%s' 需要 %d 个参数，实际 %d", b.name, fixed, total)
		}

		args := make([]reflect.Value, 0, total+1)
		for i := 0; i < fixed; i++ {
			argValue, err := LuaToReflectValueWithContext(ctx, c.Arg(i), b.fixed[i])
			if err != nil {
				return nil, fmt.Errorf("函数 '%s' 的第 %d 个参数: %w", b.name, i+1, err)
			}
			args = append(args, argValue)
		}

		if b.varElem != nil {
			tail := reflect.MakeSlice(reflect.SliceOf(b.varElem), 0, total-fixed)
			for i := fixed; i < total; i++ {
				argValue, err := LuaToReflectValueWithContext(ctx, c.Etc()[i-fixed], b.varElem)
				if err != nil {
					return nil, fmt.Errorf("函数 '%s' 的第 %d 个变参: %w", b.name, i-fixed+1, err)
				}
				tail = reflect.Append(tail, argValue)
			}
			args = append(args, tail)
		}

		var results []reflect.Value
		if b.varElem != nil {
			results = b.fn.CallSlice(args)
		} else {
			results = b.fn.Call(args)
		}

		// 末位 error 非 nil → 直接回抛，Lua 侧成为运行时错误（可被 pcall 捕获）
		if b.lastErr {
			last := results[len(results)-1]
			results = results[:len(results)-1]
			if !last.IsNil() {
				return nil, last.Interface().(error)
			}
		}

		values := make([]rt.Value, 0, len(results))
		for i, result := range results {
			luaValue, err := GoToLuaValueErr(ctx, result.Interface())
			if err != nil {
				return nil, fmt.Errorf("函数 '%s' 的第 %d 个返回值: %w", b.name, i+1, err)
			}
			values = append(values, luaValue)
		}
		return c.PushingNext(t.Runtime, values...), nil
	}
}

// replayGoFuncsLocked 把反射注册的函数回放到当前 runtime。调用方必须持有 rtMu 写锁
// （构造期例外，理由见 replayRegistrationsLocked）。
func (ls *LuaScript) replayGoFuncsLocked(ctx context.Context) {
	registeredGoFuncsMu.RLock()
	defer registeredGoFuncsMu.RUnlock()

	for name, binding := range registeredGoFuncs {
		fn := ls.runtime.NewGoFunction(binding.handler(ctx), name, binding.nArgs(), true)
		ls.runtime.SetEnv(ls.env, name, rt.FunctionValue(fn))
	}
}
