package lua

import (
	"touchgocore/syncmap"
)

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

// Len 返回 table 长度
func (this *LuaTable) Len() int {
	if this.tbl == nil {
		return 0
	}
	return this.tbl.Length()
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
