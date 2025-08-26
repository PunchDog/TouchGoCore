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

// RandomStr 随机生成字符串
func RandomStr(length int) string {
	str := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	bytes := []byte(str)
	result := []byte{}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < length; i++ {
		result = append(result, bytes[r.Intn(len(bytes))])
	}
	return string(result)
}

// 格式化，把所有?按格式化数据替换类型主要用于数据库
func Sprintf(format string, a ...interface{}) string {
	var builder strings.Builder
	buf := make([]interface{}, 0, len(a))
	pattern := []byte(format)
	argIndex := 0

	// 预处理参数类型
	types := make([]string, len(a))
	for i, v := range a {
		switch v.(type) {
		case string:
			types[i] = "'%s'"
		case time.Time:
			types[i] = "'%s'"
			a[i] = v.(time.Time).Format("2006-01-02 15:04:05")
		case float32, float64:
			types[i] = "%f"
		default:
			types[i] = "%d"
		}
	}

	// 单次遍历构建格式字符串
	for i := 0; i < len(pattern); i++ {
		if pattern[i] == '?' && argIndex < len(types) {
			builder.WriteString(types[argIndex])
			buf = append(buf, a[argIndex])
			argIndex++
		} else {
			builder.WriteByte(pattern[i])
		}
	}

	return fmt.Sprintf(builder.String(), buf...)
}

// 设置默认值
func setDefaultValue(des interface{}) {
	switch d := des.(type) {
	case *bool:
		*d = false
	case *int, *int8, *int16, *int32, *int64, *uint, *uint8, *uint16, *uint32, *uint64, *float32, *float64:
		// 使用反射设置整数类型默认值为0
		reflect.ValueOf(d).Elem().Set(reflect.Zero(reflect.ValueOf(d).Elem().Type()))
	case *string:
		*d = ""
	case *time.Time:
		*d = time.Now().UTC() // 零值时间
	}
}

// 转换为布尔值
func toBool(src interface{}) bool {
	switch v := src.(type) {
	case bool:
		return v
	case string:
		if v == "" || v == "0" || v == "false" || v == "False" || v == "FALSE" {
			return false
		}
		return true
	case float32, float64:
		s := fmt.Sprintf("%f", v)
		val, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return false
		}
		return val != 0
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		s := fmt.Sprintf("%d", v)
		val, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return false
		}
		return val != 0
	case time.Time:
		// 非零时间视为true
		return !v.IsZero()
	default:
		return false
	}
}

// 转换为整数
func toInt(src interface{}) int64 {
	switch v := src.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return int64(reflect.ValueOf(v).Int())
	case float32, float64:
		return int64(reflect.ValueOf(v).Float())
	case string:
		if v == "" {
			// log.ZError(context.TODO(), "空整数字符串", nil)
			return 0
		}
		val, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			// log.ZError(context.TODO(), "整数转换失败", err, "value", v)
			return 0
		}
		return val
	case bool:
		if v {
			return 1
		}
		return 0
	case time.Time:
		return int64(v.UTC().UnixMilli())
	default:
		return 0
	}
	return 0
}

// 转换为float64
func toFloat64(src interface{}) float64 {
	switch v := src.(type) {
	case float32:
		return float64(v)
	case float64:
		return v
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		s := fmt.Sprintf("%d", v)
		val, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return val
	case string:
		if v == "" {
			// log.ZError(context.TODO(), "空浮点数字符串", nil)
			return 0
		}
		val, err := strconv.ParseFloat(v, 64)
		if err != nil {
			// log.ZError(context.TODO(), "浮点数转换失败", err, "value", v)
			return 0
		}
		return val
	case bool:
		if v {
			return 1.0
		}
		return 0.0
	case time.Time:
		return float64(v.UTC().UnixMilli())
	default:
		return 0.0
	}
}

// 转换为字符串
func toString(src interface{}) string {
	switch v := src.(type) {
	case string:
		return v
	case float32, float64:
		return fmt.Sprintf("%f", v)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	case bool:
		return fmt.Sprintf("%t", v)
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// 转换为时间
func toTime(src interface{}) time.Time {
	switch v := src.(type) {
	case time.Time:
		return v
	case string:
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
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		s := fmt.Sprintf("%d", v)
		t, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.UnixMilli(t).UTC()
	default:
		return time.Now().UTC()
	}
}

// ParseDbData 将不同类型的数据源(src)转换为目标类型(des)并赋值给目标变量
// des 必须是指针类型，用于接收转换后的值
// src 可以是多种类型，函数会尝试将其转换为des对应的类型
func ParseDbData(des, src interface{}) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("类型转换异常", fmt.Errorf("%v", r))
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
	case interface{}:
		if reflect.TypeOf(des).Elem().Kind() == reflect.Struct {
			buf, _ := json.Marshal(src)
			if err := json.Unmarshal(buf, des); err != nil {
				vars.Error("结构体转换失败", err)
			}
		}
	}
}
