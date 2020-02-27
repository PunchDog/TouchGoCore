package rpc

import (
	"time"

	"github.com/TouchGoCore/syncmap"
)

var classMap_ *syncmap.Map = &syncmap.Map{}

//启动监控
func Run() {
	classMap_.Range(func(k, v interface{}) bool {
		class_ := v.(IRpcClass)
		class_.Run()
		return true
	})
}
