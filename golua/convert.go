package lua

import (
	"context"
	"fmt"
	"reflect"
	"touchgocore/syncmap"
	"touchgocore/util"

	rt "github.com/arnodel/golua/runtime"
)

// GoToLuaValue 将 Go 值转换为 arnodel/golua 的 rt.Value（向后兼容）
func GoToLuaValue(val interface{}) rt.Value {
	return GoToLuaValueWithContext(context.Background(), val)
}

// GoToLuaValueWithContext 使用上下文将 Go 值转换为 arnodel/golua 的 rt.Value
func GoToLuaValueWithContext(ctx context.Context, val interface{}) rt.Value {
	select {
	case <-ctx.Done():
		return rt.NilValue
	default:
	}

	if val == nil {
		return rt.NilValue
	}

	switch v := val.(type) {
	case string:
		return rt.StringValue(v)
	case int, int8, int16, int32, int64:
		return rt.IntValue(reflect.ValueOf(val).Int())
	case uint, uint8, uint16, uint32, uint64:
		return rt.IntValue(int64(reflect.ValueOf(val).Uint()))
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
	case *syncmap.MapAny:
		if v != nil {
			tbl := &LuaTable{tbl: v}
			return rt.TableValue(tbl.ToTable())
		}
		return rt.NilValue
	default:
		// 尝试将其他类型转换为 table
		if tbl := convertToTable(ctx, val); tbl != nil && tbl.HaveData() {
			return rt.TableValue(tbl.ToTable())
		}
		return rt.NilValue
	}
}

// convertToTable 尝试将任意类型转换为 LuaTable
func convertToTable(ctx context.Context, val interface{}) *LuaTable {
	if val == nil {
		return nil
	}

	select {
	case <-ctx.Done():
		return nil
	default:
	}

	defer func() {
		if r := recover(); r != nil {
			// 类型转换失败，忽略
		}
	}()

	return newTable(val)
}

// LuaToGoValue 将 arnodel/golua 的 rt.Value 转换为 Go 值（向后兼容）
func LuaToGoValue(v rt.Value) interface{} {
	return LuaToGoValueWithContext(context.Background(), v)
}

// LuaToGoValueWithContext 使用上下文将 arnodel/golua 的 rt.Value 转换为 Go 值
func LuaToGoValueWithContext(ctx context.Context, v rt.Value) interface{} {
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	if v.IsNil() {
		return nil
	}

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
		return tableFromRuntime(ctx, t)
	}

	return nil
}

// LuaToReflectValue 将 Lua 值转换为指定 Go 类型的反射值（向后兼容）
func LuaToReflectValue(v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	return LuaToReflectValueWithContext(context.Background(), v, targetType)
}

// LuaToReflectValueWithContext 使用上下文将 Lua 值转换为指定 Go 类型的反射值
func LuaToReflectValueWithContext(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	select {
	case <-ctx.Done():
		return reflect.Value{}, ctx.Err()
	default:
	}

	if v.IsNil() {
		return reflect.Zero(targetType), nil
	}

	switch targetType.Kind() {
	case reflect.Bool:
		if b, ok := v.TryBool(); ok {
			return reflect.ValueOf(b), nil
		}
		return reflect.Value{}, fmt.Errorf("cannot convert Lua value to Go bool")

	case reflect.String:
		if s, ok := v.TryString(); ok {
			return reflect.ValueOf(s), nil
		}
		return reflect.Value{}, fmt.Errorf("cannot convert Lua value to Go string")

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if i, ok := v.TryInt(); ok {
			converted := util.ConvertToKind(float64(i), targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		}
		return reflect.Value{}, fmt.Errorf("cannot convert Lua value to Go %s", targetType.Kind())

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if i, ok := v.TryInt(); ok {
			converted := util.ConvertToKind(float64(i), targetType.Kind())
			return reflect.ValueOf(converted).Convert(targetType), nil
		}
		return reflect.Value{}, fmt.Errorf("cannot convert Lua value to Go %s", targetType.Kind())

	case reflect.Float32, reflect.Float64:
		var num float64
		if f, ok := v.TryFloat(); ok {
			num = f
		} else if i, ok := v.TryInt(); ok {
			num = float64(i)
		} else {
			return reflect.Value{}, fmt.Errorf("cannot convert Lua value to Go %s", targetType.Kind())
		}
		return reflect.ValueOf(num).Convert(targetType), nil

	case reflect.Interface:
		return reflect.ValueOf(LuaToGoValueWithContext(ctx, v)), nil

	default:
		return reflect.Value{}, fmt.Errorf("unsupported Go type %s", targetType.Kind())
	}
}

// tableFromRuntime 从 arnodel/golua 的 rt.Table 转换为 *LuaTable
func tableFromRuntime(ctx context.Context, tbl *rt.Table) *LuaTable {
	if tbl == nil {
		return nil
	}

	result := newTable(nil)

	// 使用 Next 遍历 table
	var k rt.Value = rt.NilValue
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		key, val, ok := tbl.Next(k)
		if !ok || key == rt.NilValue {
			break
		}

		goKey := LuaToGoValueWithContext(ctx, key)
		
		// 检查值是否是嵌套 table
		if t, ok := val.TryTable(); ok {
			nestedTbl := tableFromRuntime(ctx, t)
			result.Set(goKey, nestedTbl)
		} else {
			goValue := LuaToGoValueWithContext(ctx, val)
			result.Set(goKey, goValue)
		}

		k = key
	}

	return result
}
