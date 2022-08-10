package util

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

//RandomStr 随机生成字符串
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

//格式化，把所有?按格式化数据替换类型
func Sprintf(format string, a ...interface{}) string {
	s := format
	theOne := 0
	idx := strings.Index(s, "?")
	for idx != -1 {
		if theOne >= len(a) {
			break
		}
		switch a[theOne].(type) {
		case string:
			s = strings.Replace(s, "?", "%s", 1)
		case float32:
			s = strings.Replace(s, "?", "%f", 1)
		case float64:
			s = strings.Replace(s, "?", "%f", 1)
		default:
			s = strings.Replace(s, "?", "%d", 1)
		}
		theOne++
		idx = strings.Index(s, "?")
	}
	return fmt.Sprintf(s, a...)
}
