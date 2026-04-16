package gin

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
)

var routerMap = make(map[string]func(ctx *gin.Context))

// ==================== 方法缓存优化 ====================

// methodCacheEntry 缓存反射方法调用信息
type methodCacheEntry struct {
	method      reflect.Value // 缓存的反射方法值
	argType     reflect.Type  // 参数类型（*http.Request 或 *gin.Context）
	returnKinds []reflect.Kind // 返回值类型缓存
}

// methodCache 全局方法缓存，避免每次请求都做反射查找
var methodCache sync.Map // map[string]*methodCacheEntry

// getMethodCacheEntry 获取或创建方法缓存条目
func getMethodCacheEntry(rcvr reflect.Value, sname, mname string) (*methodCacheEntry, error) {
	cacheKey := sname + "." + mname
	if entry, ok := methodCache.Load(cacheKey); ok {
		return entry.(*methodCacheEntry), nil
	}

	method := rcvr.MethodByName(mname)
	if !method.IsValid() {
		return nil, fmt.Errorf("method %s not found on %s", mname, sname)
	}

	methodType := method.Type()
	entry := &methodCacheEntry{
		method:  method,
		argType: methodType.In(0),
	}

	// 缓存返回值类型
	numOut := methodType.NumOut()
	entry.returnKinds = make([]reflect.Kind, numOut)
	for i := 0; i < numOut; i++ {
		entry.returnKinds[i] = methodType.Out(i).Kind()
	}

	methodCache.Store(cacheKey, entry)
	return entry, nil
}

// ==================== 路由注册 ====================

// RegisterRouter 将一个struct中所有的函数注册到gin中
// 支持两种函数签名：
//   - func (this *class) MethodName(request *http.Request) any
//   - func (this *class) MethodName(ctx *gin.Context) any  (推荐，可获取更多上下文)
func RegisterRouter(class interface{}) {
	sname, mnames := util.GetClassName(class)
	rcvr := reflect.ValueOf(class)

	for _, mname := range mnames {
		mnameCopy := mname // 闭包捕获
		callbackmsg := fmt.Sprintf("/%s/%s", strings.ToLower(sname), strings.ToLower(mnameCopy))

		// 预热方法缓存
		if _, err := getMethodCacheEntry(rcvr, sname, mnameCopy); err != nil {
			vars.Error("注册路由方法失败: %s.%s: %v", sname, mnameCopy, err)
			continue
		}

		routerMap[callbackmsg] = func(ctx *gin.Context) {
			// 从缓存获取方法信息
			entry, err := getMethodCacheEntry(rcvr, sname, mnameCopy)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			// 根据参数类型构造调用参数
			var args []reflect.Value
			if entry.argType.String() == "*gin.Context" {
				args = []reflect.Value{reflect.ValueOf(ctx)}
			} else {
				args = []reflect.Value{reflect.ValueOf(ctx.Request)}
			}

			// 调用函数
			result := entry.method.Call(args)

			// 回消息（使用预缓存的返回值类型）
			sendResponse(ctx, result, entry.returnKinds)
		}
	}
}

// sendResponse 根据返回值类型发送响应
func sendResponse(ctx *gin.Context, result []reflect.Value, returnKinds []reflect.Kind) {
	if len(result) == 0 {
		ctx.String(http.StatusOK, "success")
		return
	}

	switch returnKinds[0] {
	case reflect.String:
		ctx.String(http.StatusOK, result[0].String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		ctx.String(http.StatusOK, strconv.Itoa(int(result[0].Int())))
	case reflect.Float64, reflect.Float32:
		ctx.String(http.StatusOK, strconv.FormatFloat(result[0].Float(), 'f', 2, 64))
	case reflect.Bool:
		ctx.String(http.StatusOK, strconv.FormatBool(result[0].Bool()))
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Struct:
		ctx.JSON(http.StatusOK, result[0].Interface())
	default:
		ctx.String(http.StatusOK, "success")
	}
}

// ==================== 服务器启动 ====================

// Run 启动Gin HTTP服务
func Run() {
	if config.Cfg_.Web == nil || config.Cfg_.Web.HTTPPort == 0 {
		vars.Error("web服务未开启")
		return
	}
	ginServer := gin.Default()

	// 添加 Recovery 中间件（防止handler panic导致整个服务崩溃）
	ginServer.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		vars.Error("HTTP请求处理panic: %v, 路径: %s", recovered, c.Request.URL.Path)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal server error",
			"code":  500,
		})
	}))

	//将注册到这里的函数注册进去
	for router, fn := range routerMap {
		ginServer.Any(router, fn)
	}

	//挂静态文件夹
	if config.Cfg_.Web.Static != nil {
		ginServer.Static("/static", *config.Cfg_.Web.Static)
	}

	// 异步启动Gin服务器，避免阻塞主流程
	go func() {
		addr := "[::]:" + strconv.Itoa(config.Cfg_.Web.HTTPPort)
		if err := ginServer.Run(addr); err != nil {
			vars.Error("web服务运行出错:%v", err)
		}
	}()
	vars.Info("web服务启动成功,端口:%d", config.Cfg_.Web.HTTPPort)
}
