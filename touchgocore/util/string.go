package util

import (
	"crypto/md5"
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

// MD5Sum 计算 32 位长度的 MD5 sum
func MD5Sum(txt string) (sum string) {
	data2 := []byte(txt)
	has := md5.Sum(data2)
	md5str1 := fmt.Sprintf("%x", has) //将[]byte转成16进制
	return md5str1
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
