package lua

import (
	"context"
	"reflect"
	"touchgocore/syncmap"

	rt "github.com/arnodel/golua/runtime"
)

// newTable 从 Go 数据创建 LuaTable 对象
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
	if data == nil {
		return tbl
	}

	// 根据数据类型转换
	switch v := data.(type) {
	case []string:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []int64:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []float64:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []interface{}:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case map[interface{}]interface{}:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[string]string:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[string]int64:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[int64]int64:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[int64]string:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[string]interface{}:
		// 支持嵌套 map[string]interface{}
		for key, value := range v {
			// 递归处理嵌套结构
			if isNestedStructure(value) {
				tbl.SetTableData(key, newTable(value))
			} else {
				tbl.SetTableData(key, value)
			}
		}
	case []map[string]interface{}:
		// 支持嵌套数组
		for i, item := range v {
			tbl.SetTableData(i+1, newTable(item))
		}
	default:
		// 尝试反射处理其他类型
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			length := rv.Len()
			for i := 0; i < length; i++ {
				elem := rv.Index(i).Interface()
				if isNestedStructure(elem) {
					tbl.AddListData(newTable(elem))
				} else {
					tbl.AddListData(elem)
				}
			}
		} else if rv.Kind() == reflect.Map {
			// 通用 map 处理
			iter := rv.MapRange()
			for iter.Next() {
				key := iter.Key().Interface()
				value := iter.Value().Interface()
				if isNestedStructure(value) {
					tbl.SetTableData(key, newTable(value))
				} else {
					tbl.SetTableData(key, value)
				}
			}
		}
	}
	return tbl
}

// isNestedStructure 检查是否是嵌套结构（map 或 slice）
func isNestedStructure(v interface{}) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array
}

// tableFromRuntimeV2 从 arnodel/golua 的 rt.Table 转换为 *LuaTable (内部版本)
func tableFromRuntimeV2(ctx context.Context, tbl *rt.Table) *LuaTable {
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

		goKey := LuaToGoValueWithContext(ctx, key)
		goValue := LuaToGoValueWithContext(ctx, val)

		// 检查值是否是嵌套 table
		if t, ok := val.TryTable(); ok {
			nestedTbl := tableFromRuntimeV2(ctx, t)
			result.Set(goKey, nestedTbl)
		} else {
			result.Set(goKey, goValue)
		}

		k = key
	}

	return result
}

type LuaTable struct {
	tbl *syncmap.MapAny
}

// HasData 检查 table 是否有数据
func (this *LuaTable) HasData() bool {
	return this.tbl != nil && this.tbl.Length() > 0
}

// HaveData 检查 table 是否有数据（兼容性别名）
func (this *LuaTable) HaveData() bool {
	return this.HasData()
}

// Append 添加列表元素
func (this *LuaTable) Append(val interface{}) {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	this.Set(this.tbl.Length()+1, val)
}

// AddListData 添加列表元素（兼容性别名）
func (this *LuaTable) AddListData(val interface{}) {
	this.Append(val)
}

// Set 设置键值对
func (this *LuaTable) Set(key, val interface{}) {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	this.tbl.Store(key, val)
}

// SetTableData 设置键值对（兼容性别名）
func (this *LuaTable) SetTableData(key, val interface{}) {
	this.Set(key, val)
}

// ToTable 转换为 arnodel/golua 的 rt.Table
func (lt *LuaTable) ToTable() *rt.Table {
	return lt.ToTableWithContext(context.Background())
}

// ToTableWithContext 使用上下文转换为 arnodel/golua 的 rt.Table
func (lt *LuaTable) ToTableWithContext(ctx context.Context) *rt.Table {
	if lt.tbl == nil {
		return rt.NewTable()
	}

	tbl := rt.NewTable()
	lt.tbl.Range(func(key, value interface{}) bool {
		luaKey := GoToLuaValueWithContext(ctx, key)

		// 检查值是否是嵌套 LuaTable
		if nestedTbl, ok := value.(*LuaTable); ok && nestedTbl != nil {
			nestedTable := nestedTbl.ToTableWithContext(ctx)
			luaValue := rt.TableValue(nestedTable)
			tbl.Set(luaKey, luaValue)
		} else {
			luaValue := GoToLuaValueWithContext(ctx, value)
			tbl.Set(luaKey, luaValue)
		}

		return true
	})

	return tbl
}

// PushTable 保持向后兼容的函数（已废弃）
// 在新版本中，使用 ToTable() 替代
func (this *LuaTable) PushTable(L interface{}) bool {
	// 为了向后兼容，接受第一个参数但不使用
	_ = L
	return this.HasData()
}

// SubTable 获取或创建嵌套 table
func (this *LuaTable) SubTable(key interface{}) *LuaTable {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	data, _ := this.tbl.LoadOrStore(key, newTable(nil))
	return data.(*LuaTable)
}

// AddTableData 获取或创建嵌套 table（兼容性别名）
func (this *LuaTable) AddTableData(key interface{}) *LuaTable {
	return this.SubTable(key)
}

// Length 返回 table 长度
func (this *LuaTable) Length() int {
	if this.tbl == nil {
		return 0
	}
	return this.tbl.Length()
}
