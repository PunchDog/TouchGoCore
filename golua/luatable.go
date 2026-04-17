package lua

import (
	"context"
	"reflect"
	"touchgocore/syncmap"

	rt "github.com/arnodel/golua/runtime"
)

// populateTable 填充 LuaTable 数据（内部辅助函数）
func populateTable(tbl *LuaTable, data interface{}) {
	if data == nil {
		return
	}

	switch v := data.(type) {
	case []string:
		for _, item := range v {
			tbl.Append(item)
		}
	case []int64:
		for _, item := range v {
			tbl.Append(item)
		}
	case []float64:
		for _, item := range v {
			tbl.Append(item)
		}
	case []interface{}:
		for _, item := range v {
			tbl.Append(item)
		}
	case map[interface{}]interface{}:
		for key, value := range v {
			tbl.Set(key, value)
		}
	case map[string]string:
		for key, value := range v {
			tbl.Set(key, value)
		}
	case map[string]int64:
		for key, value := range v {
			tbl.Set(key, value)
		}
	case map[int64]int64:
		for key, value := range v {
			tbl.Set(key, value)
		}
	case map[int64]string:
		for key, value := range v {
			tbl.Set(key, value)
		}
	case map[string]interface{}:
		for key, value := range v {
			if isNestedStructure(value) {
				tbl.Set(key, newTable(value))
			} else {
				tbl.Set(key, value)
			}
		}
	case []map[string]interface{}:
		for i, item := range v {
			tbl.Set(i+1, newTable(item))
		}
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			length := rv.Len()
			for i := 0; i < length; i++ {
				elem := rv.Index(i).Interface()
				if isNestedStructure(elem) {
					tbl.Append(newTable(elem))
				} else {
					tbl.Append(elem)
				}
			}
		} else if rv.Kind() == reflect.Map {
			iter := rv.MapRange()
			for iter.Next() {
				key := iter.Key().Interface()
				value := iter.Value().Interface()
				if isNestedStructure(value) {
					tbl.Set(key, newTable(value))
				} else {
					tbl.Set(key, value)
				}
			}
		}
	}
}

// newTable 从 Go 数据创建 LuaTable 对象
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
	populateTable(tbl, data)
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

// tableFromRuntime 从 arnodel/golua 的 rt.Table 转换为 *LuaTable
func tableFromRuntime(ctx context.Context, tbl *rt.Table) *LuaTable {
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

type LuaTable struct {
	tbl *syncmap.MapAny
}

// HasData 检查 table 是否有数据
func (this *LuaTable) HasData() bool {
	return this.tbl != nil && this.tbl.Length() > 0
}

// Append 添加列表元素
func (this *LuaTable) Append(val interface{}) {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	this.Set(this.tbl.Length()+1, val)
}

// Set 设置键值对
func (this *LuaTable) Set(key, val interface{}) {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	this.tbl.Store(key, val)
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

// Length 返回 table 长度
func (this *LuaTable) Length() int {
	if this.tbl == nil {
		return 0
	}
	return this.tbl.Length()
}

// Get 获取 table 中的值
func (this *LuaTable) Get(key interface{}) (interface{}, bool) {
	if this.tbl == nil {
		return nil, false
	}
	return this.tbl.Load(key)
}

// GetInt 获取 int64 类型的值
func (this *LuaTable) GetInt(key interface{}) (int64, bool) {
	val, ok := this.Get(key)
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// GetString 获取 string 类型的值
func (this *LuaTable) GetString(key interface{}) (string, bool) {
	val, ok := this.Get(key)
	if !ok {
		return "", false
	}
	if str, ok := val.(string); ok {
		return str, true
	}
	return "", false
}

// GetTable 获取嵌套的 LuaTable
func (this *LuaTable) GetTable(key interface{}) (*LuaTable, bool) {
	val, ok := this.Get(key)
	if !ok {
		return nil, false
	}
	if tbl, ok := val.(*LuaTable); ok {
		return tbl, true
	}
	return nil, false
}

// GetArray 获取数组形式的值
func (this *LuaTable) GetArray() []interface{} {
	if this.tbl == nil {
		return nil
	}

	arr := make([]interface{}, 0, this.tbl.Length())
	this.tbl.Range(func(key, value interface{}) bool {
		if idx, ok := key.(int64); ok && idx > 0 {
			if int(idx-1) < len(arr) {
				arr[idx-1] = value
			} else {
				arr = append(arr, value)
			}
		}
		return true
	})
	return arr
}

// Keys 获取所有 key
func (this *LuaTable) Keys() []interface{} {
	if this.tbl == nil {
		return nil
	}

	keys := make([]interface{}, 0)
	this.tbl.Range(func(key, _ interface{}) bool {
		keys = append(keys, key)
		return true
	})
	return keys
}

// Values 获取所有 value
func (this *LuaTable) Values() []interface{} {
	if this.tbl == nil {
		return nil
	}

	values := make([]interface{}, 0)
	this.tbl.Range(func(_, value interface{}) bool {
		values = append(values, value)
		return true
	})
	return values
}

// ToMap 转换为 map[string]interface{}（仅限 string key）
func (this *LuaTable) ToMap() map[string]interface{} {
	if this.tbl == nil {
		return nil
	}

	result := make(map[string]interface{})
	this.tbl.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if nestedTbl, ok := value.(*LuaTable); ok {
				result[k] = nestedTbl.ToMap()
			} else {
				result[k] = value
			}
		}
		return true
	})
	return result
}

// Merge 合并另一个 LuaTable
func (this *LuaTable) Merge(other *LuaTable) {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}
	if other.tbl != nil {
		other.tbl.Range(func(key, value interface{}) bool {
			this.tbl.Store(key, value)
			return true
		})
	}
}

// Clear 清空 table
func (this *LuaTable) Clear() {
	if this.tbl != nil {
		this.tbl.ClearAll(nil)
	}
}
