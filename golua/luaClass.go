package lua

import (
	"fmt"
	"reflect"
	rt "github.com/arnodel/golua/runtime"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
)

// 注册类接口
type ILuaClassInterface interface {
	Init(id int64, luascript *LuaScript)
	Delete()
	Update()
}

// 注册类接口基类
type ILuaClassObject struct {
}

func (this *ILuaClassObject) Delete() {
}

func (this *ILuaClassObject) Update() {
}

// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////

// userDataMeta 元数据，用于关联 Go 对象和 Lua userdata
type userDataMeta struct {
	uid    int64
	script *LuaScript
	// 缓存反射信息，避免重复反射
	reflectType  reflect.Type
	reflectValue reflect.Value
}

// ClassRegistry 类注册信息（使用缓存的版本）
type ClassRegistry struct {
	Name        string
	MethodCache *MethodCache
}

// 全局类注册表，避免重复创建
var (
	classRegistryMap = syncmap.Map{} // key: className, value: *ClassRegistry
)

// methodCallback 表示一个方法回调
type methodCallback struct {
	methodName string
}

// callBack 处理 Lua 到 Go 的方法调用
func (mc *methodCallback) callBack(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
	// 获取第一个参数（对象本身）
	arg0 := c.Arg(0)

	// 检查是否是 UserData
	userData, ok := arg0.TryUserData()
	if !ok {
		vars.Error("LUA回调%s: 第一个参数不是 UserData", mc.methodName)
		return nil, fmt.Errorf("无效的 userdata")
	}
	if userData == nil {
		vars.Error("LUA回调%s: userdata为空", mc.methodName)
		return c.Next(), nil
	}

	meta, ok := userData.Value().(*userDataMeta)
	if !ok {
		vars.Error("LUA回调%s: userdata 元数据类型错误", mc.methodName)
		return c.Next(), nil
	}

	// 从 syncmap 中获取实际对象
	dataRaw, ok := meta.script.registeredObjects.Load(meta.uid)
	if !ok {
		vars.Error("LUA回调%s: 找不到uid=%d的对象", mc.methodName, meta.uid)
		return c.Next(), nil
	}

	data, ok := dataRaw.(ILuaClassInterface)
	if !ok {
		vars.Error("LUA回调%s: uid=%d的对象未实现ILuaClassInterface", mc.methodName, meta.uid)
		return c.Next(), nil
	}

	// 获取类缓存
	className, _ := util.GetClassName(data)
	registryRaw, ok := classRegistryMap.Load(className)
	if !ok {
		vars.Error("LUA回调%s: 类 %s 未注册", mc.methodName, className)
		return c.Next(), nil
	}
	registry := registryRaw.(*ClassRegistry)

	// 从缓存获取方法
	method, ok := registry.MethodCache.GetMethod(mc.methodName)
	if !ok {
		vars.Error("LUA回调%s: 方法不存在", mc.methodName)
		return c.Next(), nil
	}

	// 处理输入参数
	args := make([]reflect.Value, 0, method.Type.NumIn())
	luaArgCount := c.NArgs()

	// Lua 参数从索引 1 开始（索引 0 是对象自身）
	for i := 0; i < method.Type.NumIn() && (i+1) < luaArgCount; i++ {
		luaIdx := i + 1
		paramType := method.Type.In(i)
		argVal := c.Arg(luaIdx)
		argValue, err := LuaToReflectValue(argVal, paramType)
		if err != nil {
			vars.Error("LUA回调%s: 参数%d转换失败: %v", mc.methodName, i+1, err)
			return nil, err
		}
		args = append(args, argValue)
	}

	// 调用原始函数
	resultValues := method.Func.Call(args)

	// 处理返回值
	next := c.Next()
	for i, result := range resultValues {
		if result.Kind() == reflect.Invalid {
			vars.Error("LUA回调函数%s返回值%d无效", mc.methodName, i+1)
			return nil, fmt.Errorf("无效的返回值")
		}

		luaValue := GoToLuaValue(result.Interface())
		t.Push1(next, luaValue)
	}

	return next, nil
}

// registerClass 注册一个 Go 类到 Lua 脚本
func registerClass(class ILuaClassInterface, script *LuaScript) error {
	script.nextObjectID++

	// 获取类名和方法列表
	className, _ := util.GetClassName(class)

	// 检查是否已存在同名类（避免重复注册）
	existing := script.env.Get(rt.StringValue(className))
	if existing != rt.NilValue {
		vars.Info("类 %s 已注册，跳过重复注册", className)
		return nil
	}

	// 创建或获取类缓存
	var registry *ClassRegistry
	rawRegistry, loaded := classRegistryMap.Load(className)
	if loaded {
		registry = rawRegistry.(*ClassRegistry)
	} else {
		// 创建新缓存
		methodCache := CreateMethodCache(class)
		registry = &ClassRegistry{
			Name:        className,
			MethodCache: methodCache,
		}
		classRegistryMap.Store(className, registry)
		vars.Info("创建类缓存: %s, 方法数: %d", className, len(methodCache.Methods))
	}

	// 创建类构造函数
	constructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		// 创建新实例
		classType := reflect.TypeOf(class).Elem()
		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 初始化对象
		cls.Init(script.nextObjectID, script)

		// 存储到 syncmap 中
		script.registeredObjects.Store(script.nextObjectID, cls)

		// 创建元数据
		meta := &userDataMeta{
			uid:         script.nextObjectID,
			script:      script,
			reflectType: reflect.TypeOf(cls),
		}

		// 创建 UserData（使用空元表，稍后设置）
		userData := rt.NewUserData(meta, nil)

		// 返回 userdata
		next := c.Next()
		t.Push1(next, rt.UserDataValue(userData))
		return next, nil
	}

	// 创建析构函数（__gc 元方法）
	destructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		arg0 := c.Arg(0)

		userData, ok := arg0.TryUserData()
		if !ok {
			return c.Next(), nil
		}
		if userData == nil {
			return c.Next(), nil
		}

		meta, ok := userData.Value().(*userDataMeta)
		if !ok {
			return c.Next(), nil
		}

		// 从 syncmap 中获取并删除对象
		if dataRaw, ok := meta.script.registeredObjects.Load(meta.uid); ok {
			if data, ok := dataRaw.(ILuaClassInterface); ok {
				// 调用对象的 Delete 方法
				data.Delete()
				// 从 map 中删除
				meta.script.registeredObjects.Delete(meta.uid)
			}
		}

		return c.Next(), nil
	}

	// 创建 __index 元方法
	indexMethod := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		// 检查第二个参数是否是字符串（方法名）
		arg1 := c.Arg(1)

		methodName, ok := arg1.TryString()
		if !ok {
			return c.Next(), nil
		}

		// 从缓存获取方法
		_, ok = registry.MethodCache.GetMethod(methodName)
		if !ok {
			return c.Next(), nil
		}

		// 创建方法闭包
		methodMeta := &methodCallback{methodName: methodName}
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

	// 注册 __tostring 元方法（用于调试）
	tostringFunc := rt.NewGoFunction(func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		next := c.Next()
		t.Push1(next, rt.StringValue(fmt.Sprintf("%s userdata", className)))
		return next, nil
	}, "__tostring", 1, false)
	script.runtime.SetEnv(metaTable, "__tostring", rt.FunctionValue(tostringFunc))

	// 注册所有方法到元表
	for _, methodName := range registry.MethodCache.GetMethodNames() {
		methodMeta := &methodCallback{methodName: methodName}
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, false)
		script.runtime.SetEnv(metaTable, methodName, rt.FunctionValue(methodFunc))
	}

	vars.Info("成功注册 Lua 类: %s, 方法数: %d", className, len(registry.MethodCache.Methods))

	return nil
}
