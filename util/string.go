package util

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
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

// 格式化，把所有?按格式化数据替换类型
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
			types[i] = "%s"
		case time.Time:
			types[i] = "%s"
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
