package lua

import (
	"fmt"
	"os"
	"runtime"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/arnodel/golua/lib"
	rt "github.com/arnodel/golua/runtime"
)

// 全局 Lua 实例管理
var defaultLua *LuaScript = nil
var luaInstances map[int64]*LuaScript = nil
var nextInstanceID int64 = 0

// 注册的函数和类
var registeredFuncs map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)
var registeredClasses map[ILuaClassInterface]bool

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
	// 定期清理 Lua 垃圾回收
	if this.tick%GCTickCount == 0 {
		// arnodel/golua 使用 Go 的 GC，不需要手动调用
		// 这里可以触发 Go 的 GC
		runtime.GC()
	}
}

type LuaScript struct {
	runtime           *rt.Runtime
	thread            *rt.Thread
	returnValues      []interface{}
	initScriptPath    string
	registeredObjects *syncmap.Map
	nextObjectID      int64
	timer             *luaTimer
	UID               int64
	env               *rt.Table // 全局环境
}

func (ls *LuaScript) Init() {
	// 关闭老的 Lua 脚本
	ls.Close()

	// 创建新的运行时
	ls.runtime = rt.New(os.Stdout)
	ls.thread = ls.runtime.MainThread()
	ls.env = ls.runtime.GlobalEnv()

	// 注册标准库
	lib.LoadAll(ls.runtime)()

	// 注册内置函数
	registerDefaultFunctions(ls)
}

func (ls *LuaScript) Close() {
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
	// 从全局环境获取函数
	funcVal := ls.env.Get(rt.StringValue(funcname))
	if funcVal == rt.NilValue {
		return nil, fmt.Errorf("函数调用错误: 找不到 Lua 函数 '%s'", funcname)
	}

	// 检查是否是函数类型
	if _, ok := funcVal.TryCallable(); !ok {
		return nil, fmt.Errorf("函数调用错误: '%s' 不是一个 Lua 函数", funcname)
	}

	// 转换参数为 rt.Value
	args := make([]rt.Value, 0, len(list))
	for _, val := range list {
		args = append(args, GoToLuaValue(val))
	}

	// 调用 Lua 函数
	ls.returnValues = make([]interface{}, 0)

	// 使用 Call1 调用（返回一个值）
	result, err := rt.Call1(ls.thread, funcVal, args...)
	if err != nil {
		return nil, fmt.Errorf("函数调用错误: Lua 调用失败: %w", err)
	}

	// 处理返回值
	ls.returnValues = append(ls.returnValues, LuaToGoValue(result))
	return ls.returnValues, nil
}

// 创建一个 Lua 脚本实例
func NewLuaScript(initluapath string) (*LuaScript, error) {
	p := &LuaScript{
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initluapath,
		registeredObjects: &syncmap.Map{},
		nextObjectID:      0,
	}
	p.Init()

	// 初始化注册的函数
	for funcName, function := range registeredFuncs {
		p.runtime.SetEnvGoFunc(p.env, funcName, function, 1, false)
	}

	// 注册类
	for class := range registeredClasses {
		if err := registerClass(class, p); err != nil {
			vars.Error("注册 Lua 类失败: %v", err)
		}
	}

	// 读取并编译脚本文件
	source, err := os.ReadFile(p.initScriptPath)
	if err != nil {
		return nil, fmt.Errorf("读取 Lua 脚本文件失败: %w", err)
	}

	chunk, err := p.runtime.CompileAndLoadLuaChunk("main", source, rt.TableValue(p.env))
	if err != nil {
		return nil, fmt.Errorf("编译 Lua 脚本失败: %w", err)
	}

	// 执行主脚本
	_, err = rt.Call1(p.thread, rt.FunctionValue(chunk))
	if err != nil {
		return nil, fmt.Errorf("执行 Lua 脚本失败: %w", err)
	}

	// 创建定时器
	tmr, err := localtimer.NewTimer(UpdateIntervalMs, -1, &luaTimer{})
	if err != nil {
		return nil, fmt.Errorf("创建定时器失败: %w", err)
	}
	p.timer = tmr.(*luaTimer)
	p.timer.luaScript = p
	p.timer.tick = 0
	localtimer.AddTimer(p.timer)

	// 加入管理列表
	nextInstanceID++
	luaInstances[nextInstanceID] = p
	p.UID = nextInstanceID
	return p, nil
}

// Call 调用默认 Lua 实例的函数
func Call(funcName string, args ...interface{}) ([]interface{}, error) {
	if defaultLua == nil {
		return nil, fmt.Errorf("Lua 服务未启动")
	}
	return defaultLua.Call(funcName, args...)
}

// RegisterLuaFunc 注册全局函数到所有 Lua 实例
func RegisterLuaFunc(funcName string, function func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)) error {
	if registeredFuncs == nil {
		registeredFuncs = make(map[string]func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error))
	}
	if _, ok := registeredFuncs[funcName]; ok {
		return fmt.Errorf("函数 %s 已注册", funcName)
	}
	registeredFuncs[funcName] = function
	return nil
}

// RegisterLuaClass 注册一个类到所有 Lua 实例
func RegisterLuaClass(class ILuaClassInterface) error {
	className, _ := util.GetClassName(class)
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

	luaInstances = make(map[int64]*LuaScript)
	var err error
	defaultLua, err = NewLuaScript(config.Cfg_.Lua)
	if err != nil {
		return fmt.Errorf("创建 Lua 脚本失败: %w", err)
	}
	vars.Info("启动 Lua 服务成功")
	return nil
}

// StopLua 关闭所有的定时器
func StopLua() {
	for _, ls := range luaInstances {
		ls.Close()
	}
}

// registerDefaultFunctions 注册默认的内置函数
func registerDefaultFunctions(ls *LuaScript) {
	ls.runtime.SetEnvGoFunc(ls.env, "info", info, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "debug", debug, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "error", error1, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "dofile", dofile, 1, false)
	ls.runtime.SetEnvGoFunc(ls.env, "getpathluafile", getpathluafile, 1, false)
}
