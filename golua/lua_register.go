package lua

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"touchgocore/util"
	"touchgocore/vars"

	rt "github.com/arnodel/golua/runtime"
)

// 注册的函数和类
var (
	registeredFuncs     map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)
	registeredFuncsMu   sync.RWMutex // 保护 registeredFuncs 的并发访问
	registeredClasses   map[ILuaClassInterface]bool
	registeredClassesMu sync.RWMutex // 保护 registeredClasses 的并发访问
)

// registerDefaultFunctions 注册默认的内置函数
func registerDefaultFunctions(ls *LuaScript) error {
	ls.runtime.SetEnvGoFunc(ls.env, "info", info, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "debug", debug, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "error", error1, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "dofile", dofile, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "getpathluafile", getpathluafile, 1, false)
	return nil
}

// registerClassWithContext 使用上下文注册一个 Go 类到 Lua 脚本
func registerClassWithContext(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	// 获取类名
	className, err := util.GetClassName(class)
	if err != nil {
		return fmt.Errorf("get class name failed: %v", err)
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
