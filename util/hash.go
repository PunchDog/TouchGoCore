package util

import (
	"crypto/md5"
	"encoding/hex"
)

// MD5 实现 :主要是针对 字符串的加密
func MD5(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}
