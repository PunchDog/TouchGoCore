package lua

import (
	"fmt"
	"reflect"
	"touchgocore/syncmap"
	"touchgocore/util"

	"github.com/aarzilli/golua/lua"
)

// PushValue 将 Go 值压入 Lua 栈
// 支持类型：string, int/uint系列, bool, float32/64, *LuaTable, *syncmap.Map, 以及可转换为 table 的类型
// 注意: 此函数依赖 github.com/aarzilli/golua/lua,需要配置 CGO 环境才能编译
// 编译要求: set CGO_ENABLED=1 && go build -tags "!lua52,!lua53,!lua54"
func PushValue(L *lua.State, val interface{}) bool {
	if val == nil {
		L.PushNil()
		return true
	}

	switch v := val.(type) {
	case string:
		L.PushString(v)
	case int, int8, int16, int32, int64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		L.PushInteger(d)
	case uint, uint8, uint16, uint32, uint64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		L.PushInteger(d)
	case bool:
		L.PushBoolean(v)
	case float32:
		L.PushNumber(float64(v))
	case float64:
		L.PushNumber(v)
	case *LuaTable:
		if v != nil {
			return v.PushTable(L)
		}
		L.PushNil()
		return true
	case *syncmap.Map:
		if v != nil {
			tbl := &LuaTable{tbl: v}
			return tbl.PushTable(L)
		}
		L.PushNil()
		return true
	default:
		// 尝试将其他类型转换为 table
		tbl := newTable(val)
		if tbl.HaveData() {
			return tbl.PushTable(L)
		}
		return false
	}
	return true
}

// LuaToGoValue 将 Lua 值转换为 Go 值
// 支持类型：boolean, string, number, table, nil
// table 可转换为 *LuaTable
func LuaToGoValue(L *lua.State, idx int) interface{} {
	luaType := L.Type(idx)

	switch luaType {
	case lua.LUA_TBOOLEAN:
		return L.ToBoolean(idx)
	case lua.LUA_TSTRING:
		return L.ToString(idx)
	case lua.LUA_TNUMBER:
		return L.ToNumber(idx)
	case lua.LUA_TTABLE:
		return getTable(L, idx)
	case lua.LUA_TNIL:
		return nil
	default:
		return nil
	}
}

// LuaToGoReflectValue 将 Lua 值转换为指定 Go 类型的反射值
// 用于函数参数的类型转换
func LuaToGoReflectValue(L *lua.State, idx int, targetType reflect.Type) (reflect.Value, error) {
	luaType := L.Type(idx)

	switch luaType {
	case lua.LUA_TBOOLEAN:
		if targetType.Kind() == reflect.Bool {
			return reflect.ValueOf(L.ToBoolean(idx)), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua boolean 转换为 Go %s 类型", targetType.Kind())

	case lua.LUA_TSTRING:
		if targetType.Kind() == reflect.String {
			return reflect.ValueOf(L.ToString(idx)), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua string 转换为 Go %s 类型", targetType.Kind())

	case lua.LUA_TNUMBER:
		num := L.ToNumber(idx)
		switch targetType.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			converted := util.ConvertToKind(num, targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			converted := util.ConvertToKind(num, targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		case reflect.Float32, reflect.Float64:
			return reflect.ValueOf(num).Convert(targetType), nil
		case reflect.Interface:
			return reflect.ValueOf(num), nil
		default:
			return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua number (%v) 转换为 Go %s 类型", num, targetType.Kind())
		}

	case lua.LUA_TTABLE:
		table := getTable(L, idx)
		if table == nil {
			return reflect.Value{}, fmt.Errorf("类型转换错误: 无法读取 Lua table")
		}

		// 检查目标类型是否是 *LuaTable
		if targetType.Kind() == reflect.Ptr && targetType.Elem() == reflect.TypeOf(LuaTable{}) {
			return reflect.ValueOf(table), nil
		}

		// 检查目标类型是否是 *syncmap.Map
		if targetType.Kind() == reflect.Ptr && targetType.Elem() == reflect.TypeOf(syncmap.Map{}) {
			return reflect.ValueOf(table.tbl), nil
		}

		return reflect.Value{}, fmt.Errorf("类型转换错误: 不支持将 Lua table 转换为 Go %v 类型", targetType)

	case lua.LUA_TNIL:
		return reflect.Zero(targetType), nil

	default:
		return reflect.Value{}, fmt.Errorf("类型转换错误: 不支持的 Lua 类型: %v", luaType)
	}
}
