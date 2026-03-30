package gin

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
)

var routerMap = make(map[string]func(ctx *gin.Context))

// 将一个struct中所有的函数注册到gin中
// class内的函数格式为:func (this *class) mname(request *http.Request) any {
// }
// any是返回的数据块可以转换成json的数据
func RegisterRouter(class interface{}) {
	sname, mnames := util.GetClassName(class)
	for _, mname := range mnames {
		callbackmsg := fmt.Sprintf("/%s/%s", strings.ToLower(sname), strings.ToLower(mname))
		routerMap[callbackmsg] = func(ctx *gin.Context) {
			//获取类数据
			rcvr := reflect.ValueOf(class)
			//获取函数反射
			method := rcvr.MethodByName(mname)
			//回调
			args := []reflect.Value{reflect.ValueOf(ctx.Request)}
			//调用函数
			result := method.Call(args)
			//回消息
			if len(result) > 0 {
				switch result[0].Kind() {
				case reflect.String:
					ctx.String(http.StatusOK, result[0].String())
				case reflect.Int,
					reflect.Int8,
					reflect.Int16,
					reflect.Int32,
					reflect.Int64:
					ctx.String(http.StatusOK, strconv.Itoa(int(result[0].Int())))
				case reflect.Float64,
					reflect.Float32:
					ctx.String(http.StatusOK, strconv.FormatFloat(result[0].Float(), 'f', 2, 64))
				case reflect.Bool:
					ctx.String(http.StatusOK, strconv.FormatBool(result[0].Bool()))
				case reflect.Ptr,
					reflect.Interface,
					reflect.Slice,
					reflect.Map,
					reflect.Struct:
					ctx.JSON(http.StatusOK, result[0].Interface())
				default:
					ctx.String(http.StatusOK, "success")
				}
			} else {
				ctx.String(http.StatusOK, "success")
			}
		}
	}
}

func Run() {
	if config.Cfg_.Web == nil || config.Cfg_.Web.HTTPPort == 0 {
		vars.Error("web服务未开启")
		return
	}
	ginServer := gin.Default()

	//将注册到这里的函数注册进去
	for router, fn := range routerMap {
		ginServer.Any(router, fn)
	}

	//挂静态文件夹
	if config.Cfg_.Web.Static != nil {
		ginServer.Static("/static", *config.Cfg_.Web.Static)
	}

	errChan := make(chan error, 1)
	go func() { //异步启动
		errChan <- ginServer.Run("[::]:" + strconv.Itoa(config.Cfg_.Web.HTTPPort))
	}()
	//将服务器名字注册到redis中
	if err := <-errChan; err != nil {
		vars.Error("web服务启动失败:%v", err)
		return
	}
	vars.Info("web服务启动成功,端口:%d", config.Cfg_.Web.HTTPPort)
}
