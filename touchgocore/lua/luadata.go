package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	lua "github.com/yuin/gopher-lua"
)

//默认的lua指针
var defaultScript *LuaScript = newScript()

func newScript() *LuaScript {
	s := &LuaScript{
		exports: &map[string]lua.LGFunction{},
	}
	(*s.exports)["info"] = info
	(*s.exports)["debug"] = debug
	(*s.exports)["error"] = error1
	(*s.exports)["dofile"] = dofile
	return s
}

//两个默认执行的函数
func info(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Info(retstr)
	return 0
}

func debug(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Debug(retstr)
	return 0
}

func error1(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Error(retstr)
	return 0
}

func dofile(L *lua.LState) int {
	retstr := L.ToString(1)
	if err := L.DoFile(retstr); err != nil {
		L.Push(lua.LNumber(-1))
		L.Push(lua.LString(err.Error()))
	} else {
		L.Push(lua.LNumber(0))
		L.Push(lua.LString("ok"))
	}
	return 2
}
