package lua

import (
	"context"
	"fmt"

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
		return nil, fmt.Errorf("Lua service not started")
	}
	return dl.CallWithContext(ctx, funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例
func RegisterLuaFunc(funcName string, function func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)) error {
	registeredFuncsMu.Lock()
	defer registeredFuncsMu.Unlock()

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
	if class == nil {
		return fmt.Errorf("cannot register nil class")
	}

	className, err := util.GetClassName(class)
	if err != nil {
		return fmt.Errorf("get class name failed: %v", err)
	}

	registeredClassesMu.Lock()
	defer registeredClassesMu.Unlock()

	if registeredClasses == nil {
		registeredClasses = make(map[ILuaClassInterface]bool)
	}
	if _, ok := registeredClasses[class]; ok {
		return fmt.Errorf("class '%s' already registered", className)
	}
	registeredClasses[class] = true
	return nil
}

// RunLua 启动 Lua 服务
func RunLua() error {
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

// StopLua 关闭所有的定时器
func StopLua() {
	luaInstancesMu.Lock()
	defer luaInstancesMu.Unlock()

	for _, ls := range luaInstances {
		ls.Close()
	}
	luaInstances = nil
	defaultLua = nil
}

// Run 启动 Lua 服务（公开接口）
func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	luaParentCtx = ctx
	if luaCfg() == nil {
		vars.Info("不启动lua服务")
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
