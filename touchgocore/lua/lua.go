package lua

import (
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/vars"
	"log"

	"github.com/yuin/gluamapper"
	lua "github.com/yuin/gopher-lua"
)

type Script struct {
	gScript   *lua.LState
	liRetList *RetList
	//注册lua
	luaPath *string
}

//返回值结果
type RetList struct {
	liRetList []*lua.LValue
}

//删除start:end之间数据
func (this *RetList) Remove(indexstart int, indexend int) {
	len := len(this.liRetList)
	copy(this.liRetList[indexstart:], this.liRetList[indexend:])
	for k, n := len-indexend+indexstart, len; k < n; k++ {
		//or the zero value of T
		this.liRetList[k] = nil
	}
	this.liRetList = this.liRetList[:len-indexend+indexstart]
}

//清空
func (this *RetList) Clear() {
	this.liRetList = []*lua.LValue{}
}

//插入一个数
func (this *RetList) Push(data *lua.LValue) {
	this.liRetList = append(this.liRetList, data)
}

//获取一个数据
func (this *RetList) GetData(index int) *lua.LValue {
	if index >= len(this.liRetList) {
		return nil
	}
	return this.liRetList[index]
}

//注册lua回调
var exports = map[string]lua.LGFunction{
	"println": println,
}

func println(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Info(retstr)
	return 0
}

//注册函数列表
func RegisterLuaFunc(funcname string, function lua.LGFunction) bool {
	if exports[funcname] != nil {
		return false
	}
	exports[funcname] = function
	return true
}

//func Loader(L *lua.LState) int {
//// register functions to the table
//mod := L.SetFuncs(L.NewTable(), exports)
//// register other stuff
//L.SetField(mod, "name", lua.LString("value"))
//// returns the module
//L.Push(mod)
//return 1
//}

//初始化lua文件
func (this *Script) InitLua() {
	if this.gScript == nil {
		this.gScript = lua.NewState()
		this.liRetList = new(RetList)
	}

	//注册函数
	for funcname, function := range exports {
		this.gScript.SetGlobal(funcname, this.gScript.NewFunction(function)) /* Original lua_setglobal uses stack... */
	}
}

func (this *Script) CloseLua() {
	this.gScript.Close()
}

//读lua文件
func (this *Script) LoadLua(path string) error {
	if err := this.gScript.DoFile(path); err != nil {
		vars.Error(err)
		return err
	}

	vars.Info("lua load ok:", path)
	return nil
}

//获取的返回数据
func (this *Script) GetRet(index int) *lua.LValue {
	return this.liRetList.GetData(index)
}

//call lua
func (this *Script) Call(funcname string, list ...interface{}) bool {
	listlen := len(list)
	fn := lua.P{
		Fn:      this.gScript.GetGlobal(funcname),
		NRet:    lua.MultRet,
		Protect: true,
	}

	stackPos := this.gScript.GetTop()
	var err error = nil

	this.gScript.Push(fn.Fn)
	for _, val := range list {
		var arg lua.LValue = nil
		switch v := val.(type) {
		case int:
			arg = lua.LNumber(v)
		case int8:
			arg = lua.LNumber(v)
		case int16:
			arg = lua.LNumber(v)
		case int32:
			arg = lua.LNumber(v)
		case int64:
			arg = lua.LNumber(v)
		case string:
			arg = lua.LString(v)
		case bool:
			arg = lua.LBool(v)
		case []int:
			tbl := this.gScript.NewTable()
			for idx, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(idx), lua.LNumber(val))
			}
			arg = tbl
		case []string:
			tbl := this.gScript.NewTable()
			for idx, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(idx), lua.LString(val))
			}
			arg = tbl
		case map[int]string:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(key), lua.LString(val))
			}
			arg = tbl
		case map[string]string:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LString(key), lua.LString(val))
			}
			arg = tbl
		case map[string]int:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LString(key), lua.LNumber(val))
			}
			arg = tbl
		case map[int]int:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(key), lua.LNumber(val))
			}
			arg = tbl
		}
		this.gScript.Push(arg)
	}

	if fn.Protect {
		err = this.gScript.PCall(listlen, fn.NRet, fn.Handler)
	}
	if err != nil {
		log.Println("lua调用出错:", err)
		return false
	}

	nNum := this.gScript.GetTop() - stackPos
	//获取返回数据
	this.liRetList.Clear()
	for i := 0; i < nNum; i++ {
		var data lua.LValue = this.gScript.Get(-1) // returned value
		this.gScript.Pop(1)                        // remove received value
		this.liRetList.Push(&data)
	}
	return true
}

//注册lua功能
func (this *Script) RegisterLuaFile(path string, isFirst bool) {
	if isFirst {
		this.luaPath = &path
		this.InitLua()
	}

	if err := this.LoadLua(path + "/init.lua"); err == nil {
		for index := 1; ; index++ {
			if this.Call("GetFileName", index) {
				pathname := gluamapper.ToGoValue(*this.GetRet(0), gluamapper.Option{}).(string)
				if pathname != "end" {
					this.LoadLua(path + "/" + pathname)
				} else {
					break
				}
			}
		}
	}
}

//重载脚本
func (this *Script) ReLoadLua() {
	this.CloseLua()
	if this.luaPath != nil {
		this.RegisterLuaFile(*this.luaPath, true)
	}
}

var defaultScript *Script = &Script{}

//启动lua
func Run() {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动lua服务")
		return
	}

	defaultScript.RegisterLuaFile(config.Cfg_.Lua, true)
	vars.Info("启动lua服务成功")
}
