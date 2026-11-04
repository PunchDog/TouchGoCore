package lua

import (
	"context"
	"fmt"
	"reflect"

	"touchgocore/util"
	"touchgocore/vars"

	rt "github.com/arnodel/golua/runtime"
)

// ==================== Lua 包级门面 ====================

// Call 调用默认 Lua 实例的函数
func Call(funcName string, args ...interface{}) ([]interface{}, error) {
	return CallWithContext(context.Background(), funcName, args...)
}

// CallWithContext 使用上下文调用默认 Lua 实例的函数
func CallWithContext(ctx context.Context, funcName string, args ...interface{}) ([]interface{}, error) {
	luaInstancesMu.RLock()
	dl := defaultLua
	luaInstancesMu.RUnlock()
	if dl == nil {
		return nil, fmt.Errorf("lua 服务未启动 (Lua service not started)")
	}
	return dl.CallWithContext(ctx, funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例
//
// 注册表不知道宿主函数的真实元数，统一按「1 个具名槽 + etc」绑定：
// 第一个实参用 c.Arg(0)/c.StringArg(0) 取，其余实参只能从 c.Etc() 里取——
// 具名槽只有 1 格，c.Arg(1) 会直接越界 panic。
func RegisterLuaFunc(funcName string, function func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)) error {
	registeredFuncsMu.Lock()
	defer registeredFuncsMu.Unlock()

	for _, builtin := range defaultFunctionNames {
		if funcName == builtin {
			return fmt.Errorf("函数名 '%s' 已被 Lua 内置函数占用", funcName)
		}
	}
	// 与 RegisterGoFunc 共用命名空间：锁序固定 funcs → goFuncs，永不反向
	registeredGoFuncsMu.RLock()
	_, dupGoFunc := registeredGoFuncs[funcName]
	registeredGoFuncsMu.RUnlock()
	if dupGoFunc {
		return fmt.Errorf("function '%s' already registered by RegisterGoFunc", funcName)
	}

	if registeredFuncs == nil {
		registeredFuncs = make(map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error))
	}
	if _, ok := registeredFuncs[funcName]; ok {
		return fmt.Errorf("function '%s' already registered", funcName)
	}
	registeredFuncs[funcName] = function
	return nil
}

// RegisterLuaClass 注册一个类到所有 Lua 实例
func RegisterLuaClass(class ILuaClassInterface) error {
	// class == nil 只挡得住「未装箱的 nil」：(*Npc)(nil) 这类带类型的空指针
	// 是非空接口，会一路走到 GetClassName 的 reflect.Indirect 上 panic
	rv := reflect.ValueOf(class)
	if !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return fmt.Errorf("cannot register nil class")
	}
	// 值类型的方法集不含指针接收者方法，注册进去就是一张空方法表；
	// 留到建脚本实例时才报，就只剩一行「register Lua class failed」了
	if rv.Kind() != reflect.Pointer {
		return fmt.Errorf("类必须以指针形式注册（如 &Npc{}），实际: %T", class)
	}

	// GetClassName 的第二个返回值是方法名列表，不是 error
	className, _ := util.GetClassName(class)

	registeredClassesMu.Lock()
	defer registeredClassesMu.Unlock()

	if registeredClasses == nil {
		registeredClasses = make(map[string]ILuaClassInterface)
	}
	if existing, ok := registeredClasses[className]; ok {
		// 类名只取短名，跨包重名会撞在这里；同一类型重复注册则按幂等处理，
		// 否则每重启一次就多一条记录，注册表只增不减
		if reflect.TypeOf(existing) == reflect.TypeOf(class) {
			return nil
		}
		return fmt.Errorf("class '%s' already registered by %s, cannot register %s",
			className, reflect.TypeOf(existing), reflect.TypeOf(class))
	}
	registeredClasses[className] = class
	return nil
}

// RunLua 启动 Lua 服务
func RunLua() error {
	// 覆盖绕过 Run 直接调本函数的调用方：节拍必须在建实例之前落到配置值上
	applyLuaTimingConfig()

	lc := luaCfg()
	if lc == nil {
		vars.Info("Lua service disabled")
		return nil
	}

	luaInstancesMu.Lock()
	luaInstances = make(map[int64]*LuaScript)
	luaInstancesMu.Unlock()

	var err error
	dl, err := NewLuaScript(lc.ScriptPath)
	if err != nil {
		return fmt.Errorf("create Lua script failed: %w", err)
	}

	luaInstancesMu.Lock()
	defaultLua = dl
	luaInstancesMu.Unlock()

	vars.Info("Lua service started successfully")
	return nil
}

// StopLua 关闭所有的定时器。
//
// 先在锁内摘走整张登记表并置 nil，再在锁外逐个 Close：Close 内部会经
// unregisterLuaInstance 取 luaInstancesMu 摘除自己的条目，若持锁调用 Close
// 就是同锁重入死锁（sync.RWMutex 不可重入）。
func StopLua() {
	luaInstancesMu.Lock()
	snapshot := make([]*LuaScript, 0, len(luaInstances))
	for _, ls := range luaInstances {
		snapshot = append(snapshot, ls)
	}
	luaInstances = nil
	defaultLua = nil
	luaInstancesMu.Unlock()

	for _, ls := range snapshot {
		ls.Close()
	}
}

// swapDefaultLua 在默认实例被换代时把它指向新实例。
//
// 自动重载走的是「新建实例」路径：closeKeepingObjects 已把旧实例的 runtime 拆掉，
// 而 defaultLua 此前没人更新，于是 lua.Call 一直打到那个空壳上，恒返回
// 「lua runtime is not available (closed or reloading)」。
//
// 只在归属匹配时替换：监视器托管的实例未必是默认实例（例如测试里裸建的），
// 无条件替换会把别人的默认实例顶掉。
// 锁内不得取 rtMu：defaultLua 的读写与 Close/StopLua 共用 luaInstancesMu，
// 拿锁再去做会重入锁的事就是死锁（见 StopLua 注释）。
func swapDefaultLua(old, next *LuaScript) {
	luaInstancesMu.Lock()
	if defaultLua == old {
		defaultLua = next
	}
	luaInstancesMu.Unlock()
}

// Run 启动 Lua 服务（公开接口）
func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	setLuaParentCtx(ctx)
	// 顺序要紧：applyLuaTimingConfig 读的就是刚存进去的这份父上下文
	applyLuaTimingConfig()
	if luaCfg() == nil {
		vars.Info("不启动 lua 服务")
		return nil
	}
	return RunLua()
}

// Stop 关闭所有 Lua 实例。ctx 用于与其它服务 Stop 签名对齐。
func Stop(ctx context.Context) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			StopLua()
			return
		default:
		}
	}
	StopLua()
}
