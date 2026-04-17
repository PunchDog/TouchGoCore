package lua

import (
	"fmt"
	"sync"
	"touchgocore/syncmap"
)

// LuaTable 扩展方法 - 支持更多 key 类型

// pathCacheEntry 路径解析缓存条目
type pathCacheEntry struct {
	keys  []interface{}
	error error
}

var (
	pathCache     sync.Map // map[string]*pathCacheEntry
	cacheMaxSize = 1000   // 最大缓存条目数
)

// GetByPath 通过路径获取嵌套值
// 示例: GetByPath("player.stats.health")
func (lt *LuaTable) GetByPath(path string) (interface{}, bool) {
	if lt.tbl == nil {
		return nil, false
	}

	// 从缓存获取解析结果
	entry := getPathFromCache(path)
	if entry.error != nil {
		return nil, false
	}

	var current interface{} = lt
	for _, key := range entry.keys {
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
func (lt *LuaTable) SetByPath(path string, value interface{}) error {
	if lt.tbl == nil {
		lt.tbl = syncmap.NewAny()
	}

	// 从缓存获取解析结果
	entry := getPathFromCache(path)
	if entry.error != nil {
		return entry.error
	}

	if len(entry.keys) == 0 {
		return fmt.Errorf("invalid path")
	}

	// 遍历路径，创建嵌套结构
	var current *LuaTable = lt
	for i, key := range entry.keys {
		isLast := i == len(entry.keys)-1

		if isLast {
			// 最后一个 key，设置值
			current.Set(key, value)
		} else {
			// 获取或创建嵌套 table
			val, exists := current.Get(key)
			var next *LuaTable

			if !exists {
				next = newTable(nil)
				current.Set(key, next)
			} else {
				next, isTable := val.(*LuaTable)
				if !isTable {
					// 不是 table，需要替换
					next = newTable(nil)
					current.Set(key, next)
				}
			}

			current = next
		}
	}

	return nil
}

// getPathFromCache 从缓存获取路径解析结果
func getPathFromCache(path string) *pathCacheEntry {
	if v, ok := pathCache.Load(path); ok {
		return v.(*pathCacheEntry)
	}

	// 解析并缓存
	keys := parsePath(path)
	entry := &pathCacheEntry{keys: keys, error: nil}
	pathCache.Store(path, entry)

	// 简单的缓存清理策略（如果缓存过大）
	if cleanupCache() {
		// 清理后重新缓存
		pathCache.Store(path, entry)
	}

	return entry
}

// cleanupCache 清理缓存（简化版LRU）
func cleanupCache() bool {
	count := 0
	pathCache.Range(func(_, _ interface{}) bool {
		count++
		if count > cacheMaxSize {
			return false // 停止遍历，需要清理
		}
		return true
	})

	if count > cacheMaxSize {
		// 清理旧缓存（简化版：全部清空）
		pathCache = sync.Map{}
		return true
	}
	return false
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
				result.Set(key, value)
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
			result.Set(newKey, newValue)
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
				result.Set(key, nested.Copy())
			} else {
				result.Set(key, value)
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
			result.Set(key, value)
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

// KeyType 定义 table key 的类型
type KeyType int

const (
	KeyString KeyType = iota
	KeyNumber
	KeyBoolean
	KeyNil
	KeyInvalid
)

// GetKeyType 获取值的 key 类型
func GetKeyType(v interface{}) KeyType {
	switch v.(type) {
	case string:
		return KeyString
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return KeyNumber
	case bool:
		return KeyBoolean
	case nil:
		return KeyNil
	default:
		return KeyInvalid
	}
}

// NormalizeKey 标准化 key（转换为字符串用于存储）
func NormalizeKey(key interface{}) (interface{}, error) {
	if key == nil {
		return nil, fmt.Errorf("key 不能为 nil")
	}

	switch v := key.(type) {
	case string:
		return v, nil
	case int64, int32, int16, int8, int, uint64, uint32, uint16, uint8, uint:
		return v, nil
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case bool:
		return v, nil
	default:
		return nil, fmt.Errorf("不支持的 key 类型: %T", key)
	}
}

// FormatKey 格式化 key 用于显示
func FormatKey(key interface{}) string {
	if key == nil {
		return "nil"
	}

	switch v := key.(type) {
	case string:
		return fmt.Sprintf(`"%s"`, v)
	case int64, int32, int16, int8, int, uint64, uint32, uint16, uint8, uint:
		return fmt.Sprintf("%d", key)
	case float64, float32:
		return fmt.Sprintf("%f", key)
	case bool:
		return fmt.Sprintf("%t", key)
	default:
		return fmt.Sprintf("%v", key)
	}
}

// CompareKeys 比较两个 key 是否相等
func CompareKeys(k1, k2 interface{}) bool {
	if k1 == nil && k2 == nil {
		return true
	}
	if k1 == nil || k2 == nil {
		return false
	}

	if num1, ok1 := k1.(int64); ok1 {
		if num2, ok2 := k2.(int64); ok2 {
			return num1 == num2
		}
	}
	if num1, ok1 := k1.(float64); ok1 {
		if num2, ok2 := k2.(float64); ok2 {
			return num1 == num2
		}
	}

	if str1, ok1 := k1.(string); ok1 {
		if str2, ok2 := k2.(string); ok2 {
			return str1 == str2
		}
	}

	if bool1, ok1 := k1.(bool); ok1 {
		if bool2, ok2 := k2.(bool); ok2 {
			return bool1 == bool2
		}
	}

	return false
}
