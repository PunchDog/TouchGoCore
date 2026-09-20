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
	// nextObjectID 给每个 userdata 分配独立对象号： registeredObjects 的键必须是
	// 「对象」而不是「脚本」，否则同脚本里创建的第二个对象会覆盖第一个。
	nextObjectID atomic.Int64
	timer        *luaTimer
	UID          int64
	env          *rt.Table
	ctx          context.Context
	cancel       context.CancelFunc
}

// Init 初始化Lua运行时
func (ls *LuaScript) Init() error {
	// 只拆运行时，不回收对象：热重载走的就是 Init，对象登记表与 nextObjectID
	// 必须跨这次重建存活，否则重载后新 userdata 会顶掉仍在使用的旧对象
	ls.closeKeepingObjects()

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

// Close 关闭脚本实例，并回收本实例创建的全部 Go 对象。
//
// 对象生命周期与脚本实例一致，而不是跟 userdata 走 GC：golua 的 rt.NewUserData
// 不注册终结器（只有 Runtime.NewUserDataValue 才会 addFinalizer），__gc 元方法
// 永远不触发；即便改成会触发的写法也不对——配置脚本里 `local npc = Npc()` 在主块
// 返回后就没人了引用，按 GC 回收等于把刚配好的对象删掉。
func (ls *LuaScript) Close() {
	ls.closeRuntime()
	ls.releaseRegisteredObjects()
}

// closeKeepingObjects 供热重载使用：只拆运行时，Lua 侧创建的对象要跨实例存活。
func (ls *LuaScript) closeKeepingObjects() {
	ls.closeRuntime()
}

// closeRuntime 释放上下文、update 定时器与 Lua 运行时。
func (ls *LuaScript) closeRuntime() {
	if ls.cancel != nil {
		ls.cancel()
	}

	// 停表与 runtime 是否已置 nil 无关：Init 中途失败时 runtime 已是 nil，
	// 漏停就会留下一个仍指向本脚本、还在被时间轮驱动的定时器。
	if ls.timer != nil {
		// Remove 即彻底作废并归还对象池；随后必须置 nil，否则重复 Close 会把
		// 一个可能已易主的实例再归还一次。
		ls.timer.Remove()
		ls.timer = nil
	}

	if ls.runtime != nil {
		ls.runtime = nil
		ls.thread = nil
		ls.env = nil
	}
}

// releaseRegisteredObjects 逐个调用对象的 Delete() 后清空登记表。
// Range 是「锁内快照、锁外回调」，回调里再触碰登记表不会自锁。
func (ls *LuaScript) releaseRegisteredObjects() {
	if ls == nil || ls.registeredObjects == nil {
		return
	}
	ls.registeredObjects.Range(func(_, value interface{}) bool {
		if data, ok := value.(ILuaClassInterface); ok {
			data.Delete()
		}
		return true
	})
	ls.registeredObjects.Clear()
}

// startTimer 创建并注册脚本 update 定时器。
//
// 必须在主脚本执行完之后调用：Lua 运行时非线程安全，update 一旦跑起来就会和
// 仍在进行的注册/执行流程并发读写同一个 runtime。
func (ls *LuaScript) startTimer() error {
	// initcallback 里完成实例归属与上下文绑定：实例来自对象池，可能带着上一个
	// 脚本实例的全部状态，每个字段都要重新赋值（tick 残留会错后 GC 节拍）。
	_, err := localtimer.NewTimer[*luaTimer](UpdateIntervalMs, -1, func(t *luaTimer) {
		ls.timer = t
		t.luaScript = ls
		t.ctx = ls.ctx
		t.tick.Store(0)
	})
	if err != nil {
		return fmt.Errorf("create timer failed: %w", err)
	}
	// 没有 update 定时器，这个脚本实例永远不会被驱动，不能当成创建成功返回
	if err := localtimer.AddTimer(ls.timer); err != nil {
		return fmt.Errorf("register lua timer failed: %w", err)
	}
	return nil
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

// RangeRegisteredObjects 遍历本脚本里由 Lua 构造出来的 Go 对象。
// userdata 只把对象登记在脚本内部，Go 侧逻辑（例如地图取全部 NPC）需要这份
// 列表时只能走这里，不要去碰 registeredObjects。
func (ls *LuaScript) RangeRegisteredObjects(fn func(objectUID int64, obj any) bool) {
	if ls == nil || ls.registeredObjects == nil || fn == nil {
		return
	}
	ls.registeredObjects.Range(func(key, value interface{}) bool {
		objectUID, ok := key.(int64)
		if !ok {
			return true
		}
		return fn(objectUID, value)
	})
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
	// nArgs=1、hasEtc=true：注册表拿不到宿主函数的真实元数，按 1 个具名槽绑定后
	// 其余实参才会留在 c.Etc() 里；hasEtc=false 会让 golua 直接丢掉第 2 个之后的参数
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		p.runtime.SetEnvGoFunc(p.env, funcName, function, 1, true)
	}
	registeredFuncsMu.RUnlock()

	// 注册类
	registeredClassesMu.RLock()
	for _, class := range registeredClasses {
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

	// 创建 update 定时器（必须在主脚本执行完之后）
	if err := p.startTimer(); err != nil {
		return nil, err
	}

	// 加入管理列表
	instanceID := nextInstanceID.Add(1)
	luaInstancesMu.Lock()
	// StopLua 会把表置 nil，而 NewLuaScriptWithContext 是公开入口：
	// 不补建就是一次向 nil map 赋值的 panic
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstances[instanceID] = p
	luaInstancesMu.Unlock()
	p.UID = instanceID
	return p, nil
}
