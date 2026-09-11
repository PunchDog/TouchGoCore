package lua

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/localtimer"
	"touchgocore/metrics"
	"touchgocore/syncmap"
	"touchgocore/vars"

	"github.com/arnodel/golua/lib"
	rt "github.com/arnodel/golua/runtime"
)

// Lua 定时器和垃圾回收配置
var (
	UpdateIntervalMs = getUpdateIntervalMs()
	GCTickCount      = getGCTickCount()
)

// getUpdateIntervalMs 从配置获取更新间隔
func luaCfg() *config.LuaConfig {
	cfg := corectx.CfgFrom(luaParentCtx)
	if cfg == nil {
		return nil
	}
	return cfg.LuaConfig
}

func getUpdateIntervalMs() int64 {
	if lc := luaCfg(); lc != nil && lc.UpdateInterval > 0 {
		return lc.UpdateInterval
	}
	return 1000 // 默认 1 秒
}

// getGCTickCount 从配置获取 GC tick 计数
func getGCTickCount() int64 {
	if lc := luaCfg(); lc != nil && lc.GCTickCount > 0 {
		return lc.GCTickCount
	}
	return 1800 // 默认 30 分钟 (30 * 60)
}

// Lua 文件扩展名
const (
	LuaFileExt = ".lua"
)

// 全局 Lua 实例管理
var (
	defaultLua     *LuaScript = nil
	luaInstances   map[int64]*LuaScript
	luaInstancesMu sync.RWMutex
	nextInstanceID atomic.Int64
	luaParentCtx   context.Context = context.Background()
)

// LuaScript Lua脚本实例
type LuaScript struct {
	runtime           *rt.Runtime
	thread            *rt.Thread
	returnValues      []interface{}
	initScriptPath    string
	registeredObjects *syncmap.MapAny
	timer             *luaTimer
	UID               int64
	env               *rt.Table
	ctx               context.Context
	cancel            context.CancelFunc
}

// Init 初始化Lua运行时
func (ls *LuaScript) Init() error {
	ls.Close()

	// 创建上下文
	parent := luaParentCtx
	if parent == nil {
		parent = context.Background()
	}
	ls.ctx, ls.cancel = context.WithCancel(parent)

	// 创建新的运行时
	ls.runtime = rt.New(os.Stdout)
	ls.thread = ls.runtime.MainThread()
	ls.env = ls.runtime.GlobalEnv()

	// 注册标准库
	lib.LoadAll(ls.runtime)()

	// 注册内置函数
	if err := registerDefaultFunctions(ls); err != nil {
		return fmt.Errorf("register default functions failed: %w", err)
	}

	return nil
}

// Close 关闭Lua运行时
func (ls *LuaScript) Close() {
	if ls.cancel != nil {
		ls.cancel()
	}

	if ls.runtime != nil {
		if ls.timer != nil {
			ls.timer.Remove()
		}
		ls.runtime = nil
		ls.thread = nil
		ls.env = nil
	}
}

// Call 调用 Lua 函数
func (ls *LuaScript) Call(funcname string, list ...interface{}) ([]interface{}, error) {
	return ls.CallWithContext(context.Background(), funcname, list...)
}

// CallWithContext 使用上下文调用 Lua 函数。
// 超时只中止等待：Lua 运行时非线程安全，后台 goroutine 不会被强杀，调用方应串行使用同一实例。
func (ls *LuaScript) CallWithContext(ctx context.Context, funcname string, list ...interface{}) ([]interface{}, error) {
	select {
	case <-ls.ctx.Done():
		return nil, fmt.Errorf("Lua script is closed")
	default:
	}

	// 从全局环境获取函数
	funcVal := ls.env.Get(rt.StringValue(funcname))
	if funcVal == rt.NilValue {
		return nil, fmt.Errorf("function not found: '%s'", funcname)
	}

	// 检查是否是函数类型
	if _, ok := funcVal.TryCallable(); !ok {
		return nil, fmt.Errorf("'%s' is not a callable function", funcname)
	}

	// 转换参数为 rt.Value
	args := make([]rt.Value, 0, len(list))
	for _, val := range list {
		args = append(args, GoToLuaValueWithContext(ctx, val))
	}

	// 调用 Lua 函数（带超时保护）
	resultChan := make(chan rt.Value, 1)
	errChan := make(chan error, 1)

	go func() {
		result, err := rt.Call1(ls.thread, funcVal, args...)
		if err != nil {
			errChan <- err
		} else {
			resultChan <- result
		}
	}()

	metrics.Lua.IncCalls(funcname)
	started := time.Now()
	select {
	case <-ctx.Done():
		metrics.Lua.IncCalls("timeout:" + funcname)
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		return nil, fmt.Errorf("call timeout: %w", ctx.Err())
	case result := <-resultChan:
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		ls.returnValues = []interface{}{LuaToGoValueWithContext(ctx, result)}
		return ls.returnValues, nil
	case err := <-errChan:
		metrics.Lua.ObserveCallLatency(funcname, time.Since(started))
		return nil, fmt.Errorf("Lua call failed: %w", err)
	}
}

// NewLuaScript 创建一个 Lua 脚本实例
func NewLuaScript(initluapath string) (*LuaScript, error) {
	return NewLuaScriptWithContext(context.Background(), initluapath)
}

// NewLuaScriptWithContext 使用上下文创建 Lua 脚本实例
func NewLuaScriptWithContext(ctx context.Context, initluapath string) (*LuaScript, error) {
	p := &LuaScript{
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initluapath,
		registeredObjects: syncmap.NewAny(),
	}

	// 初始化Lua运行时
	if err := p.Init(); err != nil {
		return nil, err
	}

	// 初始化注册的函数
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		p.runtime.SetEnvGoFunc(p.env, funcName, function, 1, false)
	}
	registeredFuncsMu.RUnlock()

	// 注册类
	registeredClassesMu.RLock()
	for class := range registeredClasses {
		if err := registerClassWithContext(ctx, class, p); err != nil {
			vars.Error("register Lua class failed: %v", err)
		}
	}
	registeredClassesMu.RUnlock()

	// 读取并编译脚本文件
	source, err := os.ReadFile(p.initScriptPath)
	if err != nil {
		return nil, fmt.Errorf("read Lua script file failed: %w", err)
	}

	chunk, err := p.runtime.CompileAndLoadLuaChunk("main", source, rt.TableValue(p.env))
	if err != nil {
		return nil, fmt.Errorf("compile Lua script failed: %w", err)
	}

	// 执行主脚本（带超时保护）
	if _, err = rt.Call1(p.thread, rt.FunctionValue(chunk)); err != nil {
		return nil, fmt.Errorf("execute Lua script failed: %w", err)
	}

	// 创建定时器
	tmr, err := localtimer.NewTimer(UpdateIntervalMs, -1, &luaTimer{})
	if err != nil {
		return nil, fmt.Errorf("create timer failed: %w", err)
	}
	p.timer = tmr.(*luaTimer)
	p.timer.luaScript = p
	p.timer.ctx = p.ctx
	localtimer.AddTimer(p.timer)

	// 加入管理列表
	instanceID := nextInstanceID.Add(1)
	luaInstancesMu.Lock()
	luaInstances[instanceID] = p
	luaInstancesMu.Unlock()
	p.UID = instanceID
	return p, nil
}
