package util

import (
	"fmt"
	"reflect"
	"touchgocore/vars"
)

// ConvertToKind 安全转换数值类型
func ConvertToKind(val float64, kind reflect.Kind) interface{} {
	switch kind {
	case reflect.Int:
		return int(val)
	case reflect.Int8:
		return int8(val)
	case reflect.Int16:
		return int16(val)
	case reflect.Int32:
		return int32(val)
	case reflect.Int64:
		return int64(val)
	case reflect.Uint:
		return uint(val)
	case reflect.Uint8:
		return uint8(val)
	case reflect.Uint16:
		return uint16(val)
	case reflect.Uint32:
		return uint32(val)
	case reflect.Uint64:
		return uint64(val)
	case reflect.Float32:
		return float32(val)
	case reflect.Float64:
		return val
	default:
		vars.Error(fmt.Sprintf("无法转换数值到类型：%s,值:%v", kind.String(), val))
		return 0
	}
}
