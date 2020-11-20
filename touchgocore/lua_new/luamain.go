package lua

import (
	"fmt"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	"github.com/aarzilli/golua/lua"
)

//lua指针
var _defaultlua *LuaScript = nil

//注册用的函数
var _exports map[string]func(L *lua.State) int
var _exportsClass map[ILuaClassInterface]bool

type LuaScript struct {
	l                 *lua.State
	retList           []interface{} //返回值列表
	path              string        //脚本地址
	closeLuaClearTick chan byte
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
		this.l.Close()
		this.l = nil
	}
}

//调用
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

//创建一个lua指针
func NewLuaScript() *LuaScript {
	p := &LuaScript{}
	p.Init()
	//初始化注册的函数
	for funcname, function := range _exports {
		p.l.Register(funcname, function)
	}
	//注册类
	for i, _ := range _exportsClass {
		newLuaClass(i, p)
	}
	return p
}

//调用
func Call(funcname string, list ...interface{}) bool {
	return _defaultlua.Call(funcname, list...)
}

//注册函数列表
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
	_defaultlua = NewLuaScript()

	if err := _defaultlua.l.DoFile("./calls.lua"); err != nil {
		fmt.Println(err)
		vars.Error(err)
		return
	}
	Call("testcall", 100, []interface{}{"this is a test", 666})
}
