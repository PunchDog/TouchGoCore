package lua

import (
	"fmt"
	"touchgocore/syncmap"
)

// LuaTable 扩展方法 - 支持更多 key 类型

// GetByPath 通过路径获取嵌套值
// 示例: GetByPath("player.stats.health")
func (this *LuaTable) GetByPath(path string) (interface{}, bool) {
	if this.tbl == nil {
		return nil, false
	}

	// 解析路径（支持点号分隔）
	keys := parsePath(path)
	if len(keys) == 0 {
		return nil, false
	}

	var current interface{} = this
	for _, key := range keys {
		if tbl, exists := current.(*LuaTable); exists {
			val, ok := tbl.Get(key)
			if !ok {
				return nil, false
			}
			current = val
		} else {
			return nil, false
		}
	}

	return current, true
}

// SetByPath 通过路径设置嵌套值
func (this *LuaTable) SetByPath(path string, value interface{}) error {
	if this.tbl == nil {
		this.tbl = syncmap.NewAny()
	}

	keys := parsePath(path)
	if len(keys) == 0 {
		return fmt.Errorf("无效的路径")
	}

	// 遍历路径，创建嵌套结构
	var current *LuaTable = this
	for i, key := range keys {
		isLast := i == len(keys)-1

		if isLast {
			// 最后一个 key，设置值
			current.SetTableData(key, value)
		} else {
			// 获取或创建嵌套 table
			val, exists := current.Get(key)
			var next *LuaTable

			if !exists {
				// 创建新的嵌套 table
				next = newTable(nil)
				current.SetTableData(key, next)
			} else {
				// 已存在，检查是否是 LuaTable
				next, isTable := val.(*LuaTable)
				if !isTable {
					// 不是 table，需要替换
					next = newTable(nil)
					current.SetTableData(key, next)
				}
			}

			current = next
		}
	}

	return nil
}

// parsePath 解析路径字符串
func parsePath(path string) []interface{} {
	if path == "" {
		return nil
	}

	parts := make([]interface{}, 0)
	var current string
	inBrackets := false
	bracketChar := byte(0)

	for i := 0; i < len(path); i++ {
		ch := path[i]

		if inBrackets {
			if ch == bracketChar {
				// 方括号结束
				if current != "" {
					parts = append(parts, current)
					current = ""
				}
				inBrackets = false
				bracketChar = 0
				// 跳过可能的点号
				if i+1 < len(path) && path[i+1] == '.' {
					i++
				}
			} else {
				current += string(ch)
			}
		} else {
			if ch == '.' {
				// 点号分割
				if current != "" {
					parts = append(parts, current)
					current = ""
				}
			} else if ch == '[' {
				// 方括号开始
				if current != "" {
					parts = append(parts, current)
					current = ""
				}
				inBrackets = true
				if i+1 < len(path) {
					nextCh := path[i+1]
					if nextCh == '\'' || nextCh == '"' {
						bracketChar = nextCh
						i++ // 跳过引号
					} else {
						bracketChar = ']' // 支持数字索引，如 [1]
					}
				}
			} else {
				current += string(ch)
			}
		}
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}

// GetInt64 获取 int64 值
func (this *LuaTable) GetInt64(key interface{}) (int64, bool) {
	return this.GetInt(key)
}

// GetFloat64 获取 float64 值
func (this *LuaTable) GetFloat64(key interface{}) (float64, bool) {
	val, ok := this.Get(key)
	if !ok {
		return 0, false
	}

	switch v := val.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

// GetBool 获取 bool 值
func (this *LuaTable) GetBool(key interface{}) (bool, bool) {
	val, ok := this.Get(key)
	if !ok {
		return false, false
	}
	if b, ok := val.(bool); ok {
		return b, true
	}
	return false, false
}

// HasKey 检查 key 是否存在
func (this *LuaTable) HasKey(key interface{}) bool {
	_, ok := this.Get(key)
	return ok
}

// Delete 删除 key
func (this *LuaTable) Delete(key interface{}) {
	if this.tbl != nil {
		this.tbl.Delete(key)
	}
}

// ForEach 遍历 table
func (this *LuaTable) ForEach(fn func(key, value interface{}) bool) {
	if this.tbl != nil {
		this.tbl.Range(fn)
	}
}

// Filter 过滤 table
func (this *LuaTable) Filter(predicate func(key, value interface{}) bool) *LuaTable {
	result := newTable(nil)

	if this.tbl != nil {
		this.tbl.Range(func(key, value interface{}) bool {
			if predicate(key, value) {
				result.SetTableData(key, value)
			}
			return true
		})
	}

	return result
}

// Map 映射转换 table
func (this *LuaTable) Map(fn func(key, value interface{}) (interface{}, interface{})) *LuaTable {
	result := newTable(nil)

	if this.tbl != nil {
		this.tbl.Range(func(key, value interface{}) bool {
			newKey, newValue := fn(key, value)
			result.SetTableData(newKey, newValue)
			return true
		})
	}

	return result
}

// Copy 深拷贝 table
func (this *LuaTable) Copy() *LuaTable {
	result := newTable(nil)

	if this.tbl != nil {
		this.tbl.Range(func(key, value interface{}) bool {
			// 递归拷贝嵌套 table
			if nested, ok := value.(*LuaTable); ok {
				result.SetTableData(key, nested.Copy())
			} else {
				result.SetTableData(key, value)
			}
			return true
		})
	}

	return result
}

// Clone 创建浅拷贝
func (this *LuaTable) Clone() *LuaTable {
	result := newTable(nil)

	if this.tbl != nil {
		this.tbl.Range(func(key, value interface{}) bool {
			result.SetTableData(key, value)
			return true
		})
	}

	return result
}

// String 返回 table 的字符串表示
func (this *LuaTable) String() string {
	if this.tbl == nil {
		return "{}"
	}

	result := "{"
	first := true

	this.tbl.Range(func(key, value interface{}) bool {
		if !first {
			result += ", "
		}
		first = false

		// 格式化 key
		result += FormatKey(key)
		result += "="

		// 格式化 value
		if nested, ok := value.(*LuaTable); ok {
			result += nested.String()
		} else {
			result += fmt.Sprintf("%v", value)
		}

		return true
	})

	result += "}"
	return result
}
