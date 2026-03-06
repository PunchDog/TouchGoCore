package lua

// Lua 定时器和垃圾回收配置常量
const (
	UpdateIntervalMs    = 1000 // 更新间隔（毫秒）
	GCCollectionMinutes = 30   // 垃圾回收间隔（分钟）
	GCTickCount         = 1800 // 垃圾回收的 tick 计数 (30 * 60)
)

// Lua 文件扩展名
const (
	LuaFileExt = ".lua"
)
