package gin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"touchgocore/corectx"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

var (
	routerMap  = make(map[string]func(ctx *gin.Context))
	httpServer *http.Server
	httpMu     sync.Mutex
)

// ==================== 方法缓存优化 ====================

// methodCacheEntry 缓存反射方法调用信息
type methodCacheEntry struct {
	method      reflect.Value  // 缓存的反射方法值
	argType     reflect.Type   // 参数类型（*http.Request 或 *gin.Context）
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

// 注册路由时必须传入类型：比如GET，POST
type IRouterInterface interface {
	RouterType() []string
}

// RegisterRouter 将一个struct中所有的函数注册到gin中
// 支持两种函数签名：
//   - func (this *class) MethodName(request *http.Request) any
//   - func (this *class) MethodName(ctx *gin.Context) any  (推荐，可获取更多上下文)
func RegisterRouter(class IRouterInterface) {
	sname, mnames := util.GetClassName(class)
	rcvr := reflect.ValueOf(class)

	for _, mname := range mnames {
		//这个是类型，不进行router注册
		if mname == "RouterType" {
			continue
		}

		mnameCopy := mname // 闭包捕获
		callbackmsg := fmt.Sprintf("/%s/%s", strings.ToLower(sname), strings.ToLower(mnameCopy))
		if s := class.RouterType(); s != nil && len(s) > 0 { //设置了只注册哪些监控
			callbackmsg += "|" + strings.Join(s, "&&")
		}

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
func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.Web == nil || cfg.Web.HTTPPort == 0 {
		vars.Error("web服务未开启")
		return nil
	}
	ginServer := gin.Default()

	ginServer.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		vars.Error("HTTP请求处理panic: %v, 路径: %s", recovered, c.Request.URL.Path)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "internal server error",
			"code":  500,
		})
	}))

	origins := []string{}
	if cfg.Web != nil {
		origins = cfg.Web.AllowOrigins
	}
	if len(origins) == 0 {
		vars.Warning("Gin CORS 未配置 allow_origins，拒绝跨域请求")
	} else if len(origins) == 1 && origins[0] == "*" {
		ginServer.Use(cors.Default())
	} else {
		corsCfg := cors.DefaultConfig()
		corsCfg.AllowOrigins = origins
		corsCfg.AllowCredentials = false
		ginServer.Use(cors.New(corsCfg))
	}

	for router, fn := range routerMap {
		r := strings.Split(router, "|")
		if len(r) == 1 {
			ginServer.Any(router, fn)
		} else {
			ss := strings.Split(r[1], "&&")
			for _, v := range ss {
				if v == "GET" {
					ginServer.GET(r[0], fn)
				}
				if v == "POST" {
					ginServer.POST(r[0], fn)
				}
				if v == "PUT" {
					ginServer.PUT(r[0], fn)
				}
			}
		}
	}

	if cfg.Web.Static != nil {
		ginServer.Static("/static", *cfg.Web.Static)
	}

	addr := "[::]:" + strconv.Itoa(cfg.Web.HTTPPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: ginServer,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	httpMu.Lock()
	httpServer = srv
	httpMu.Unlock()

	useTLS := cfg.Web.TLS != nil && cfg.Web.TLS.Enable
	errCh := make(chan error, 1)
	go func() {
		var err error
		if useTLS {
			err = srv.ListenAndServeTLS(cfg.Web.TLS.CertFile, cfg.Web.TLS.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			vars.Error("web服务运行出错:%v", err)
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(100 * time.Millisecond):
	}
	if useTLS {
		vars.Info("web服务启动成功(HTTPS),端口:%d", cfg.Web.HTTPPort)
	} else {
		vars.Info("web服务启动成功,端口:%d", cfg.Web.HTTPPort)
	}
	return nil
}

func Stop(ctx context.Context) error {
	httpMu.Lock()
	srv := httpServer
	httpMu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
