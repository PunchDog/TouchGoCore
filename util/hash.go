package util

import (
	"crypto/md5"
	"encoding/hex"
)

// MD5 实现 :主要是针对 字符串的加密
//
// Deprecated: 仓内零调用，仅为兼容既有外部用法保留。MD5 不适用于签名与口令
// 场景，仅可作为内容指纹。
func MD5(data string) string {
	h := md5.New()
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}
