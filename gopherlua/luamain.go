package gopherlua

import (
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/time"
	"touchgocore/vars"

	lua "github.com/yuin/gopher-lua"
)

//lua指针
var _defaultlua *LuaScript = nil
var _luaList []*LuaScript = make([]*LuaScript, 0)
var _luaTimerTick chan func()

//注册用的函数
var _exports map[string]func(L *lua.LState) int
var _exportsClass map[ILuaClassInterface]bool

type luaTimer struct {
	time.TimerObj
	tick      int64
	luaScript *LuaScript
}

func (this *luaTimer) Tick() {
	this.tick++
	this.luaScript.defaultLuaData.Range(func(key, value interface{}) bool {
		lua := value.(ILuaClassInterface)
		_luaTimerTick <- lua.Update
		return true
	})
	//30分钟清理一次lua缓存
	if this.tick%1800 == 0 {
		//定时垃圾回收
		this.luaScript.Call("collectgarbage", "collect")
	}
}

type LuaScript struct {
	l                 *lua.LState
	liRetList         []*lua.LValue //返回值列表
	initluapath       string        //初始化脚本地址
	defaultLuaData    *syncmap.Map
	defaultLuaDataUid int64
	luaTimer          *luaTimer
}

func (this *LuaScript) Init() {
	//关闭老的lua脚本
	this.Close()
	//新创建lua脚本指针
	this.l = lua.NewState()
	this.l.OpenLibs()

	//初始化几个主要函数
	this.l.SetGlobal("info", this.l.NewFunction(info)) /* Original lua_setglobal uses stack... */
	this.l.SetGlobal("debug", this.l.NewFunction(debug))
	this.l.SetGlobal("error", this.l.NewFunction(error1))
	// this.l.SetGlobal("dofile", this.l.NewFunction(dofile))
	this.l.SetGlobal("getpathluafile", this.l.NewFunction(getpathluafile))
}

func (this *LuaScript) Close() {
	if this.l != nil {
		this.luaTimer.Delete()
		this.l.Close()
		this.l = nil
	}
}

//call lua
func (this *LuaScript) Call(funcname string, list ...interface{}) bool {
	listlen := len(list)
	fn := lua.P{
		Fn:      this.l.GetGlobal(funcname),
		NRet:    lua.MultRet,
		Protect: true,
	}

	stackPos := this.l.GetTop()
	var err error = nil

	this.l.Push(fn.Fn)
	for _, val := range list {
		this.l.Push(push(val, this.l))
	}

	if fn.Protect {
		err = this.l.PCall(listlen, fn.NRet, fn.Handler)
	}
	if err != nil {
		vars.Error("lua调用出错:", err)
		return false
	}

	nNum := this.l.GetTop() - stackPos
	//获取返回数据
	this.liRetList = make([]*lua.LValue, 0)
	for i := 0; i < nNum; i++ {
		data := this.l.Get(-1) // returned value
		this.l.Pop(1)          // remove received value
		this.liRetList = append(this.liRetList, &data)
	}
	return true
}

//创建一个lua指针
func NewLuaScript(initluapath string) *LuaScript {
	p := &LuaScript{
		l:                 nil,
		liRetList:         make([]*lua.LValue, 0),
		initluapath:       initluapath,
		defaultLuaData:    &syncmap.Map{},
		defaultLuaDataUid: 0,
	}
	p.Init()
	//初始化注册的函数
	for funcname, function := range _exports {
		p.l.Register(funcname, function)
	}
	//注册类
	for i, _ := range _exportsClass {
		newLuaClass(i, p)
	}

	//读取脚本文件
	if err := p.l.DoFile(initluapath); err != nil {
		panic(err)
	}

	//创建定时器
	p.luaTimer = &luaTimer{
		tick:      0,
		luaScript: p,
	}
	p.luaTimer.Init(1000)
	time.AddTimer(p.luaTimer)

	//加入管理列表
	_luaList = append(_luaList, p)
	return p
}

//调用
func Call(funcname string, list ...interface{}) bool {
	return _defaultlua.Call(funcname, list...)
}

//注册函数列表
func RegisterLuaFunc(funcname string, function func(L *lua.LState) int) bool {
	if _exports == nil {
		_exports = make(map[string]func(L *lua.LState) int)
	}
	if _, ok := _exports[funcname]; ok {
		return false
	}
	_exports[funcname] = function
	return true
}

//注册一个类到默认lua
func RegisterLuaClass(class ILuaClassInterface) bool {
	//初始化一个类初始化
	if _exportsClass == nil {
		_exportsClass = make(map[ILuaClassInterface]bool)
	}
	if _, ok := _exportsClass[class]; ok {
		return false
	}
	_exportsClass[class] = true
	return true
}

//启动函数
func Run() {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动lua服务")
		return
	}

	_defaultlua = NewLuaScript(config.Cfg_.Lua)
	_luaTimerTick = make(chan func(), 100000)
	vars.Info("启动lua服务成功")
}

//关闭所有的定时器
func Stop() {
	for _, lua := range _luaList {
		lua.Close()
	}
}

//lua时间操作
func TimeTick() chan bool {
	select {
	case fn := <-_luaTimerTick:
		fn()
	default:
	}
	return nil
}