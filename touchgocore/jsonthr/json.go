package jsonthr

import jsoniter "github.com/json-iterator/go"

//json请一定使用这个来做序列化，效率是golang 原生json的3倍
var Json = jsoniter.ConfigCompatibleWithStandardLibrary
