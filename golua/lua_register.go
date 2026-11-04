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

// defaultFunctionNames 是内置宿主函数名。
// 注册表（RegisterLuaFunc / RegisterGoFunc）不允许占用它们：覆盖内置函数会让
// 脚本里的 info/dofile 悄悄换实现。
var defaultFunctionNames = []string{"info", "logdebug", "logerror", "dofile", "getpathluafile"}

// registerDefaultFunctions 注册默认的内置函数
//
// 名字不能压过标准库：lib.LoadAll 里 base 带了 error、debuglib 带了 debug 表，
// 早前用同名 Go 函数覆盖后，脚本里的 error("配置无效") 只落一行日志就继续往下跑，
// debug.traceback 这类惯用法也全废了。日志入口另起名字。
//
// SetEnvGoFunc 没有错误通道，注册不可能失败，所以本函数不返回 error。
func registerDefaultFunctions(ls *LuaScript) {
	ls.runtime.SetEnvGoFunc(ls.env, "info", info, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "logdebug", debug, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "logerror", error1, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "dofile", dofile, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "getpathluafile", getpathluafile, 1, false)
}

// registerClassWithContext 把一个 Go 类绑定进脚本环境：元表、方法表与构造器。
//
// 调用方必须持有 rtMu 写锁（或处于构造期，理由见 replayRegistrationsLocked）。
func registerClassWithContext(ctx context.Context, class ILuaClassInterface, script *LuaScript) error {
	classPtrType, err := classPtrTypeOf(class)
	if err != nil {
		return err
	}
	className := shortClassName(class)

	// 同一个脚本实例内不重复绑定（Init 幂等回放靠这条早退）
	if existing := script.env.Get(rt.StringValue(className)); existing != rt.NilValue {
		vars.Info("class '%s' already registered, skipping", className)
		return nil
	}

	registry := getOrBuildClassRegistry(className, classPtrType)
	metaTable := newClassMetaTable(script, className)
	bindClassMethods(script, metaTable, registry, className, ctx)

	constructor := newClassConstructor(script, classPtrType.Elem(), metaTable, className)
	script.runtime.SetEnv(script.env, className, rt.FunctionValue(constructor))
	return nil
}

// classPtrTypeOf 校验注册对象必须是指针，返回其指针类型。
func classPtrTypeOf(class ILuaClassInterface) (reflect.Type, error) {
	classPtrType := reflect.TypeOf(class)
	if classPtrType == nil || classPtrType.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("类必须以指针形式注册（如 &Npc{}），实际: %v", classPtrType)
	}
	return classPtrType, nil
}

// shortClassName 取注册用的短类名。
// util.GetClassName 的第二个返回值是方法名列表，不是 error，桥接层不用。
func shortClassName(class ILuaClassInterface) string {
	name, _ := util.GetClassName(class)
	return name
}

// getOrBuildClassRegistry 取或建该类的反射方法缓存。
//
// 缓存按短类名常驻、跨脚本实例与跨热重载复用，所以重载不会重复做反射枚举；
// 每实例各一份的只有元表与方法表那两张 rt.Table。
func getOrBuildClassRegistry(className string, classPtrType reflect.Type) *ClassRegistry {
	if raw, loaded := classRegistryMap.Load(className); loaded {
		return raw.(*ClassRegistry)
	}
	registry := &ClassRegistry{
		Name:        className,
		reflectType: classPtrType, // 指针类型，方法集才含指针接收者方法
	}
	classRegistryMap.Store(className, registry)
	registry.buildMethodCache()
	vars.Info("registered Lua class: %s with %d methods", className, len(registry.methodCache))
	return registry
}

// newClassMetaTable 建类元表并挂上 __tostring。
//
// 这张表必须交给 rt.NewUserData 的第二参才会生效：修复前这张表建完就被丢弃，
// userdata 上没有元表，Lua 侧任何 `npc:SetXxx()` 都是
// "attempt to index a userdata value"，整个类注册等于没跑通。
func newClassMetaTable(script *LuaScript, className string) *rt.Table {
	// 两个元方法都是字符串键，预分配给哈希部分（nrec），不是序列部分（nseq）
	metaTable := rt.NewTableWithCapacity(0, 2)

	tostringFunc := script.runtime.NewGoFunction(func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		next := c.Next()
		t.Push1(next, rt.StringValue(fmt.Sprintf("%s userdata", className)))
		return next, nil
	}, "__tostring", 1, false)
	script.runtime.SetEnv(metaTable, "__tostring", rt.FunctionValue(tostringFunc))
	return metaTable
}

// bindClassMethods 为每个暴露的方法建一个 GoFunction，挂到 __index 指向的方法表上。
//
// golua 对 userdata 只走 metaGetS("__index")，从不 raw 查元表本身（runtime/lib.go 的
// Index），所以方法必须挂在 __index 指向的那张表上；修复前方法被逐个塞进元表，
// 永远读不到，每次取值都由 __index 现造一个闭包。
//
// 绑定形状：具名槽数取方法的真实元数（含接收者），hasEtc 恒真——
// 否则超出槽位的实参会被 golua 丢弃，方法体再也报不出「参数过多」。
func bindClassMethods(script *LuaScript, metaTable *rt.Table, registry *ClassRegistry, className string, ctx context.Context) {
	methodNames := registry.methodNames()
	methodTable := rt.NewTableWithCapacity(0, len(methodNames))

	for _, methodName := range methodNames {
		method, ok := registry.getMethod(methodName)
		if !ok {
			// buildMethodCache 之后方法表不会缩容；真取不到就说明缓存被并发重建过，
			// 跳过这一个而不是让整个类注册失败
			vars.Error("class '%s' method '%s' vanished from cache", className, methodName)
			continue
		}
		mc := newMethodCallback(className, method, ctx)
		methodFunc := script.runtime.NewGoFunction(mc.callBack, methodName, mc.fixed, true)
		script.runtime.SetEnv(methodTable, methodName, rt.FunctionValue(methodFunc))
	}

	script.runtime.SetEnv(metaTable, "__index", rt.TableValue(methodTable))
}

// newClassConstructor 建 Lua 侧的类构造器（`Npc()`）。
func newClassConstructor(script *LuaScript, classType reflect.Type, metaTable *rt.Table, className string) *rt.GoFunction {
	constructor := func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		if len(c.Etc()) > 0 {
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
		script.registeredObjects.Store(objectUID, cls)

		next := c.Next()
		// 元表必须作为 NewUserData 的第二参才生效（见 newClassMetaTable）。
		// 不要换成 Runtime.NewUserDataValue：那条会注册终结器，而配置脚本里的
		// `local npc = Npc()` 在主块返回后就不可达，按 GC 回收等于删掉刚配好的对象。
		t.Push1(next, rt.UserDataValue(rt.NewUserData(&userDataMeta{
			uid:    objectUID,
			script: script,
		}, metaTable)))
		return next, nil
	}
	return script.runtime.NewGoFunction(constructor, className, 0, true)
}
