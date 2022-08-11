package gopherlua

import (
	"reflect"
	"strconv"
	"strings"
	"touchgocore/util"
	"touchgocore/vars"

	lua "github.com/yuin/gopher-lua"
)

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

// func dofile(L *lua.LState) int {
// 	retstr := L.ToString(1)
// 	if err := L.DoFile(retstr); err != nil {
// 		L.Push(push(-1, L))
// 		L.Push(push(err.Error(), L))
// 	} else {
// 		L.Push(push(0, L))
// 		L.Push(push("ok", L))
// 	}
// 	return 2
// }

//获取路径下所有文件
func getpathluafile(L *lua.LState) int {
	path := L.ToString(1)
	pathlist := util.GetPathFile(path, []string{".lua"})

	//返回所有文件
	tbl := newTable(pathlist)
	tbl.PushTable(L)
	return 1
}

//转换压数据
func push(val interface{}, l *lua.LState) (arg lua.LValue) {
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
	case uint:
		arg = lua.LNumber(v)
	case uint8:
		arg = lua.LNumber(v)
	case uint16:
		arg = lua.LNumber(v)
	case uint32:
		arg = lua.LNumber(v)
	case uint64:
		arg = lua.LNumber(v)
	case string:
		arg = lua.LString(v)
	case bool:
		arg = lua.LBool(v)
	case float32:
		arg = lua.LNumber(v)
	case float64:
		arg = lua.LNumber(v)
	default:
		//有可能是list的，需要转一下table试试
		tbl := newTable(val)
		if tbl.HaveData() {
			arg = tbl.PushTable(l)
		}
	}
	return
}

func toInt64(val lua.LValue) int64 {
	if lv, ok := val.(lua.LNumber); ok {
		return int64(lv)
	}
	if lv, ok := val.(lua.LString); ok {
		if num, err := parseNumber(string(lv)); err == nil {
			return int64(num)
		}
	}
	return 0
}

func parseNumber(number string) (lua.LNumber, error) {
	var value lua.LNumber
	number = strings.Trim(number, " \t\n")
	if v, err := strconv.ParseInt(number, 0, lua.LNumberBit); err != nil {
		if v2, err2 := strconv.ParseFloat(number, lua.LNumberBit); err2 != nil {
			return lua.LNumber(0), err2
		} else {
			value = lua.LNumber(v2)
		}
	} else {
		value = lua.LNumber(v)
	}
	return value, nil
}

func toNumber(val lua.LValue) float64 {
	return float64(lua.LVAsNumber(val))
}

//逆向获取数据函数
func pop(val lua.LValue, kind reflect.Kind) interface{} {
	var ret interface{} = nil
	switch val.Type() {
	case lua.LTNumber:
		switch kind {
		case reflect.Int:
			ret = toInt64(val)
		case reflect.Int8:
			ret = int8(toInt64(val))
		case reflect.Int16:
			ret = int16(toInt64(val))
		case reflect.Int32:
			ret = int32(toInt64(val))
		case reflect.Uint:
			ret = uint(toInt64(val))
		case reflect.Uint8:
			ret = uint8(toInt64(val))
		case reflect.Uint16:
			ret = uint16(toInt64(val))
		case reflect.Uint32:
			ret = uint32(toInt64(val))
		case reflect.Int64:
			ret = int64(toInt64(val))
		case reflect.Uint64:
			ret = uint64(toInt64(val))
		case reflect.Float32:
			ret = float32(toNumber(val))
		case reflect.Float64:
			ret = float64(toNumber(val))
		default:
			ret = float64(toNumber(val))
		}
	case lua.LTBool:
		ret = bool(lua.LVAsBool(val))
	case lua.LTString:
		ret = string(lua.LVAsString(val))
	case lua.LTTable:
		ret = getTable(val, nil)
	}

	return ret
}
