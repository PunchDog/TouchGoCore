package lua

import (
	"fmt"
	"reflect"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
	"unsafe"

	"github.com/aarzilli/golua/lua"
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
func (mc *methodCallback) callBack(L *lua.State) int {
	// 获取 userdata 元数据
	userdataPtr := L.ToUserdata(1)
	if userdataPtr == nil {
		vars.Error("LUA回调%s: userdata为空", mc.methodName)
		return 0
	}

	meta := *(*userDataMeta)(userdataPtr)

	// 从 syncmap 中获取实际对象
	dataRaw, ok := meta.script.registeredObjects.Load(meta.uid)
	if !ok {
		vars.Error("LUA回调%s: 找不到uid=%d的对象", mc.methodName, meta.uid)
		return 0
	}

	data, ok := dataRaw.(ILuaClassInterface)
	if !ok {
		vars.Error("LUA回调%s: uid=%d的对象未实现ILuaClassInterface", mc.methodName, meta.uid)
		return 0
	}

	// 获取类缓存
	className, _ := util.GetClassName(data)
	registryRaw, ok := classRegistryMap.Load(className)
	if !ok {
		vars.Error("LUA回调%s: 类 %s 未注册", mc.methodName, className)
		return 0
	}
	registry := registryRaw.(*ClassRegistry)

	// 从缓存获取方法
	method, ok := registry.MethodCache.GetMethod(mc.methodName)
	if !ok {
		vars.Error("LUA回调%s: 方法不存在", mc.methodName)
		return 0
	}

	// 处理输入参数
	args := make([]reflect.Value, 0, method.Type.NumIn())
	luaArgCount := L.GetTop()

	// Lua 参数从索引 2 开始（索引 1 是对象自身）
	for i := 0; i < method.Type.NumIn() && (i+2) <= luaArgCount; i++ {
		luaIdx := i + 2
		paramType := method.Type.In(i)
		argValue, err := LuaToGoReflectValue(L, luaIdx, paramType)
		if err != nil {
			vars.Error("LUA回调%s: 参数%d转换失败: %v", mc.methodName, i+1, err)
			return 0
		}
		args = append(args, argValue)
	}

	// 调用原始函数
	resultValues := method.Func.Call(args)

	// 处理返回值
	for i, result := range resultValues {
		if result.Kind() == reflect.Invalid {
			vars.Error("LUA回调函数%s返回值%d无效", mc.methodName, i+1)
			return 0
		}

		if !PushValue(L, result.Interface()) {
			vars.Error("LUA回调函数%s返回值%d处理失败,类型:%v 值:%v",
				mc.methodName, i+1, result.Type(), result.Interface())
			return 0
		}
	}

	return len(resultValues)
}

// registerClass 注册一个 Go 类到 Lua 脚本
func registerClass(class ILuaClassInterface, script *LuaScript) error {
	script.nextObjectID++

	// 获取类名和方法列表
	className, _ := util.GetClassName(class)

	// 检查是否已存在同名类（避免重复注册）
	script.state.GetGlobal(className)
	if !script.state.IsNil(-1) {
		vars.Info("类 %s 已注册，跳过重复注册", className)
		script.state.Pop(1)
		return nil
	}
	script.state.Pop(1)

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
	constructor := func(L *lua.State) int {
		// 创建新实例
		classType := reflect.TypeOf(class).Elem()
		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 初始化对象
		cls.Init(script.nextObjectID, script)

		// 存储到 syncmap 中
		script.registeredObjects.Store(script.nextObjectID, cls)

		// 创建 userdata 并设置元表
		meta := &userDataMeta{
			uid:         script.nextObjectID,
			script:      script,
			reflectType: reflect.TypeOf(cls),
		}

		// 分配 userdata 内存
		userdataPtr := script.state.NewUserdata(uintptr(unsafe.Sizeof(meta)))
		*(*userDataMeta)(userdataPtr) = *meta

		// 设置元表
		script.state.LGetMetaTable(className)
		if script.state.IsNil(-1) {
			vars.Error("类 %s 的元表不存在", className)
			return 0
		}
		script.state.SetMetaTable(-2)

		return 1
	}

	// 创建析构函数（__gc 元方法）
	destructor := func(L *lua.State) int {
		userdataPtr := L.ToUserdata(1)
		if userdataPtr == nil {
			return 0
		}

		meta := *(*userDataMeta)(userdataPtr)

		// 从 syncmap 中获取并删除对象
		if dataRaw, ok := meta.script.registeredObjects.Load(meta.uid); ok {
			if data, ok := dataRaw.(ILuaClassInterface); ok {
				// 调用对象的 Delete 方法
				data.Delete()
				// 从 map 中删除
				meta.script.registeredObjects.Delete(meta.uid)
			}
		}

		return 0
	}

	// 创建 __index 元方法
	indexMethod := func(L *lua.State) int {
		// 检查栈顶是否是字符串（方法名）
		if L.Type(2) != lua.LUA_TSTRING {
			return 0
		}

		methodName := L.ToString(2)

		// 尝试从元表获取方法
		L.GetMetaTable(1)
		L.PushString(methodName)
		L.GetTable(-2)

		// 如果找到方法，返回
		if L.IsFunction(-1) {
			L.Remove(-2) // 移除元表
			return 1
		}

		// 没找到方法，返回 nil
		L.Pop(2) // 移除结果和元表
		return 0
	}

	// 注册构造函数到全局
	script.state.PushGoFunction(constructor)
	script.state.SetGlobal(className)

	// 创建或获取类的元表
	script.state.NewMetaTable(className)

	// 注册 __gc 元方法
	script.state.PushString("__gc")
	script.state.PushGoFunction(destructor)
	script.state.SetTable(-3)

	// 注册 __index 元方法
	script.state.PushString("__index")
	script.state.PushGoFunction(indexMethod)
	script.state.SetTable(-3)

	// 注册 __tostring 元方法（用于调试）
	script.state.PushString("__tostring")
	script.state.PushGoFunction(func(L *lua.State) int {
		script.state.PushString(fmt.Sprintf("%s userdata", className))
		return 1
	})
	script.state.SetTable(-3)

	// 注册所有方法到元表
	for _, methodName := range registry.MethodCache.GetMethodNames() {
		script.state.PushString(methodName)
		methodMeta := &methodCallback{methodName: methodName}
		script.state.PushGoFunction(methodMeta.callBack)
		script.state.SetTable(-3)
	}

	// 弹出元表
	script.state.Pop(1)

	vars.Info("成功注册 Lua 类: %s, 方法数: %d", className, len(registry.MethodCache.Methods))

	return nil
}
