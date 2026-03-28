package lua

import (
	"fmt"
	"sync"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/vars"

	"github.com/aarzilli/golua/lua"
)

// 全局 Lua 管理器（单例，替代分散的全局变量）
var globalManager = NewLuaManager()

// 向后兼容的全局函数（包装管理器调用）
// 注意：以下变量已废弃，仅用于保持编译兼容性
var (
	// 保持向后兼容的默认实例引用
	defaultLua *LuaScript // 已废弃，通过管理器访问

	// 已废弃的全局变量，保持编译但不再使用
	luaInstances        sync.Map                          // 已废弃
	nextInstanceID      int64                             // 已废弃
	instanceIDMu        sync.Mutex                        // 已废弃
	registeredFuncsMu   sync.RWMutex                      // 已废弃
	registeredFuncs     map[string]func(L *lua.State) int // 已废弃
	registeredClassesMu sync.RWMutex                      // 已废弃
	registeredClasses   map[ILuaClassInterface]bool       // 已废弃
)

type luaTimer struct {
	localtimer.TimerInterface
	tick      int64
	luaScript *LuaScript
}

func (this *luaTimer) Tick() {
	this.tick++
	this.luaScript.registeredObjects.Range(func(key, value interface{}) bool {
		obj := value.(ILuaClassInterface)
		obj.Update()
		return true
	})
	// 定期清理 Lua 缓存
	if this.tick%GCTickCount == 0 {
		this.luaScript.Call("collectgarbage", "collect")
	}
}

type LuaScript struct {
	state             *lua.State
	returnValues      []interface{} // 返回值列表
	initScriptPath    string        // 初始化脚本地址
	registeredObjects *syncmap.Map  // 注册的 Go 对象
	nextObjectID      int64         // 下一个对象 ID
	timer             *luaTimer     // 定时器
	UID               int64         // 脚本实例 ID
	manager           *LuaManager   // 反向引用管理器（可选，用于扩展功能）
}

func (ls *LuaScript) Init() {
	// 关闭老的 Lua 脚本
	ls.Close()
	// 新创建 Lua 状态机
	ls.state = lua.NewState()
	ls.state.OpenLibs()

	// 注册内置函数
	ls.state.Register("info", info)
	ls.state.Register("debug", debug)
	ls.state.Register("error", error1)
	ls.state.Register("dofile", dofile)
	ls.state.Register("getpathluafile", getpathluafile)
}

func (ls *LuaScript) Close() {
	if ls.state != nil {
		if ls.timer != nil {
			ls.timer.Remove()
		}
		ls.state.Close()
		ls.state = nil
	}
}

// Call 调用 Lua 函数
func (ls *LuaScript) Call(funcname string, list ...interface{}) ([]interface{}, error) {
	var nargs int = 0
	// 设置函数名
	ls.state.GetGlobal(funcname)

	// 检查函数是否存在
	if ls.state.IsNil(-1) {
		ls.state.Pop(1)
		return nil, fmt.Errorf("函数调用错误: 找不到 Lua 函数 '%s'", funcname)
	}

	// 检查是否是函数类型
	if ls.state.Type(-1) != lua.LUA_TFUNCTION {
		ls.state.Pop(1)
		return nil, fmt.Errorf("函数调用错误: '%s' 不是一个 Lua 函数", funcname)
	}

	// 压参数
	for i, val := range list {
		if !PushValue(ls.state, val) {
			vars.Error("调用函数 %s 出错，压参数 %d 出错", funcname, i+1)
			ls.state.Pop(1) // 弹出函数
			return nil, fmt.Errorf("函数调用错误: 压入参数 %d 失败", i+1)
		}
		nargs++
	}

	// 调用 Lua 函数
	ls.returnValues = make([]interface{}, 0)
	if err := ls.state.Call(nargs, -1); err != nil {
		return nil, fmt.Errorf("函数调用错误: Lua 调用失败: %w", err)
	}

	// 写返回值
	nNum := ls.state.GetTop()
	for i := 1; i <= nNum; i++ {
		ls.returnValues = append(ls.returnValues, LuaToGoValue(ls.state, i))
	}
	return ls.returnValues, nil
}

// NewLuaScript 创建一个 Lua 脚本实例（已废弃，请使用 LuaManager.NewScript）
// 注意：此函数仍使用旧的全局变量模式，仅用于向后兼容
// 新代码应使用 globalManager.NewScript() 或创建自定义管理器
func NewLuaScript(initluapath string) (*LuaScript, error) {
	return globalManager.NewScript(initluapath)
}

// Call 调用默认 Lua 实例的函数
func Call(funcName string, args ...interface{}) ([]interface{}, error) {
	return globalManager.Call(funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例（线程安全）
// 注意：已迁移到 LuaManager，此函数为兼容性包装
func RegisterLuaFunc(funcName string, function func(L *lua.State) int) error {
	return globalManager.RegisterFunc(funcName, function)
}

// RegisterLuaClass 注册一个类到所有 Lua 实例（线程安全）
// 注意：已迁移到 LuaManager，此函数为兼容性包装
func RegisterLuaClass(class ILuaClassInterface) error {
	return globalManager.RegisterClass(class)
}

// RunLua 启动 Lua 服务
// 注意：此函数仍使用旧的全局变量模式，建议迁移到 LuaManager.NewScript
func RunLua() error {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动 Lua 服务")
		return nil
	}

	// 使用管理器创建脚本实例
	script, err := globalManager.NewScript(config.Cfg_.Lua)
	if err != nil {
		return fmt.Errorf("创建 Lua 脚本失败: %w", err)
	}

	// 保持向后兼容：设置默认实例引用
	// 注意：defaultLua 已标记为废弃，实际通过管理器访问
	defaultLua = script

	vars.Info("启动 Lua 服务成功")
	return nil
}

// StopLua 停止所有 Lua 实例
func StopLua() {
	globalManager.CloseAll()
}
