package util

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"time"
	"touchgocore/vars"
)

// RandomStr 生成指定长度的随机字符串
func RandomStr(length int) string {
	const charset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	result := make([]byte, length)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := range result {
		result[i] = charset[r.Intn(len(charset))]
	}
	return string(result)
}

// Sprintf 格式化字符串，将?占位符替换为格式化数据，主要用于数据库操作
func Sprintf(format string, a ...interface{}) string {
	if len(a) == 0 {
		return format
	}

	var builder strings.Builder
	buf := make([]interface{}, 0, len(a))
	argIndex := 0

	// 单次遍历构建格式字符串和参数列表
	for i := 0; i < len(format); i++ {
		if format[i] == '?' && argIndex < len(a) {
			switch v := a[argIndex].(type) {
			case string:
				builder.WriteString("'%s'")
				buf = append(buf, v)
			case time.Time:
				builder.WriteString("'%s'")
				buf = append(buf, v.Format("2006-01-02 15:04:05"))
			case float32, float64:
				builder.WriteString("%f")
				buf = append(buf, v)
			default:
				builder.WriteString("%d")
				buf = append(buf, v)
			}
			argIndex++
		} else {
			builder.WriteByte(format[i])
		}
	}

	return fmt.Sprintf(builder.String(), buf...)
}

// setDefaultValue 设置各种类型的默认值
func setDefaultValue(des interface{}) {
	switch d := des.(type) {
	case *bool:
		*d = false
	case *int, *int8, *int16, *int32, *int64, *uint, *uint8, *uint16, *uint32, *uint64, *float32, *float64:
		reflect.ValueOf(d).Elem().Set(reflect.Zero(reflect.ValueOf(d).Elem().Type()))
	case *string:
		*d = ""
	case *time.Time:
		*d = time.Time{} // 使用零值时间而不是当前时间
	}
}

// toBool 将任意类型转换为布尔值
func toBool(src interface{}) bool {
	switch v := src.(type) {
	case bool:
		return v
	case string:
		v = strings.ToLower(strings.TrimSpace(v))
		return v != "" && v != "0" && v != "false"
	case float32, float64:
		return reflect.ValueOf(v).Float() != 0
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return reflect.ValueOf(v).Int() != 0
	case time.Time:
		return !v.IsZero()
	default:
		return false
	}
}

// toInt 将任意类型转换为int64
func toInt(src interface{}) int64 {
	switch v := src.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return reflect.ValueOf(v).Int()
	case float32, float64:
		return int64(reflect.ValueOf(v).Float())
	case string:
		if v = strings.TrimSpace(v); v == "" {
			return 0
		}
		val, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return val
	case bool:
		if v {
			return 1
		}
		return 0
	case time.Time:
		return v.UnixMilli()
	default:
		return 0
	}
}

// toFloat64 将任意类型转换为float64
func toFloat64(src interface{}) float64 {
	switch v := src.(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return float64(reflect.ValueOf(v).Int())
	case string:
		if v = strings.TrimSpace(v); v == "" {
			return 0
		}
		val, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return val
	case bool:
		if v {
			return 1.0
		}
		return 0.0
	case time.Time:
		return float64(v.UnixMilli())
	default:
		return 0.0
	}
}

// toString 将任意类型转换为字符串
func toString(src interface{}) string {
	switch v := src.(type) {
	case string:
		return v
	case float32, float64:
		return strconv.FormatFloat(reflect.ValueOf(v).Float(), 'f', -1, 64)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return strconv.FormatInt(reflect.ValueOf(v).Int(), 10)
	case bool:
		return strconv.FormatBool(v)
	case time.Time:
		return v.Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// toTime 将任意类型转换为时间
func toTime(src interface{}) time.Time {
	switch v := src.(type) {
	case time.Time:
		return v
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}
		}

		// 尝试多种时间格式解析
		formats := []string{
			time.RFC3339,
			"2006-01-02 15:04:05",
			"2006-01-02",
		}

		for _, format := range formats {
			t, err := time.Parse(format, v)
			if err == nil {
				return t
			}
		}
		return time.Time{}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return time.UnixMilli(reflect.ValueOf(v).Int())
	case float32, float64:
		return time.UnixMilli(int64(reflect.ValueOf(v).Float()))
	default:
		return time.Time{}
	}
}

// ParseDbData 将不同类型的数据源转换为目标类型并赋值给目标变量
// des: 必须是指针类型，用于接收转换后的值
// src: 可以是多种类型，函数会尝试将其转换为des对应的类型
func ParseDbData(des, src interface{}) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("ParseDbData类型转换异常", fmt.Errorf("%v", r))
		}
	}()

	// 处理nil值，根据目标类型设置合适的默认值
	if src == nil {
		setDefaultValue(des)
		return
	}

	// 根据目标类型进行转换
	switch d := des.(type) {
	case *bool:
		*d = toBool(src)
	case *int, *int8, *int16, *int32, *int64, *uint, *uint8, *uint16, *uint32, *uint64:
		val := reflect.ValueOf(toInt(src)).Convert(reflect.ValueOf(d).Elem().Type())
		reflect.ValueOf(d).Elem().Set(val)
	case *float32, *float64:
		val := reflect.ValueOf(toFloat64(src)).Convert(reflect.ValueOf(d).Elem().Type())
		reflect.ValueOf(d).Elem().Set(val)
	case *string:
		*d = toString(src)
	case *time.Time:
		*d = toTime(src)
	default:
		// 处理结构体类型
		if reflect.TypeOf(des).Elem().Kind() == reflect.Struct {
			buf, err := json.Marshal(src)
			if err != nil {
				vars.Error("结构体序列化失败", err)
				return
			}
			if err := json.Unmarshal(buf, des); err != nil {
				vars.Error("结构体反序列化失败", err)
			}
		}
	}
}
