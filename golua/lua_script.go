package lua

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/vars"

	"github.com/arnodel/golua/lib"
	rt "github.com/arnodel/golua/runtime"
)

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
)

// LuaScript Lua脚本实例
type LuaScript struct {
	// rtMu 保护 runtime/thread/env 的生命周期：CallWithContext 持读锁，
	// Init/closeKeepingObjects/ReloadScript 持写锁，确保重载与调用互斥。
	rtMu sync.RWMutex
	// poisoned 标记 runtime 已被超时协程污染：超时后协程仍在非线程安全的
	// golua runtime 上执行（不会被强杀），后续任何调用都不得再使用该 runtime。
	// Init 会重置此标记（因为创建了全新 runtime）。
	poisoned          atomic.Bool
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
	// regCtx 是创建方（NewLuaScriptWithContext / ReloadScriptWithContext）传入的上下文，
	// 注册回放与方法回调捕获它。回调今天拿的就是调用方 ctx，而不是派生自父上下文
	// 的 ls.ctx；换成 ls.ctx 会让「创建方 ctx 取消后 Lua 方法报 closed」消失，
	// 那是未经裁决的语义变更，所以单独存一份。
	regCtx context.Context
}

// Init 初始化Lua运行时
func (ls *LuaScript) Init() error {
	ls.rtMu.Lock()
	defer ls.rtMu.Unlock()
	return ls.initLocked()
}

// initLocked 内部初始化逻辑，调用方必须持有 rtMu 写锁。
func (ls *LuaScript) initLocked() error {
	// 只拆运行时，不回收对象：热重载走的就是 Init，对象登记表与 nextObjectID
	// 必须跨这次重建存活，否则重载后新 userdata 会顶掉仍在使用的旧对象
	ls.closeRuntimeLocked()

	// 创建上下文
	parent := currentLuaParentCtx()
	ls.ctx, ls.cancel = context.WithCancel(parent)

	// 创建新的运行时
	ls.runtime = rt.New(os.Stdout)
	ls.thread = ls.runtime.MainThread()
	ls.env = ls.runtime.GlobalEnv()

	// 注册标准库
	lib.LoadAll(ls.runtime)()

	// 注册内置函数
	registerDefaultFunctions(ls)

	// Init() 可以在零值实例上被调用（外部直接 &LuaScript{...} 再 Init 的用法真实存在），
	// 构造器需要登记表，这里先补建。注意不能清空已有内容：热重载靠它跨实例存活。
	if ls.registeredObjects == nil {
		ls.registeredObjects = syncmap.NewAny()
	}

	// 回放全局注册表。Init() 是 poison 恢复与重载失败回退的唯一重建入口：
	// 不在这里回放，这两条路径重建出的 runtime 就只有 5 个内置函数，
	// Lua 侧的 Npc() 与宿主函数全部消失。
	ls.replayRegistrationsLocked(ls.regContextLocked())

	// 新 runtime，清除中毒标记
	ls.poisoned.Store(false)
	return nil
}

// regContextLocked 返回注册回放与方法回调应捕获的上下文。
// 优先用创建方传入的 regCtx，缺失时退回 ls.ctx，再没有就用 Background。
func (ls *LuaScript) regContextLocked() context.Context {
	if ls.regCtx != nil {
		return ls.regCtx
	}
	if ls.ctx != nil {
		return ls.ctx
	}
	return context.Background()
}

// Close 关闭脚本实例，并回收本实例创建的全部 Go 对象。
//
// 对象生命周期与脚本实例一致，而不是跟 userdata 走 GC：golua 的 rt.NewUserData
// 不注册终结器（只有 Runtime.NewUserDataValue 才会 addFinalizer），__gc 元方法
// 永远不触发；即便改成会触发的写法也不对——配置脚本里 `local npc = Npc()` 在主块
// 返回后就没人了引用，按 GC 回收等于把刚配好的对象删掉。
func (ls *LuaScript) Close() {
	ls.closeKeepingObjects() // 内部取写锁
	ls.releaseRegisteredObjects()
	// 实例已作废：从全局登记表摘除，避免 luaInstances 长期钉住死实例
	unregisterLuaInstance(ls)
}

// unregisterLuaInstance 把实例从 luaInstances 登记表摘除。
// 只删「键仍归属本实例」的条目：UID 为 0（未经 NewLuaScriptWithContext 登记）
// 或键已被其他实例接管时不动别人的登记项。
func unregisterLuaInstance(ls *LuaScript) {
	if ls == nil || ls.UID == 0 {
		return
	}
	luaInstancesMu.Lock()
	if cur, ok := luaInstances[ls.UID]; ok && cur == ls {
		delete(luaInstances, ls.UID)
	}
	luaInstancesMu.Unlock()
}

// IsPoisoned 报告该实例的 runtime 是否已被超时协程污染。
// poisoned 实例拒绝所有新调用，需要 Init() 重建或创建新实例。
func (ls *LuaScript) IsPoisoned() bool {
	return ls.poisoned.Load()
}

// closeKeepingObjects 供热重载使用：只拆运行时，Lua 侧创建的对象要跨实例存活。
// 取写锁以阻塞正在进行的 Call，确保不会在调用中途 nil 掉 runtime。
func (ls *LuaScript) closeKeepingObjects() {
	ls.rtMu.Lock()
	defer ls.rtMu.Unlock()
	ls.closeRuntimeLocked()
}

// closeRuntimeLocked 释放上下文、update 定时器与 Lua 运行时。
// 调用方必须持有 rtMu 写锁。
func (ls *LuaScript) closeRuntimeLocked() {
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

	// 即使 poisoned（超时协程仍在跑），也安全置 nil：协程已捕获 thread 局部引用，
	// Go GC 保证其存活直到协程退出。此处只是断开本实例对旧 runtime 的引用。
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
	_, err := localtimer.NewTimer[*luaTimer](luaUpdateIntervalMsValue(), -1, func(t *luaTimer) {
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

// replayRegistrationsLocked 把全局注册表（宿主函数与类）回放到当前 runtime。
//
// 调用方必须持有 rtMu 写锁；唯一例外是 NewLuaScriptWithContext 的构造期，
// 那时实例还没登记进 luaInstances，不存在并发调用方。
//
// 绑定形状是契约，不是实现细节：
//   - 宿主函数按「1 个具名槽 + etc」绑定。注册表拿不到宿主函数的真实元数，
//     hasEtc=false 会让 golua 直接丢掉第 2 个之后的实参（见 RegisterLuaFunc 注释）。
//   - 类的元表布局约束见 registerClassWithContext。
func (ls *LuaScript) replayRegistrationsLocked(ctx context.Context) {
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		ls.runtime.SetEnvGoFunc(ls.env, funcName, function, 1, true)
	}
	registeredFuncsMu.RUnlock()

	// 反射注册的函数元数是真实的，绑定形状与上面那批不同
	ls.replayGoFuncsLocked(ctx)

	registeredClassesMu.RLock()
	for _, class := range registeredClasses {
		if err := registerClassWithContext(ctx, class, ls); err != nil {
			vars.Error("register Lua class failed: %v", err)
		}
	}
	registeredClassesMu.RUnlock()
}

// runMainChunkLocked 读取、编译并执行主脚本块。
// 调用方必须持有 rtMu 写锁（构造期例外，理由同 replayRegistrationsLocked）。
func (ls *LuaScript) runMainChunkLocked(path string) error {
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Lua script file failed: %w", err)
	}

	chunk, err := ls.runtime.CompileAndLoadLuaChunk("main", source, rt.TableValue(ls.env))
	if err != nil {
		return fmt.Errorf("compile Lua script failed: %w", err)
	}

	if _, err = rt.Call1(ls.thread, rt.FunctionValue(chunk)); err != nil {
		return fmt.Errorf("execute Lua script failed: %w", err)
	}
	return nil
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
	ls := &LuaScript{
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initluapath,
		registeredObjects: syncmap.NewAny(),
	}

	// 初始化Lua运行时（注册表回放在 initLocked 内完成，回调捕获 regCtx）
	ls.regCtx = ctx
	if err := ls.Init(); err != nil {
		return nil, err
	}

	// 读取并编译脚本文件，执行主块
	if err := ls.runMainChunkLocked(initluapath); err != nil {
		return nil, err
	}

	// 创建 update 定时器（必须在主脚本执行完之后）
	if err := ls.startTimer(); err != nil {
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
	luaInstances[instanceID] = ls
	luaInstancesMu.Unlock()
	ls.UID = instanceID
	return ls, nil
}
