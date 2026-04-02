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
		return nil, fmt.Errorf("get class name failed: %w", err)
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

// registerClass 注册一个 Go 类到 Lua 脚本
func registerClass(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	// 获取类名
	className, err := util.GetClassName(class)
	if err != nil {
		return fmt.Errorf("get class name failed: %w", err)
	}

	// 检查是否已存在同名类（避免重复注册）
	existing := script.env.Get(rt.StringValue(className))
	if existing != rt.NilValue {
		vars.Info("class '%s' already registered, skipping", className)
		return nil
	}

	// 创建或获取类注册信息
	var registry *ClassRegistry
	rawRegistry, loaded := classRegistryMap.Load(className)
	if loaded {
		registry = rawRegistry.(*ClassRegistry)
	} else {
		// 创建新的类注册信息
		classType := reflect.TypeOf(class).Elem()
		registry = &ClassRegistry{
			Name:        className,
			reflectType: classType,
			methodCache: nil, // 延迟构建
		}
		classRegistryMap.Store(className, registry)
		registry.buildMethodCache() // 立即构建方法缓存
		vars.Info("registered Lua class: %s with %d methods", className, len(registry.methodCache))
	}

	// 创建类构造函数
	constructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		classType := reflect.TypeOf(class).Elem()
		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 初始化对象（调用旧版Init方法以保持兼容）
		cls.Init(script.UID, script)

		// 存储到 syncmap 中
		script.registeredObjects.Store(script.UID, cls)

		// 创建元数据
		meta := &userDataMeta{
			uid:    script.UID,
			script: script,
		}

		// 创建 UserData
		userData := rt.NewUserData(meta, nil)

		next := c.Next()
		t.Push1(next, rt.UserDataValue(userData))
		return next, nil
	}

	// 创建析构函数（__gc 元方法）
	destructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		arg0 := c.Arg(0)

		userData, ok := arg0.TryUserData()
		if !ok || userData == nil {
			return c.Next(), nil
		}

		meta, ok := userData.Value().(*userDataMeta)
		if !ok {
			return c.Next(), nil
		}

		// 从 syncmap 中获取并删除对象
		if dataRaw, ok := meta.script.registeredObjects.Load(meta.uid); ok {
			if data, ok := dataRaw.(ILuaClassInterface); ok {
				// 调用旧版Delete方法以保持兼容
				data.Delete()
				meta.script.registeredObjects.Delete(meta.uid)
			}
		}

		return c.Next(), nil
	}

	// 创建 __index 元方法
	indexMethod := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		arg1 := c.Arg(1)
		methodName, ok := arg1.TryString()
		if !ok {
			return c.Next(), nil
		}

		// 从缓存检查方法是否存在
		if _, ok := registry.getMethod(methodName); !ok {
			return c.Next(), nil
		}

		// 创建方法闭包
		methodMeta := &methodCallback{methodName: methodName, ctx: ctx}
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, false)

		next := c.Next()
		t.Push1(next, rt.FunctionValue(methodFunc))
		return next, nil
	}

	// 注册构造函数到全局
	constructorFunc := rt.NewGoFunction(constructor, className, 0, false)
	script.runtime.SetEnv(script.env, className, rt.FunctionValue(constructorFunc))

	// 创建类的元表
	metaTable := rt.NewTable()

	// 注册 __gc 元方法
	script.runtime.SetEnv(metaTable, "__gc", rt.FunctionValue(rt.NewGoFunction(destructor, "__gc", 1, false)))

	// 注册 __index 元方法
	script.runtime.SetEnv(metaTable, "__index", rt.FunctionValue(rt.NewGoFunction(indexMethod, "__index", 2, false)))

	// 注册 __tostring 元方法
	tostringFunc := rt.NewGoFunction(func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		next := c.Next()
		t.Push1(next, rt.StringValue(fmt.Sprintf("%s userdata", className)))
		return next, nil
	}, "__tostring", 1, false)
	script.runtime.SetEnv(metaTable, "__tostring", rt.FunctionValue(tostringFunc))

	// 注册所有方法到元表（从缓存）
	for methodName := range registry.methodCache {
		methodMeta := &methodCallback{methodName: methodName, ctx: ctx}
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, false)
		script.runtime.SetEnv(metaTable, methodName, rt.FunctionValue(methodFunc))
	}

	return nil
}
