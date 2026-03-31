package websocket

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	UPGRADER_READ_BUFFER_SIZE  = 1024 * 1024 * 10
	UPGRADER_WRITE_BUFFER_SIZE = 1024 * 1024 * 10
)

var (
	serverList              []*http.Server = make([]*http.Server, 0)
	upgraderOnce            sync.Once
	upgrader                *websocket.Upgrader
	allowedOrigins          []string
	checkOrigin             bool
	skipOriginForIntranet   bool
)

// ============ 改进部分 ============

// ServerStats 服务器统计信息
// ============ 原有代码 ============

// initUpgrader 初始化 WebSocket Upgrader
func initUpgrader() {
	// 加载配置
	if config.Cfg_ != nil && config.Cfg_.Websocket != nil {
		allowedOrigins = config.Cfg_.Websocket.AllowedOrigins
		checkOrigin = config.Cfg_.Websocket.CheckOrigin
		skipOriginForIntranet = config.Cfg_.Websocket.SkipOriginForIntranet
	}

	upgrader = &websocket.Upgrader{
		ReadBufferSize:  UPGRADER_READ_BUFFER_SIZE,
		WriteBufferSize: UPGRADER_WRITE_BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			// 获取客户端IP
			clientIP := getClientIP(r)

			// 如果配置了内网跳过 Origin 检查，且客户端IP为内网IP，则直接允许
			if skipOriginForIntranet && util.IsIntranetIP(clientIP) {
				return true
			}

			// 如果不检查 Origin，直接允许
			if !checkOrigin {
				return true
			}

			// 获取 Origin 头
			origin := r.Header.Get("Origin")
			if origin == "" {
				// 没有 Origin 头，检查 Host
				host := r.Host
				return isAllowedOrigin(host, host)
			}

			// 解析 Origin
			originHost := strings.TrimPrefix(origin, "http://")
			originHost = strings.TrimPrefix(originHost, "https://")
			if idx := strings.Index(originHost, "/"); idx != -1 {
				originHost = originHost[:idx]
			}

			// 检查是否在白名单中
			return isAllowedOrigin(origin, originHost)
		},
	}
}

// isAllowedOrigin 检查 Origin 是否允许
func isAllowedOrigin(origin, host string) bool {
	// 白名单为空时，允许所有
	if len(allowedOrigins) == 0 {
		return true
	}

	// 检查是否在白名单中
	for _, allowed := range allowedOrigins {
		if allowed == "*" {
			return true
		}
		if allowed == origin || allowed == host {
			return true
		}
		// 支持通配符 *.example.com
		if strings.HasPrefix(allowed, "*.") {
			suffix := strings.TrimPrefix(allowed, "*.")
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}

	vars.Warning("WebSocket Origin 验证失败: %s (允许: %v)", origin, allowedOrigins)
	return false
}

func getClientIP(r *http.Request) string {
	// 优先从X-Forwarded-For解析第一个IP
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	// 次选X-Real-IP
	realIP := r.Header.Get("X-Real-IP")
	if realIP != "" {
		return realIP
	}
	// 最后从TCP连接获取（可能为代理IP）
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		vars.Error("获取客户端IP失败: %v", err)
		ip = "127.0.0.1"
	}
	return ip
}

// 监听端口
func ListenAndServe(port int, className string) error {
	// 初始化 upgrader（只执行一次）
	upgraderOnce.Do(initUpgrader)

	r := gin.Default()

	// 使用中间件将className存储到gin.Context中
	r.Use(func(c *gin.Context) {
		c.Set("className", className)
		c.Next()
	})
	
	r.GET("/ws", func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("WebSocket处理发生panic错误: %v", err)
			}
		}()
		var (
			wsConn *websocket.Conn
			err    error
		)
		// 完成ws协议的握手操作
		// Upgrade:websocket
		if wsConn, err = upgrader.Upgrade(c.Writer, c.Request, nil); err != nil {
			vars.Error("路径/ws链接错误: %v", err)
			http.NotFound(c.Writer, c.Request)
			return
		}

		// 从gin.Context中获取className
		classNameFromContext, exists := c.Get("className")
		if !exists {
			classNameFromContext = className // 如果不存在，使用传入的className
		}
		classNameStr := classNameFromContext.(string)
		
		_, err = NewClient(wsConn, getClientIP(c.Request), classNameStr)
		if err != nil {
			vars.Error("创建WebSocket客户端失败: %v", err)
			return
		}
	})

	//websocket实现ipv6
	server := &http.Server{
		Addr:    "[::]:" + strconv.Itoa(port),
		Handler: r,
	}

	go func() { //异步启动
		defer func() {
			if err := recover(); err != nil {
				vars.Error("WebSocket服务器发生panic错误: %v", err)
			}
		}()
		server.ListenAndServe()
	}()
	serverList = append(serverList, server)
	//将服务器名字注册到redis中
	return nil
}

// ============ 新增改进功能 ============

// IsIntranetIP 检查是否为内网IP（改进版本）
func IsIntranetIP(ip string) bool {
	return util.IsIntranetIP(ip)
}