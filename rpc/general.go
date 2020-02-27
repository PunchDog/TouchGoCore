package rpc

import (
	"time"

	"github.com/TouchGoCore/syncmap"
)

//注册rpc功能接口
type IRpcClass interface {
	Run()
}

var classMap_ *syncmap.Map = &syncmap.Map{}

//启动监控
func Run() {
	go func() {
		//持续监听注册
		for {
			classMap_.Range(func(k, v interface{}) bool {
				class_ := v.(IRpcClass)
				class_.Run()
				return true
			})
			time.Sleep(time.Second)
		}
	}()
}
