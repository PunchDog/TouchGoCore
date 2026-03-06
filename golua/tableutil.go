package lua

import (
	"fmt"
)

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

	// 尝试数值比较
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

	// 字符串比较
	if str1, ok1 := k1.(string); ok1 {
		if str2, ok2 := k2.(string); ok2 {
			return str1 == str2
		}
	}

	// 布尔比较
	if bool1, ok1 := k1.(bool); ok1 {
		if bool2, ok2 := k2.(bool); ok2 {
			return bool1 == bool2
		}
	}

	return false
}
