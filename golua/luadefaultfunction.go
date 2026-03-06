package lua

import (
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/aarzilli/golua/lua"
)

// Lua 内置函数：日志输出
func info(L *lua.State) int {
	msg := L.ToString(1)
	vars.Info("%s", msg)
	return 0
}

func debug(L *lua.State) int {
	msg := L.ToString(1)
	vars.Debug("%s", msg)
	return 0
}

func error1(L *lua.State) int {
	msg := L.ToString(1)
	vars.Error("%s", msg)
	return 0
}

func dofile(L *lua.State) int {
	filePath := L.ToString(1)
	if err := L.DoFile(filePath); err != nil {
		PushValue(L, 0)
		PushValue(L, err.Error())
	} else {
		PushValue(L, 1)
		PushValue(L, "ok")
	}
	return 2
}

// GetLuaFiles 获取指定路径下所有 Lua 文件
func getpathluafile(L *lua.State) int {
	path := L.ToString(1)
	fileList := util.GetPathFile(path, []string{LuaFileExt})

	// 返回所有文件
	tbl := newTable(fileList)
	tbl.PushTable(L)
	return 1
}
