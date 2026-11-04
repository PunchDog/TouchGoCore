package lua

import (
	"context"
	"sync"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
)

// ==================== Lua 启动期配置 ====================
//
// update 节拍与 GC 节拍来自 lua_config 段（update_interval / gc_tick_count）。

const (
	defaultLuaUpdateIntervalMs int64 = 1000 // 默认 1 秒
	defaultLuaGCTickCount      int64 = 1800 // 默认 30 分钟 (30 * 60)
)

// UpdateIntervalMs 与 GCTickCount 是启动期写入的兼容镜像。
//
// 它们此前在包级 var 声明处就调 getUpdateIntervalMs() 求值，而那次求值发生在
// Run() 写入父上下文之前——Go 的包级初始化早于任何函数调用，于是
// luaCfg() 恒为 nil，配置里的 update_interval / gc_tick_count 永不生效。
// 现在这里只落默认值，真实值由 applyLuaTimingConfig 在启动期写入。
// 运行期请读 luaUpdateIntervalMsValue / luaGCTickCountValue，不要读这两个 var。
var (
	UpdateIntervalMs int64 = defaultLuaUpdateIntervalMs
	GCTickCount      int64 = defaultLuaGCTickCount
)

var (
	// 权威值：只有 applyLuaTimingConfig 写，消费点一律经 getter 读
	luaUpdateIntervalMs atomic.Int64
	luaGCTickCount      atomic.Int64
	luaTimingMu         sync.Mutex // 串行化「读配置 → 写权威值 + 写镜像 var」

	luaParentCtxValue atomic.Pointer[context.Context]
)

// currentLuaParentCtx 与 setLuaParentCtx 是父上下文的唯一读写入口。
// 裸赋值是一次数据竞争：Run() 写它，而 confineLuaPath 跑在 Lua 调用栈里读它。
func currentLuaParentCtx() context.Context {
	if p := luaParentCtxValue.Load(); p != nil && *p != nil {
		return *p
	}
	return context.Background()
}

func setLuaParentCtx(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	luaParentCtxValue.Store(&ctx)
}

// luaCfg 返回启动配置里的 Lua 段；未加载配置时返回 nil。
func luaCfg() *config.LuaConfig {
	cfg := corectx.CfgFrom(currentLuaParentCtx())
	if cfg == nil {
		return nil
	}
	return cfg.LuaConfig
}

// resolveUpdateIntervalMs 从配置解析更新间隔，缺配回默认。
func resolveUpdateIntervalMs() int64 {
	if lc := luaCfg(); lc != nil && lc.UpdateInterval > 0 {
		return lc.UpdateInterval
	}
	return defaultLuaUpdateIntervalMs
}

// resolveGCTickCount 从配置解析 GC 节拍，缺配回默认。
func resolveGCTickCount() int64 {
	if lc := luaCfg(); lc != nil && lc.GCTickCount > 0 {
		return lc.GCTickCount
	}
	return defaultLuaGCTickCount
}

// applyLuaTimingConfig 是节拍唯一写者：把解析结果落到权威值，并镜像进两个导出 var。
// 必须在 setLuaParentCtx 之后调用，否则读到的还是上一份配置。
func applyLuaTimingConfig() {
	interval := resolveUpdateIntervalMs()
	gcTicks := resolveGCTickCount()

	luaTimingMu.Lock()
	defer luaTimingMu.Unlock()
	luaUpdateIntervalMs.Store(interval)
	luaGCTickCount.Store(gcTicks)
	UpdateIntervalMs = interval
	GCTickCount = gcTicks
}

// luaUpdateIntervalMsValue 返回当前生效的 update 间隔（毫秒）。
func luaUpdateIntervalMsValue() int64 {
	if v := luaUpdateIntervalMs.Load(); v > 0 {
		return v
	}
	return defaultLuaUpdateIntervalMs
}

// luaGCTickCountValue 返回当前生效的 GC 节拍（每多少次 tick 触发一次 runtime.GC）。
func luaGCTickCountValue() int64 {
	if v := luaGCTickCount.Load(); v > 0 {
		return v
	}
	return defaultLuaGCTickCount
}
