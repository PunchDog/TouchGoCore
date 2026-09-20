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
	registeredFuncs   map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)
	registeredFuncsMu sync.RWMutex // 保护 registeredFuncs 的并发访问
	// registeredClasses 按类名索引：重复注册同一类型走幂等，跨包重名能被查出来
	registeredClasses   map[string]ILuaClassInterface
	registeredClassesMu sync.RWMutex // 保护 registeredClasses 的并发访问
)

// registerDefaultFunctions 注册默认的内置函数
//
// 名字不能压过标准库：lib.LoadAll 里 base 带了 error、debuglib 带了 debug 表，
// 早前用同名 Go 函数覆盖后，脚本里的 error("配置无效") 只落一行日志就继续往下跑，
// debug.traceback 这类惯用法也全废了。日志入口另起名字。
func registerDefaultFunctions(ls *LuaScript) error {
	ls.runtime.SetEnvGoFunc(ls.env, "info", info, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "logdebug", debug, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "logerror", error1, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "dofile", dofile, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "getpathluafile", getpathluafile, 1, false)
	return nil
}

// registerClassWithContext 使用上下文注册一个 Go 类到 Lua 脚本
func registerClassWithContext(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	classPtrType := reflect.TypeOf(class)
	if classPtrType == nil || classPtrType.Kind() != reflect.Pointer {
		return fmt.Errorf("类必须以指针形式注册（如 &Npc{}），实际: %v", classPtrType)
	}
	classType := classPtrType.Elem()

	// 获取类名：GetClassName 的第二个返回值是方法名列表，不是 error
	className, _ := util.GetClassName(class)

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
		registry = &ClassRegistry{
			Name:        className,
			reflectType: classPtrType, // 指针类型，方法集才含指针接收者方法
			methodCache: nil,          // 延迟构建
		}
		classRegistryMap.Store(className, registry)
		registry.buildMethodCache() // 立即构建方法缓存
		vars.Info("registered Lua class: %s with %d methods", className, len(registry.methodCache))
	}

	// 类元表 + 方法表。golua 对 userdata 只走 metaGetS("__index")，从不 raw 查
	// 元表本身（runtime/lib.go 的 Index），所以方法必须挂在 __index 指向的那张表上；
	// 修复前方法被逐个塞进元表，永远读不到，每次取值都由 __index 现造一个闭包。
	//
	// 元表必须交给 rt.NewUserData 的第二参才会生效：修复前这张表建完就被丢弃，
	// userdata 上没有元表，Lua 侧任何 `npc:SetXxx()` 都是
	// "attempt to index a userdata value"，整个类注册等于没跑通。
	metaTable := rt.NewTable()
	methodTable := rt.NewTable()

	// 注册 __tostring 元方法
	tostringFunc := rt.NewGoFunction(func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		next := c.Next()
		t.Push1(next, rt.StringValue(fmt.Sprintf("%s userdata", className)))
		return next, nil
	}, "__tostring", 1, false)
	script.runtime.SetEnv(metaTable, "__tostring", rt.FunctionValue(tostringFunc))

	// 注册所有方法到 __index 表（从缓存）
	registry.methodMutex.RLock()
	methodNames := make([]string, 0, len(registry.methodCache))
	for methodName := range registry.methodCache {
		methodNames = append(methodNames, methodName)
	}
	registry.methodMutex.RUnlock()
	for _, methodName := range methodNames {
		methodMeta := &methodCallback{methodName: methodName, ctx: ctx}
		// nArgs=1 是接收者槽，hasEtc=true 才收得到其余实参
		methodFunc := rt.NewGoFunction(methodMeta.callBack, methodName, 1, true)
		script.runtime.SetEnv(methodTable, methodName, rt.FunctionValue(methodFunc))
	}
	script.runtime.SetEnv(metaTable, "__index", rt.TableValue(methodTable))

	// 创建类构造函数
	constructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		if etc := c.Etc(); len(etc) > 0 {
			// 构造器不吃参数：Go 侧无法把任意 Lua 实参映射到某个业务字段，
			// 静默丢掉会让脚本以为 ID 已经设上了
			return nil, fmt.Errorf("class '%s' takes no constructor arguments, call the setters instead", className)
		}

		cls := reflect.New(classType).Interface().(ILuaClassInterface)

		// 每个 userdata 一个独立对象号。修复前这里用 script.UID 当键：
		// 而 UID 在主脚本跑完之后才赋值（脚本执行期间恒为 0），于是同一段脚本
		// 里 `Npc()` 十次就得到十个对象、一个键，后一个把前一个盖掉——
		// 之后所有 userdata 上的方法调用都落在最后创建的那个对象上（身份踩踏）。
		objectUID := script.nextObjectID.Add(1)
		cls.Init(objectUID, script)

		// 存储到 syncmap 中
		script.registeredObjects.Store(objectUID, cls)

		// 创建元数据
		meta := &userDataMeta{
			uid:    objectUID,
			script: script,
		}

		next := c.Next()
		t.Push1(next, rt.UserDataValue(rt.NewUserData(meta, metaTable)))
		return next, nil
	}

	// 注册构造函数到全局
	constructorFunc := rt.NewGoFunction(constructor, className, 0, true)
	script.runtime.SetEnv(script.env, className, rt.FunctionValue(constructorFunc))

	return nil
}
