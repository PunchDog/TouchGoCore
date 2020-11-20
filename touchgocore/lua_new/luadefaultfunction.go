package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	"github.com/aarzilli/golua/lua"
)

//两个默认执行的函数
func info(L *lua.State) int {
	retstr := L.ToString(1)
	vars.Info(retstr)
	return 0
}

func debug(L *lua.State) int {
	retstr := L.ToString(1)
	vars.Debug(retstr)
	return 0
}

func error1(L *lua.State) int {
	retstr := L.ToString(1)
	vars.Error(retstr)
	return 0
}

func dofile(L *lua.State) int {
	retstr := L.ToString(1)
	if err := L.DoFile(retstr); err != nil {
		push(L, -1)
		push(L, err.Error())
	} else {
		push(L, 0)
		push(L, "ok")
	}
	return 2
}

//获取路径下所有文件
func getpathluafile(L *lua.State) int {
	path := L.ToString(1)
	pathlist := util.GetPathFile(path, []string{".lua"})

	//返回所有文件
	tbl := newTable(pathlist)
	tbl.PushTable(L)
	return 1
}

//转换压数据
func push(L *lua.State, val interface{}) bool {
	switch val.(type) {
	case string:
		L.PushString(val.(string))
	case int8:
		L.PushInteger(int64(val.(int8)))
	case uint8:
		L.PushInteger(int64(val.(uint8)))
	case int16:
		L.PushInteger(int64(val.(int16)))
	case uint16:
		L.PushInteger(int64(val.(uint16)))
	case int32:
		L.PushInteger(int64(val.(int32)))
	case uint32:
		L.PushInteger(int64(val.(uint32)))
	case int64:
		L.PushInteger(int64(val.(int64)))
	case uint64:
		L.PushInteger(int64(val.(uint64)))
	case int:
		L.PushInteger(int64(val.(int)))
	case uint:
		L.PushInteger(int64(val.(uint)))
	case bool:
		L.PushBoolean(val.(bool))
	case float32:
		L.PushNumber(float64(val.(float32)))
	case float64:
		L.PushNumber(val.(float64))
	case *LuaTable:
		tbl := val.(*LuaTable)
		return tbl.PushTable(L)
	default:
		//有可能是list的，需要转一下table试试
		tbl := newTable(val)
		if tbl.HaveData() {
			return tbl.PushTable(L)
		}
		return false
	}
	return true
}

//获取数据函数
func pop(L *lua.State, idx int) interface{} {
	var ret interface{} = nil
	//根据数据类型转换
	switch L.Type(idx) {
	case lua.LUA_TBOOLEAN:
		ret = L.ToBoolean(idx)
	case lua.LUA_TSTRING:
		ret = L.ToString(idx)
	case lua.LUA_TNUMBER:
		ret = L.ToNumber(idx)
	case lua.LUA_TTABLE:
		tbl := getTable(L, idx)
		ret = tbl
	}
	return ret
}
