package lua

import (
	"fmt"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/aarzilli/golua/lua"
)

// lua指针
var _defaultlua *LuaScript = nil
var _luaList map[int64]*LuaScript = nil
var _LuaListUid int64 = 0

// 注册用的函数
var _exports map[string]func(L *lua.State) int
var _exportsClass map[ILuaClassInterface]bool

type luaTimer struct {
	util.Timer
	tick      int64
	luaScript *LuaScript
}

func (this *luaTimer) Tick() {
	this.tick++
	this.luaScript.defaultLuaData.Range(func(key, value interface{}) bool {
		lua := value.(ILuaClassInterface)
		lua.Update()
		return true
	})
	//30分钟清理一次lua缓存
	if this.tick%1800 == 0 {
		//定时垃圾回收
		this.luaScript.Call("collectgarbage", "collect")
	}
}

type LuaScript struct {
	l                 *lua.State
	retList           []interface{} //返回值列表
	initluapath       string        //初始化脚本地址
	defaultLuaData    *syncmap.Map
	defaultLuaDataUid int64
	luaTimer          *luaTimer
	Uid               int64
}

func (this *LuaScript) Init() {
	//关闭老的lua脚本
	this.Close()
	//新创建lua脚本指针
	this.l = lua.NewState()
	this.l.OpenLibs()

	//初始化几个主要函数
	this.l.Register("info", info)
	this.l.Register("debug", debug)
	this.l.Register("error", error1)
	this.l.Register("dofile", dofile)
	this.l.Register("getpathluafile", getpathluafile)
}

func (this *LuaScript) Close() {
	if this.l != nil {
		this.luaTimer.Remove()
		this.l.Close()
		this.l = nil
	}
}

// 调用
func (this *LuaScript) Call(funcname string, list ...interface{}) bool {
	var nargs int = 0
	//设置函数名
	this.l.GetGlobal(funcname)
	//压参数
	for _, val := range list {
		if !push(this.l, val) {
			vars.Error("调用函数%s出错，压参数出错")
			return false
		}

		nargs++
	}
	//调用lua函数
	this.retList = make([]interface{}, 0)
	if err := this.l.Call(nargs, -1); err != nil {
		vars.Error("lua call fail:", err)
		return false
	}

	//写返回值
	nNum := this.l.GetTop()
	for i := 1; i <= nNum; i++ {
		this.retList = append(this.retList, pop(this.l, i))
	}
	fmt.Println(this.retList)
	return true
}

// 创建一个lua指针
func NewLuaScript(initluapath string) *LuaScript {
	p := &LuaScript{
		l:                 nil,
		retList:           make([]interface{}, 0),
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
	p.luaTimer = util.NewTimer(1000, -1, &luaTimer{}).(*luaTimer)
	p.luaTimer.luaScript = p
	p.luaTimer.tick = 0
	util.AddTimer(p.luaTimer)

	//加入管理列表
	_LuaListUid++
	_luaList[_LuaListUid] = p
	return p
}

// 调用
func Call(funcname string, list ...interface{}) bool {
	return _defaultlua.Call(funcname, list...)
}

// 注册函数列表
func RegisterLuaFunc(funcname string, function func(L *lua.State) int) bool {
	if _exports == nil {
		_exports = make(map[string]func(L *lua.State) int)
	}
	if _, ok := _exports[funcname]; ok {
		return false
	}
	_exports[funcname] = function
	return true
}

// 注册一个类到默认lua
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

// 启动函数
func Run() {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动lua服务")
		return
	}

	_luaList = make(map[int64]*LuaScript)
	_defaultlua = NewLuaScript(config.Cfg_.Lua)
	vars.Info("启动lua服务成功")
}

// 关闭所有的定时器
func Stop() {
	for _, lua := range _luaList {
		lua.Close()
	}
}
