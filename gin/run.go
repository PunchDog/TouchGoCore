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
	routerMap = make(map[string]func(ctx *gin.Context))
	// routerMu 保护 routerMap：RegisterRouter 可能在 Run 之后由业务动态调用，
	// 而 Run 会整表遍历注册路由。
	routerMu   sync.Mutex
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
// key = 类型名.方法名#接收者地址：同名类型的不同实例必须各自缓存，
// 否则后注册的实例会覆盖前者（路由错乱），且缓存会长期钉住先注册的实例。
var methodCache sync.Map // map[string]*methodCacheEntry

// receiverKey 返回接收者的身份标识；非指针接收者退化为类型名。
func receiverKey(rcvr reflect.Value) string {
	if rcvr.Kind() == reflect.Ptr {
		return strconv.FormatUint(uint64(rcvr.Pointer()), 16)
	}
	return rcvr.Type().String()
}

// getMethodCacheEntry 获取或创建方法缓存条目
func getMethodCacheEntry(rcvr reflect.Value, sname, mname string) (*methodCacheEntry, error) {
	cacheKey := sname + "." + mname + "#" + receiverKey(rcvr)
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
//   - func (this *class) MethodName(ctx *gin.Context) any  (推荐，可获取更多上下文)
//   - timeout map[string]int64 自定义ctx超时时间
func RegisterRouter(class IRouterInterface, timeoutmap map[string]int64) {
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

		handler := func(ctx *gin.Context) {
			// 默认 15s，避免慢接口拖死 worker；CheckUrlNow 批量探测外网，单独放宽到 60s
			reqTimeout := 15 * time.Second
			if sec, h := timeoutmap[ctx.FullPath()]; h {
				reqTimeout = time.Duration(sec) * time.Second
			}

			ctxnew, cancel := context.WithTimeout(ctx.Request.Context(), reqTimeout)
			defer cancel() // 必须调用，释放资源

			// 将新的 ctx 写入请求
			ctx.Request = ctx.Request.WithContext(ctxnew)

			// 从缓存获取方法信息
			entry, err := getMethodCacheEntry(rcvr, sname, mnameCopy)
			if err != nil {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			// 根据参数类型构造调用参数
			var args []reflect.Value
			args = []reflect.Value{reflect.ValueOf(ctx)}

			// 调用函数
			result := entry.method.Call(args)

			// 回消息（使用预缓存的返回值类型）
			sendResponse(ctx, result, entry.returnKinds)
		}

		routerMu.Lock()
		if _, dup := routerMap[callbackmsg]; dup {
			// URL 由「类型名/方法名」推导，同类型的第二个实例必然与第一个撞同一批路径，
			// 这里以后者覆盖前者；需要两套实例并存请拆分类型或用不同路由前缀。
			vars.Error("路由 %s 重复注册（同名类型的多个实例），本次注册的实例将覆盖先前实例", callbackmsg)
		}
		routerMap[callbackmsg] = handler
		routerMu.Unlock()
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
	ginServer := gin.New()
	// 用自定义 logger 桥接（见 ginLogger），取代 gin.Default() 自带的 Logger()：
	// 后者直接写 stdout，高并发下因共享锁被串行化，且脱离项目统一日志体系。
	ginServer.Use(ginLogger())

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

	routerMu.Lock()
	routes := make(map[string]func(ctx *gin.Context), len(routerMap))
	for k, v := range routerMap {
		routes[k] = v
	}
	routerMu.Unlock()

	for router, fn := range routes {
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
	useTLS := cfg.Web.TLS != nil && cfg.Web.TLS.Enable
	// 先占端口再报成功：ListenAndServe 在 goroutine 里异步失败时，
	// 靠 sleep 等错误会既误报「启动成功」又漏掉端口占用。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("web服务监听失败[%s]: %v", addr, err)
		return err
	}
	srv := &http.Server{
		Handler: ginServer,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}
	httpMu.Lock()
	httpServer = srv
	httpMu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		var err error
		if useTLS {
			err = srv.ServeTLS(ln, cfg.Web.TLS.CertFile, cfg.Web.TLS.KeyFile)
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			vars.Error("web服务运行出错:%v", err)
			errCh <- err
		}
	}()
	// Serve 的失败（证书读不到等）会立刻回炉，这里给一次短窗口确认，
	// 之后端口已绑定，启动结论不再依赖等待时长。
	select {
	case err := <-errCh:
		// Serve/ServeTLS 失败时不会替调用方关监听器：不释放就把端口永久占住，
		// 一次配置写错就得重启整个进程才能再起来。
		_ = ln.Close()
		httpMu.Lock()
		// 只摘自己：期间可能有另一次 Run 已经装上了新 server
		if httpServer == srv {
			httpServer = nil
		}
		httpMu.Unlock()
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

// ginLogger 将 gin 的访问日志桥接到项目的 vars 日志系统。
// 取代 gin.Default() 自带的 Logger()（直接写 stdout，高并发下因共享锁被串行化）。
// 访问日志量大，2xx 成功请求不再逐条输出（1 万并发时 fmt 锁会把延迟打穿）；
// 仅记录 4xx/5xx 或 gin 私有错误，走 vars.Debug。
func ginLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery
		c.Next()

		if raw != "" {
			path = path + "?" + raw
		}
		status := c.Writer.Status()
		clientIP := c.ClientIP()
		method := c.Request.Method
		errMsg := c.Errors.ByType(gin.ErrorTypePrivate).String()
		if status < 400 && errMsg == "" {
			return
		}

		// 先自行 Sprintf，避免把用户可控的路径/错误信息当作格式串传给 vars，
		// 也避免二次格式化。vars.Debug 在 0 个变参时直接原样输出，安全。
		var line string
		if errMsg != "" {
			line = fmt.Sprintf("[GIN] %3d | %13v | %15s | %-7s %s | %s",
				status, time.Since(start), clientIP, method, path, errMsg)
		} else {
			line = fmt.Sprintf("[GIN] %3d | %13v | %15s | %-7s %s",
				status, time.Since(start), clientIP, method, path)
		}
		vars.Debug(line)
	}
}

func Stop(ctx context.Context) error {
	httpMu.Lock()
	srv := httpServer
	// 置空：已关掉的 server 不能再被第二次 Stop（或 Run 之后的清理）复用
	httpServer = nil
	httpMu.Unlock()
	if srv == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return srv.Shutdown(ctx)
}
