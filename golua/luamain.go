package lua

import (
	"fmt"
	"sync"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/aarzilli/golua/lua"
)

// 全局 Lua 实例管理（sync.Map 保证线程安全）
var defaultLua *LuaScript = nil
var luaInstances sync.Map // map[int64]*LuaScript
var nextInstanceID int64 = 0
var instanceIDMu sync.Mutex // 保护 nextInstanceID

// 注册的函数和类（读写锁保护）
var (
	registeredFuncsMu  sync.RWMutex
	registeredFuncs    map[string]func(L *lua.State) int

	registeredClassesMu sync.RWMutex
	registeredClasses   map[ILuaClassInterface]bool
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

// 创建一个lua指针
func NewLuaScript(initluapath string) (*LuaScript, error) {
	p := &LuaScript{
		state:             nil,
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initluapath,
		registeredObjects: &syncmap.Map{},
		nextObjectID:      0,
	}
	p.Init()

	// 初始化注册的函数（持读锁遍历）
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		p.state.Register(funcName, function)
	}
	registeredFuncsMu.RUnlock()

	// 注册类（持读锁遍历）
	registeredClassesMu.RLock()
	for class := range registeredClasses {
		if err := registerClass(class, p); err != nil {
			vars.Error("注册 Lua 类失败: %v", err)
		}
	}
	registeredClassesMu.RUnlock()

	// 读取脚本文件
	if err := p.state.DoFile(p.initScriptPath); err != nil {
		return nil, fmt.Errorf("加载 Lua 脚本失败: %w", err)
	}

	// 创建定时器
	timerImpl := &luaTimer{}
	tmr, err := localtimer.NewTimer(UpdateIntervalMs, -1, timerImpl)
	if err != nil {
		return nil, fmt.Errorf("创建定时器失败: %w", err)
	}
	p.timer = tmr.(*luaTimer)
	p.timer.luaScript = p
	p.timer.tick = 0
	localtimer.AddTimer(p.timer)

	// 加入管理列表（原子递增 ID，sync.Map 存储）
	instanceIDMu.Lock()
	nextInstanceID++
	id := nextInstanceID
	instanceIDMu.Unlock()

	luaInstances.Store(id, p)
	p.UID = id
	return p, nil
}

// Call 调用默认 Lua 实例的函数
func Call(funcName string, args ...interface{}) ([]interface{}, error) {
	if defaultLua == nil {
		return nil, fmt.Errorf("Lua 服务未启动")
	}
	return defaultLua.Call(funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例（线程安全）
func RegisterLuaFunc(funcName string, function func(L *lua.State) int) error {
	registeredFuncsMu.Lock()
	defer registeredFuncsMu.Unlock()
	if registeredFuncs == nil {
		registeredFuncs = make(map[string]func(L *lua.State) int)
	}
	if _, ok := registeredFuncs[funcName]; ok {
		return fmt.Errorf("函数 %s 已注册", funcName)
	}
	registeredFuncs[funcName] = function
	return nil
}

// RegisterLuaClass 注册一个类到所有 Lua 实例（线程安全）
func RegisterLuaClass(class ILuaClassInterface) error {
	className, _ := util.GetClassName(class)
	registeredClassesMu.Lock()
	defer registeredClassesMu.Unlock()
	if registeredClasses == nil {
		registeredClasses = make(map[ILuaClassInterface]bool)
	}
	if _, ok := registeredClasses[class]; ok {
		return fmt.Errorf("类 %s 已注册", className)
	}
	registeredClasses[class] = true
	return nil
}

// RunLua 启动 Lua 服务
func RunLua() error {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动 Lua 服务")
		return nil
	}

	// luaInstances 为 sync.Map，无需 make
	var err error
	defaultLua, err = NewLuaScript(config.Cfg_.Lua)
	if err != nil {
		return fmt.Errorf("创建 Lua 脚本失败: %w", err)
	}
	vars.Info("启动 Lua 服务成功")
	return nil
}

// 关闭所有的定时器
func StopLua() {
	luaInstances.Range(func(_, v interface{}) bool {
		if ls, ok := v.(*LuaScript); ok {
			ls.Close()
		}
		return true
	})
}

func init() {
	util.DefaultCallFunc.Register("RunLua", RunLua)
	util.DefaultCallFunc.Register("StopLua", StopLua)
}
