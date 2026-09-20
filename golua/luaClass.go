package lua

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	rt "github.com/arnodel/golua/runtime"
)

// ILuaClassInterface 注册类接口（保持向后兼容）
type ILuaClassInterface interface {
	Init(id int64, luascript *LuaScript)
	Delete()
	Update()
}

// ILuaClassObject 注册类接口基类（保持向后兼容）
type ILuaClassObject struct{}

func (l *ILuaClassObject) Delete() {
}

func (l *ILuaClassObject) Update() {
}

// userDataMeta 元数据，用于关联 Go 对象和 Lua userdata
type userDataMeta struct {
	uid    int64
	script *LuaScript
}

// ClassRegistry 类注册信息
type ClassRegistry struct {
	Name string
	// reflectType 是类的指针类型（如 *Npc）：只有它的方法集才包含指针接收者
	// 方法，用结构体类型枚举会得到空方法表，Lua 侧一个方法都调不到。
	reflectType reflect.Type
	methodCache map[string]reflect.Method
	methodMutex sync.RWMutex
}

// 全局类注册表，避免重复创建
var (
	classRegistryMap = syncmap.NewAny() // key: className, value: *ClassRegistry
)

// lifecycleMethods 是 ILuaClassInterface 的框架方法，由 Go 侧驱动，
// 暴露给 Lua 等于让脚本自己改写对象的登记键与生命周期。
var lifecycleMethods = map[string]bool{
	"Init":   true,
	"Delete": true,
	"Update": true,
}

// getMethod 获取缓存的反射方法
func (cr *ClassRegistry) getMethod(methodName string) (reflect.Method, bool) {
	cr.methodMutex.RLock()
	method, ok := cr.methodCache[methodName]
	cr.methodMutex.RUnlock()
	return method, ok
}

// buildMethodCache 构建方法缓存
func (cr *ClassRegistry) buildMethodCache() {
	cr.methodMutex.Lock()
	defer cr.methodMutex.Unlock()

	if cr.methodCache != nil {
		return // 已经构建过
	}

	cr.methodCache = make(map[string]reflect.Method)
	for i := 0; i < cr.reflectType.NumMethod(); i++ {
		method := cr.reflectType.Method(i)
		if method.PkgPath != "" { // 只导出方法
			continue
		}
		if lifecycleMethods[method.Name] {
			continue
		}
		cr.methodCache[method.Name] = method
	}
}

// methodCallback 方法回调闭包
type methodCallback struct {
	methodName string
	ctx        context.Context
}

// callBack 处理 Lua 到 Go 的方法调用
func (mc *methodCallback) callBack(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	select {
	case <-mc.ctx.Done():
		return nil, fmt.Errorf("Lua script is closed")
	default:
	}

	// 本函数注册时固定为「1 个具名参数 + etc」：具名槽放接收者，其余实参全在 etc。
	// 必须先查再取：Arg 不做任何边界检查，槽数为 0 时 c.Arg(0) 直接越界 panic
	if c.NArgs() < 1 {
		return nil, fmt.Errorf("method '%s' called without receiver", mc.methodName)
	}

	// 获取第一个参数（对象本身）
	arg0 := c.Arg(0)

	// 检查是否是 UserData
	userData, ok := arg0.TryUserData()
	if !ok {
		return nil, fmt.Errorf("invalid userdata: not a userdata type")
	}
	if userData == nil {
		return c.Next(), nil
	}

	meta, ok := userData.Value().(*userDataMeta)
	if !ok {
		return nil, fmt.Errorf("invalid userdata metadata type")
	}

	// 从 syncmap 中获取实际对象
	dataRaw, ok := meta.script.registeredObjects.Load(meta.uid)
	if !ok {
		vars.Error("object not found: uid=%d", meta.uid)
		return c.Next(), nil
	}

	data, ok := dataRaw.(ILuaClassInterface)
	if !ok {
		vars.Error("object does not implement ILuaClassInterface: uid=%d", meta.uid)
		return c.Next(), nil
	}

	// 获取类注册信息：GetClassName 的第二个返回值是方法名列表，不是 error
	className, _ := util.GetClassName(data)

	registryRaw, ok := classRegistryMap.Load(className)
	if !ok {
		return nil, fmt.Errorf("class '%s' not registered", className)
	}
	registry := registryRaw.(*ClassRegistry)

	// 从缓存获取方法
	method, ok := registry.getMethod(mc.methodName)
	if !ok {
		return nil, fmt.Errorf("method '%s' not found in class '%s'", mc.methodName, className)
	}

	// 具名槽里只有接收者，其余实参全在 etc
	luaArgs := make([]rt.Value, 0, 1+len(c.Etc()))
	luaArgs = append(luaArgs, c.Arg(0))
	luaArgs = append(luaArgs, c.Etc()...)

	// 指针类型的方法以「静态函数」形式反射调用，第一个入参是接收者，
	// 必须显式补上；否则 Call 会拿 Lua 的实参去填接收者位置。
	recv := reflect.ValueOf(data)
	if !recv.Type().AssignableTo(method.Type.In(0)) {
		return nil, fmt.Errorf("receiver type mismatch for method '%s'", mc.methodName)
	}

	numIn := method.Type.NumIn()
	variadic := method.Type.IsVariadic()
	fixed := numIn
	if variadic {
		fixed = numIn - 1 // 末尾的切片在可变段里凑
	}
	if len(luaArgs) < fixed {
		return nil, fmt.Errorf("method '%s' needs %d argument(s), got %d", mc.methodName, fixed, len(luaArgs))
	}
	if !variadic && len(luaArgs) > fixed {
		return nil, fmt.Errorf("method '%s' takes %d argument(s), got %d", mc.methodName, fixed, len(luaArgs))
	}

	args := make([]reflect.Value, 0, numIn)
	args = append(args, recv)
	for i := 1; i < fixed; i++ {
		argValue, err := luaArgToReflect(mc.ctx, luaArgs[i], method.Type.In(i))
		if err != nil {
			return nil, fmt.Errorf("parameter %d conversion failed: %w", i, err)
		}
		args = append(args, argValue)
	}

	var resultValues []reflect.Value
	if variadic {
		sliceType := method.Type.In(numIn - 1)
		etc := reflect.MakeSlice(sliceType, 0, len(luaArgs)-fixed)
		for i := fixed; i < len(luaArgs); i++ {
			argValue, err := luaArgToReflect(mc.ctx, luaArgs[i], sliceType.Elem())
			if err != nil {
				return nil, fmt.Errorf("variadic parameter %d conversion failed: %w", i-fixed+1, err)
			}
			etc = reflect.Append(etc, argValue)
		}
		args = append(args, etc)
		resultValues = method.Func.CallSlice(args)
	} else {
		resultValues = method.Func.Call(args)
	}

	// 处理返回值
	next := c.Next()
	for i, result := range resultValues {
		if result.Kind() == reflect.Invalid {
			return nil, fmt.Errorf("return value %d is invalid", i+1)
		}

		luaValue := GoToLuaValueWithContext(mc.ctx, result.Interface())
		t.Push1(next, luaValue)
	}

	return next, nil
}

// luaArgToReflect 转换单个 Lua 实参。interface 形参拿到 nil 时 reflect.ValueOf
// 会返回无效值，直接交给 Call 就是 panic，这里回退成零值。
//
// 转换结果必须自己可赋值给目标形参：非空 interface 形参拿到不相干的类型时，
// reflect.Call 会 panic 把整个进程带下去，这里提前报错。
func luaArgToReflect(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	argValue, err := LuaToReflectValueWithContext(ctx, v, targetType)
	if err != nil {
		return reflect.Value{}, err
	}
	if !argValue.IsValid() {
		return reflect.Zero(targetType), nil
	}
	if !argValue.Type().AssignableTo(targetType) {
		return reflect.Value{}, fmt.Errorf("实参类型 %s 无法赋给形参类型 %s", argValue.Type(), targetType)
	}
	return argValue, nil
}

// registerClass 注册一个 Go 类到 Lua 脚本（向后兼容，委托给 Context 版本）
func registerClass(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	return registerClassWithContext(ctx, class, script)
}
