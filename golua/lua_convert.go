package lua

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	rt "github.com/arnodel/golua/runtime"
	"touchgocore/util"
	"touchgocore/vars"
)

// GoToLuaValue 将 Go 值转换为 arnodel/golua 的 rt.Value（向后兼容）
func GoToLuaValue(val interface{}) rt.Value {
	return GoToLuaValueWithContext(context.Background(), val)
}

// GoToLuaValueWithContext 把 Go 值转成 rt.Value，折不动时记一条日志并返回 nil。
//
// 公开签名里没有错误通道（约束：不能改），所以「折不动」在这一层只能变成 nil。
// 需要知道原因的用 GoToLuaValueErr —— 桥接层的返回值编码就走它。
func GoToLuaValueWithContext(ctx context.Context, val interface{}) rt.Value {
	luaValue, err := GoToLuaValueErr(ctx, val)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		vars.Error("Go→Lua 转换失败 (%T): %v", val, err)
	}
	return luaValue
}

// GoToLuaValueErr 转换并给出失败原因。
//
// 此前折不动的类型（struct、指向 struct 的指针、自定义类型）一律静默变 nil：
// Npc.GetShop 返回 []*ShopItem、GetDialog 返回 *DialogItem，Lua 侧拿到的就是
// nil 或全是 nil 的数组，而 Go 侧一声不响。现在这类值折成 Lua 表，
// 真折不动的（chan/func/超深）返回 error。
func GoToLuaValueErr(ctx context.Context, val interface{}) (rt.Value, error) {
	if err := ctx.Err(); err != nil {
		return rt.NilValue, err
	}
	if val == nil {
		return rt.NilValue, nil
	}
	return goToLuaReflect(ctx, reflect.ValueOf(val), 0, map[uintptr]*LuaTable{})
}

// goToLuaReflect 经 LuaTable 中间层把任意 Go 值折成 rt.Value。
func goToLuaReflect(ctx context.Context, rv reflect.Value, depth int, seen map[uintptr]*LuaTable) (rt.Value, error) {
	value, ok, err := goTableValue(ctx, rv, depth, seen)
	if err != nil {
		return rt.NilValue, err
	}
	if !ok {
		return rt.NilValue, nil
	}

	switch v := value.(type) {
	case *LuaTable:
		return rt.TableValue(v.ToTableWithContext(ctx)), nil
	case string:
		return rt.StringValue(v), nil
	case int64:
		return rt.IntValue(v), nil
	case float64:
		return rt.FloatValue(v), nil
	case bool:
		return rt.BoolValue(v), nil
	case nil:
		return rt.NilValue, nil
	default:
		return rt.NilValue, fmt.Errorf("无法表示为 Lua 值的类型 %T", value)
	}
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

	case reflect.Slice, reflect.Array:
		return luaTableToSequence(ctx, v, targetType)

	case reflect.Map:
		return luaTableToMap(ctx, v, targetType)

	case reflect.Pointer:
		return luaTableToStructPointer(ctx, v, targetType)

	case reflect.Struct:
		return luaTableToStruct(ctx, v, targetType)

	case reflect.Func:
		return reflect.Value{}, fmt.Errorf("不支持把 Lua 值转换为 Go 函数形参 %s", targetType)

	case reflect.Interface:
		return luaValueToInterface(ctx, v, targetType)

	default:
		return reflect.Value{}, fmt.Errorf("unsupported Go type %s (目标类型 %s)", targetType.Kind(), targetType)
	}
}

// luaTableToSequence 把 Lua 表按 1..n 顺序键折成 slice / array。
// 空洞之后不再取值（与 Lua 的 # 语义一致）。
func luaTableToSequence(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	tbl, ok := v.TryTable()
	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot convert Lua %s to Go %s", v.TypeName(), targetType)
	}

	length := tbl.Len() // rt.Table 的 # 语义，int64
	if targetType.Kind() == reflect.Array {
		if length > int64(targetType.Len()) {
			return reflect.Value{}, fmt.Errorf("Lua 表长度 %d 超过数组容量 %d", length, targetType.Len())
		}
		length = int64(targetType.Len())
	}

	var out reflect.Value
	if targetType.Kind() == reflect.Array {
		out = reflect.New(targetType).Elem()
	} else {
		out = reflect.MakeSlice(targetType, int(length), int(length))
	}

	for i := int64(0); i < length; i++ {
		elem := tbl.Get(rt.IntValue(i + 1))
		converted, err := LuaToReflectValueWithContext(ctx, elem, targetType.Elem())
		if err != nil {
			return reflect.Value{}, fmt.Errorf("第 %d 个元素: %w", i+1, err)
		}
		out.Index(int(i)).Set(converted)
	}
	return out, nil
}

// luaTableToMap 把 Lua 表全量遍历成 Go map。
func luaTableToMap(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	tbl, ok := v.TryTable()
	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot convert Lua %s to Go %s", v.TypeName(), targetType)
	}

	out := reflect.MakeMap(targetType)
	var key rt.Value = rt.NilValue
	for {
		k, val, more := tbl.Next(key)
		// 表尾的约定是 (nil, nil, true)：ok 为真但键是 nil，必须靠判 nil 收尾，
		// 只看 ok 会在这里原地打转（上游 hashtable.go 的 next 结尾就是这么返回的）。
		if !more || k.IsNil() {
			break
		}
		key = k

		goKey, err := LuaToReflectValueWithContext(ctx, k, targetType.Key())
		if err != nil {
			return reflect.Value{}, fmt.Errorf("map 键 %v: %w", k, err)
		}
		goValue, err := LuaToReflectValueWithContext(ctx, val, targetType.Elem())
		if err != nil {
			return reflect.Value{}, fmt.Errorf("map 键 %v: %w", k, err)
		}
		out.SetMapIndex(goKey, goValue)
	}
	return out, nil
}

// luaTableToStructPointer 只支持「指向结构体」的指针形参：
// Lua 表里没有指针语义，指向标量/切片/映射的指针无法可靠还原。
// userdata 例外——它本来就登记着 Go 对象，直接解包更准确。
func luaTableToStructPointer(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	if ud, ok := v.TryUserData(); ok && ud != nil {
		if obj, ok := luaUserDataObject(ud); ok {
			rv := reflect.ValueOf(obj)
			if rv.Type().AssignableTo(targetType) {
				return rv, nil
			}
			return reflect.Value{}, fmt.Errorf("userdata 登记的对象类型 %s 无法赋给形参 %s", rv.Type(), targetType)
		}
	}

	if targetType.Elem().Kind() != reflect.Struct {
		return reflect.Value{}, fmt.Errorf("不支持的指针形参类型 %s（仅支持结构体指针）", targetType)
	}
	converted, err := luaTableToStruct(ctx, v, targetType.Elem())
	if err != nil {
		return reflect.Value{}, err
	}
	ptr := reflect.New(targetType.Elem())
	ptr.Elem().Set(converted)
	return ptr, nil
}

// luaTableToStruct 把 Lua 表填进 Go 结构体。
//
// 键名同时接受 json tag 与 Go 导出字段名（宽松入），写出时统一用 json tag（严格出）。
// 未知键忽略：配置表里混着别的字段是常态。已知字段转换失败要报错，
// 否则半个结构体悄悄没填上，调用方只会看到零值。
func luaTableToStruct(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	tbl, ok := v.TryTable()
	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot convert Lua %s to Go 结构体 %s", v.TypeName(), targetType)
	}

	out := reflect.New(targetType).Elem()
	for i := 0; i < targetType.NumField(); i++ {
		field := targetType.Field(i)
		tagName, exposed := luaFieldName(field)
		if !exposed {
			continue
		}

		luaValue := rt.NilValue
		if tagName != "" {
			luaValue = tbl.Get(rt.StringValue(tagName))
		}
		if luaValue == rt.NilValue && field.Name != tagName {
			luaValue = tbl.Get(rt.StringValue(field.Name))
		}
		if luaValue == rt.NilValue {
			continue // 未知/缺失键：保持零值
		}

		converted, err := LuaToReflectValueWithContext(ctx, luaValue, field.Type)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("字段 %s: %w", field.Name, err)
		}
		out.Field(i).Set(converted)
	}
	return out, nil
}

// luaValueToInterface 处理 any 形参。
//
// 三条口径：
//   - userdata 解包回它登记的 Go 对象（脚本把 npc 当参数传回来是合法用法）；
//   - Lua 函数与协程没有 Go 表示，直接报错 —— 此前经 reflect.Zero 静默变成 nil；
//   - 其余走 LuaToGoValue，表折成 *LuaTable（DialogItem.Param1 依赖这一点）。
func luaValueToInterface(ctx context.Context, v rt.Value, targetType reflect.Type) (reflect.Value, error) {
	if ud, ok := v.TryUserData(); ok && ud != nil {
		if obj, ok := luaUserDataObject(ud); ok {
			rv := reflect.ValueOf(obj)
			if rv.Type().AssignableTo(targetType) {
				return rv, nil
			}
		}
	}

	if _, ok := v.TryCallable(); ok {
		return reflect.Value{}, fmt.Errorf("Lua 函数无法转换为 Go 形参 %s", targetType)
	}
	if _, ok := v.TryThread(); ok {
		return reflect.Value{}, fmt.Errorf("Lua 协程无法转换为 Go 形参 %s", targetType)
	}

	goValue := LuaToGoValueWithContext(ctx, v)
	if goValue == nil {
		return reflect.Zero(targetType), nil
	}
	return reflect.ValueOf(goValue), nil
}

// luaUserDataObject 取回 userdata 登记的 Go 对象（对象已被回收时返回 false）。
func luaUserDataObject(ud *rt.UserData) (interface{}, bool) {
	meta, ok := ud.Value().(*userDataMeta)
	if !ok {
		return nil, false
	}
	return meta.script.registeredObjects.Load(meta.uid)
}
