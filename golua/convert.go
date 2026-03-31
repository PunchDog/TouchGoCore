package lua

import (
	"fmt"
	"reflect"
	rt "github.com/arnodel/golua/runtime"
	"touchgocore/syncmap"
	"touchgocore/util"
)

// GoToLuaValue 将 Go 值转换为 arnodel/golua 的 rt.Value
// 支持类型：string, int/uint系列, bool, float32/64, *LuaTable, *syncmap.Map, 以及可转换为 table 的类型
// 注意: 此函数不再依赖 CGO，使用纯 Go 实现的 arnodel/golua
func GoToLuaValue(val interface{}) rt.Value {
	if val == nil {
		return rt.NilValue
	}

	switch v := val.(type) {
	case string:
		return rt.StringValue(v)
	case int, int8, int16, int32, int64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		return rt.IntValue(d)
	case uint, uint8, uint16, uint32, uint64:
		d := int64(0)
		val1 := reflect.ValueOf(val).Convert(reflect.ValueOf(d).Type())
		reflect.ValueOf(&d).Elem().Set(val1)
		return rt.IntValue(d)
	case bool:
		return rt.BoolValue(v)
	case float32:
		return rt.FloatValue(float64(v))
	case float64:
		return rt.FloatValue(v)
	case *LuaTable:
		if v != nil {
			return rt.TableValue(v.ToTable())
		}
		return rt.NilValue
	case *syncmap.Map:
		if v != nil {
			tbl := &LuaTable{tbl: v}
			return rt.TableValue(tbl.ToTable())
		}
		return rt.NilValue
	default:
		// 尝试将其他类型转换为 table，使用 defer recover 防止 panic
		func() {
			defer func() {
				if r := recover(); r != nil {
					// 类型转换失败，忽略
				}
			}()
			tbl := newTable(val)
			if tbl.HaveData() {
				return
			}
		}()
		return rt.NilValue
	}
}

// LuaToGoValue 将 arnodel/golua 的 rt.Value 转换为 Go 值
// 支持类型：boolean, string, number, table, nil
// table 可转换为 *LuaTable
func LuaToGoValue(v rt.Value) interface{} {
	if v.IsNil() {
		return nil
	}

	// 使用 Try 系列方法检查类型
	if b, ok := v.TryBool(); ok {
		return b
	}
	if s, ok := v.TryString(); ok {
		return s
	}
	if i, ok := v.TryInt(); ok {
		return i
	}
	if f, ok := v.TryFloat(); ok {
		return f
	}
	if t, ok := v.TryTable(); ok {
		return tableFromRuntime(t)
	}

	return nil
}

// LuaToReflectValue 将 Lua 值转换为指定 Go 类型的反射值
// 用于函数参数的类型转换
func LuaToReflectValue(v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	if v.IsNil() {
		return reflect.Zero(targetType), nil
	}

	switch targetType.Kind() {
	case reflect.Bool:
		if b, ok := v.TryBool(); ok {
			return reflect.ValueOf(b), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua 值转换为 Go bool 类型")

	case reflect.String:
		if s, ok := v.TryString(); ok {
			return reflect.ValueOf(s), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua 值转换为 Go string 类型")

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if i, ok := v.TryInt(); ok {
			converted := util.ConvertToKind(float64(i), targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua 值转换为 Go %s 类型", targetType.Kind())

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if i, ok := v.TryInt(); ok {
			converted := util.ConvertToKind(float64(i), targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		}
		return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua 值转换为 Go %s 类型", targetType.Kind())

	case reflect.Float32, reflect.Float64:
		var num float64
		if f, ok := v.TryFloat(); ok {
			num = f
		} else if i, ok := v.TryInt(); ok {
			num = float64(i)
		} else {
			return reflect.Value{}, fmt.Errorf("类型转换错误: 无法将 Lua 值转换为 Go %s 类型", targetType.Kind())
		}
		return reflect.ValueOf(num).Convert(targetType), nil

	case reflect.Interface:
		// 返回 interface{} 类型
		if b, ok := v.TryBool(); ok {
			return reflect.ValueOf(b), nil
		} else if s, ok := v.TryString(); ok {
			return reflect.ValueOf(s), nil
		} else if i, ok := v.TryInt(); ok {
			return reflect.ValueOf(i), nil
		} else if f, ok := v.TryFloat(); ok {
			return reflect.ValueOf(f), nil
		} else if t, ok := v.TryTable(); ok {
			tbl := tableFromRuntime(t)
			return reflect.ValueOf(tbl), nil
		}
		return reflect.ValueOf(nil), nil

	default:
		return reflect.Value{}, fmt.Errorf("类型转换错误: 不支持的 Go 类型 %s", targetType.Kind())
	}
}

// tableFromRuntime 从 arnodel/golua 的 rt.Table 转换为 *LuaTable
func tableFromRuntime(tbl *rt.Table) *LuaTable {
	if tbl == nil {
		return nil
	}

	result := newTable(nil)

	// 使用 Next 遍历 table
	var k rt.Value = rt.NilValue
	for {
		key, val, ok := tbl.Next(k)
		if !ok || key == rt.NilValue {
			break
		}

		goKey := LuaToGoValue(key)
		goValue := LuaToGoValue(val)

		// 检查值是否是嵌套 table
		if t, ok := val.TryTable(); ok {
			nestedTbl := tableFromRuntime(t)
			result.Set(goKey, nestedTbl)
		} else {
			result.Set(goKey, goValue)
		}

		k = key
	}

	return result
}

// PushValue 保持向后兼容的函数（已废弃，建议使用 GoToLuaValue）
// 这个函数将在内部调用 GoToLuaValue
// 注意: 新版本不再接受 *lua.State 参数，改为直接返回 rt.Value
func PushValue(L interface{}, val interface{}) bool {
	// 为了向后兼容，我们接受第一个参数但不使用
	// 在完全迁移后，可以移除此函数或改为仅返回 rt.Value
	_ = L
	_ = GoToLuaValue(val)
	return true
}
