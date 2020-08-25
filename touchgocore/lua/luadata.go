package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	lua "github.com/yuin/gopher-lua"
)

//默认的lua指针
var defaultScript *LuaScript = newScript()

//lua产生的类数据
var defaultLuaDataUid int64 = 0
var defaultLuaData *syncmap.Map = &syncmap.Map{}

func newScript() *LuaScript {
	return &LuaScript{
		exports: &map[string]lua.LGFunction{
			"info":   info,
			"debug":  debug,
			"error":  error1,
			"dofile": dofile,
		},
		exportsClass:      nil,
		closeLuaClearTick: make(chan byte, 1),
	}
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
	// defer func() {
	// 	if err := recover(); err != nil {
	// 		log.Println("捕获错误:", err)
	// 	}
	// }()
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
