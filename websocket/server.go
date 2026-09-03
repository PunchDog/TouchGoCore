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
	serverList            []*http.Server = make([]*http.Server, 0)
	upgraderOnce          sync.Once
	upgrader              *websocket.Upgrader
	allowedOrigins        []string
	checkOrigin           bool
	skipOriginForIntranet bool
)

// ============ 改进部分 ============

// ServerStats 服务器统计信息
// ============ 原有代码 ============

// initUpgrader 初始化 WebSocket Upgrader
func initUpgrader() {
	// 加载配置
	if config.Cfg_ != nil && config.Cfg_.Ws != nil {
		allowedOrigins = config.Cfg_.Ws.AllowedOrigins
		checkOrigin = config.Cfg_.Ws.CheckOrigin
		skipOriginForIntranet = config.Cfg_.Ws.SkipOriginForIntranet
	}

	upgrader = &websocket.Upgrader{
		ReadBufferSize:  UPGRADER_READ_BUFFER_SIZE,
		WriteBufferSize: UPGRADER_WRITE_BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			clientIP := getDirectIP(r)

			if skipOriginForIntranet && util.IsIntranetIP(clientIP) {
				return true
			}

			if !checkOrigin {
				vars.Warning("WebSocket CheckOrigin 未启用，允许任意 Origin")
				return true
			}

			origin := r.Header.Get("Origin")
			if origin == "" {
				host := r.Host
				return isAllowedOrigin(host, host)
			}

			originHost := strings.TrimPrefix(origin, "http://")
			originHost = strings.TrimPrefix(originHost, "https://")
			if idx := strings.Index(originHost, "/"); idx != -1 {
				originHost = originHost[:idx]
			}

			return isAllowedOrigin(origin, originHost)
		},
	}
}

// isAllowedOrigin 检查 Origin 是否允许
func isAllowedOrigin(origin, host string) bool {
	if len(allowedOrigins) == 0 {
		vars.Warning("WebSocket Origin 白名单为空，拒绝: %s", origin)
		return false
	}

	for _, allowed := range allowedOrigins {
		if allowed == "*" {
			return true
		}
		if allowed == origin || allowed == host {
			return true
		}
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

func getDirectIP(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func isTrustedProxy(r *http.Request) bool {
	if config.Cfg_ == nil || config.Cfg_.Ws == nil || len(config.Cfg_.Ws.TrustedProxies) == 0 {
		return false
	}
	ip := getDirectIP(r)
	for _, p := range config.Cfg_.Ws.TrustedProxies {
		if p == ip || p == "*" {
			return true
		}
	}
	return false
}

func getClientIP(r *http.Request) string {
	if isTrustedProxy(r) {
		xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if xff != "" {
			ips := strings.Split(xff, ",")
			if len(ips) > 0 {
				return strings.TrimSpace(ips[0])
			}
		}
		realIP := r.Header.Get("X-Real-IP")
		if realIP != "" {
			return realIP
		}
	}
	return getDirectIP(r)
}

// extractAuthToken 从请求中提取认证令牌
// 优先从 HTTP Header 读取，其次从 URL Query 参数读取
func extractAuthToken(c *gin.Context) string {
	wsCfg := config.Cfg_.Ws
	if wsCfg == nil {
		return ""
	}

	// 从 Header 读取
	headerName := wsCfg.AuthTokenHeader
	if headerName == "" {
		headerName = "X-Auth-Token"
	}
	if token := c.GetHeader(headerName); token != "" {
		return token
	}

	if wsCfg.AuthTokenQuery != "" {
		if token := c.Query(wsCfg.AuthTokenQuery); token != "" {
			return token
		}
	}

	return ""
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

		// ========== 连接认证 ==========
		if authFn := GetAuthFunc(); authFn != nil {
			clientIP := getClientIP(c.Request)
			directIP := getDirectIP(c.Request)

			if config.Cfg_.Ws.AuthIntranetSkip && util.IsIntranetIP(directIP) {
				// 仅以直连 IP 判断内网，避免伪造 XFF 绕过认证
			} else {
				// 从配置的 Header 或 Query 参数中提取 Token
				token := extractAuthToken(c)
				if token == "" {
					vars.Warning("WebSocket连接认证失败: 缺少认证令牌, IP=%s", clientIP)
					c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required", "code": 401})
					return
				}
				if !authFn(token, clientIP) {
					vars.Warning("WebSocket连接认证失败: 令牌无效, IP=%s", clientIP)
					c.JSON(http.StatusForbidden, gin.H{"error": "authentication failed", "code": 403})
					return
				}
			}
		}
		// ========== 认证结束 ==========

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
		// NewClient 成功后会接管 wsConn 的生命周期（包括错误时的关闭）。
		// 若 NewClient 返回错误，说明客户端未通过认证或初始化失败，此时手动关闭连接。
		if _, err = NewClient(wsConn, getClientIP(c.Request), classNameStr); err != nil {
			vars.Error("创建WebSocket客户端失败: %v", err)
			wsConn.Close()
			return
		}
	})

	ln, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:    ln.Addr().String(),
		Handler: r,
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("WebSocket服务器发生panic错误: %v", err)
			}
		}()
		tlsCfg := config.Cfg_.Ws.TLS
		var err error
		if tlsCfg != nil && tlsCfg.Enable {
			vars.Info("WebSocket TLS 已启用，监听 wss://%s", ln.Addr().String())
			err = server.ServeTLS(ln, tlsCfg.CertFile, tlsCfg.KeyFile)
		} else {
			err = server.Serve(ln)
		}
		if err != nil && err != http.ErrServerClosed {
			vars.Error("WebSocket ListenAndServe 错误: %v", err)
		}
	}()
	serverList = append(serverList, server)
	return nil
}

// ============ 新增改进功能 ============

// IsIntranetIP 检查是否为内网IP（改进版本）
func IsIntranetIP(ip string) bool {
	return util.IsIntranetIP(ip)
}
