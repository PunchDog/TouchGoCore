package lua

import "touchgocore/config"

// Lua 定时器和垃圾回收配置
var (
	// UpdateIntervalMs 更新间隔（毫秒），可从配置读取
	UpdateIntervalMs = getUpdateIntervalMs()
	// GCTickCount 垃圾回收的 tick 计数，可从配置读取
	GCTickCount = getGCTickCount()
)

// getUpdateIntervalMs 从配置获取更新间隔
func getUpdateIntervalMs() int64 {
	if config.Cfg_ != nil && config.Cfg_.LuaConfig != nil && config.Cfg_.LuaConfig.UpdateInterval > 0 {
		return config.Cfg_.LuaConfig.UpdateInterval
	}
	return 1000 // 默认 1 秒
}

// getGCTickCount 从配置获取 GC tick 计数
func getGCTickCount() int64 {
	if config.Cfg_ != nil && config.Cfg_.LuaConfig != nil && config.Cfg_.LuaConfig.GCTickCount > 0 {
		return config.Cfg_.LuaConfig.GCTickCount
	}
	return 1800 // 默认 30 分钟 (30 * 60)
}

// Lua 文件扩展名
const (
	LuaFileExt = ".lua"
)
