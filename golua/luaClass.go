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
	Name        string
	reflectType reflect.Type
	methodCache map[string]reflect.Method
	methodMutex sync.RWMutex
}

// 全局类注册表，避免重复创建
var (
	classRegistryMap = syncmap.NewAny() // key: className, value: *ClassRegistry
)

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
		if method.PkgPath == "" { // 只导出方法
			cr.methodCache[method.Name] = method
		}
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

	// 获取类注册信息
	className, err := util.GetClassName(data)
	if err != nil {
		return nil, fmt.Errorf("get class name failed: %v", err)
	}

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

	// 处理输入参数
	args := make([]reflect.Value, 0, method.Type.NumIn())
	luaArgCount := c.NArgs()

	// Lua 参数从索引 1 开始（索引 0 是对象自身）
	for i := 0; i < method.Type.NumIn() && (i+1) < luaArgCount; i++ {
		luaIdx := i + 1
		paramType := method.Type.In(i)
		argVal := c.Arg(luaIdx)
		argValue, err := LuaToReflectValueWithContext(mc.ctx, argVal, paramType)
		if err != nil {
			return nil, fmt.Errorf("parameter %d conversion failed: %w", i+1, err)
		}
		args = append(args, argValue)
	}

	// 调用原始函数
	resultValues := method.Func.Call(args)

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

// registerClass 注册一个 Go 类到 Lua 脚本（向后兼容，委托给 Context 版本）
func registerClass(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	return registerClassWithContext(ctx, class, script)
}
