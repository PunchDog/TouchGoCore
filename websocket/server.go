package websocket

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	// 握手期读写缓冲只是 TCP 读写的暂存缓冲，不是单条消息的大小上限。
	// 原先两条各 10MiB： gorilla 在升级时就按这个值分配 bufio 缓冲，
	// 半开握手一大片就是纯内存型 DoS。默认 4KiB，需要时按配置放大。
	defaultUpgraderBufferSize = 4 * 1024
	// 单条消息默认上限（字节）；配置 server.max_msg_size 或 ws.max_message_size 时以其为准
	defaultMaxMessageSize = 1 << 20
)

// serverListMu 保护 serverList：ListenAndServe 追加与 shutdownWebsocket 摘取
// 原先在同一把锁都没有的全局切片上读写，停机与启动并发时会漏掉监听器
// （永不 Shutdown，端口占到进程退出）或读到正在改写的切片头。
var (
	serverListMu sync.Mutex
	serverList   = make([]*http.Server, 0)
)

// registerServer 登记一个监听器
func registerServer(srv *http.Server) {
	serverListMu.Lock()
	serverList = append(serverList, srv)
	serverListMu.Unlock()
}

// takeServers 取出并清空全部已登记的监听器
func takeServers() []*http.Server {
	serverListMu.Lock()
	defer serverListMu.Unlock()
	out := serverList
	serverList = make([]*http.Server, 0)
	return out
}

// originPolicy 是 Origin 校验所需的配置快照。
//
// 收进结构体而不是留四个包级全局：upgrader 现在按 Run 构造，
// 校验规则随之跟着实例走，测试之间不再互相污染。
type originPolicy struct {
	allowedOrigins        []string
	checkOrigin           bool
	skipOriginForIntranet bool
	// allowEmptyOrigin 决定「不带 Origin 的握手」是否放行，默认关闭
	allowEmptyOrigin bool
}

// ============ 改进部分 ============

// ServerStats 服务器统计信息
// ============ 原有代码 ============

// wsCfg 返回当前生命周期内的 WebSocket 配置快照（未配置时为 nil）
func wsCfg() *config.WebsocketConfig {
	cfg := corectx.CfgFrom(wsRunCtx)
	if cfg == nil {
		return nil
	}
	return cfg.Ws
}

func websocketPath(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err == nil && u.Path != "" && u.Path != "/" {
			return u.Path
		}
		return fallback
	}
	if !strings.HasPrefix(raw, "/") {
		return "/" + raw
	}
	return raw
}

func websocketListenPaths(ws *config.WebsocketConfig) []string {
	paths := []string{websocketPath("", "/ws")}
	if ws != nil {
		paths = []string{websocketPath(ws.URL, "/ws")}
		if in := websocketPath(ws.InURL, ""); in != "" && in != paths[0] {
			paths = append(paths, in)
		}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// newUpgrader 按当前配置构造握手用的 Upgrader。
//
// 每次 ListenAndServe 都新建，不再走包级 sync.Once：Once 把首次调用时的配置
// 永久钉死，同进程内换配置（以及单元测试之间的相互污染）都不会再生效。
func newUpgrader(ws *config.WebsocketConfig) *websocket.Upgrader {
	policy := originPolicy{}
	readBuf, writeBuf := defaultUpgraderBufferSize, defaultUpgraderBufferSize
	if ws != nil {
		policy = originPolicy{
			allowedOrigins:        ws.AllowedOrigins,
			checkOrigin:           ws.CheckOrigin,
			skipOriginForIntranet: ws.SkipOriginForIntranet,
			allowEmptyOrigin:      ws.AllowEmptyOrigin,
		}
		if ws.UpgraderReadBuffer > 0 {
			readBuf = ws.UpgraderReadBuffer
		}
		if ws.UpgraderWriteBuffer > 0 {
			writeBuf = ws.UpgraderWriteBuffer
		}
	}

	return &websocket.Upgrader{
		ReadBufferSize:  readBuf,
		WriteBufferSize: writeBuf,
		CheckOrigin:     policy.check,
	}
}

// currentMaxMessageSize 返回单条消息上限（字节）。
//
// 修复前 SetReadLimit 硬编码 1MiB，与 server.max_msg_size 完全脱钩：
// 业务把包上限调到 4MiB 就会在传输层被静默断连。
func currentMaxMessageSize() int64 {
	if ws := wsCfg(); ws != nil && ws.MaxMessageSize > 0 {
		return int64(ws.MaxMessageSize)
	}
	if cfg := corectx.CfgFrom(wsRunCtx); cfg != nil && cfg.Server != nil && cfg.Server.MaxMsgSize > 0 {
		return int64(cfg.Server.MaxMsgSize)
	}
	return defaultMaxMessageSize
}

// check 实现 gorilla 的 CheckOrigin 契约
func (p originPolicy) check(r *http.Request) bool {
	clientIP := getDirectIP(r)

	if p.skipOriginForIntranet && util.IsIntranetIP(clientIP) {
		return true
	}

	if !p.checkOrigin {
		vars.Warning("WebSocket CheckOrigin 未启用，允许任意 Origin")
		return true
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// 浏览器发起的跨站握手必定带 Origin，缺 Origin 的多是原生客户端/服务间调用。
		// 修复前这里退化成 isAllowedOrigin(r.Host, r.Host)——自己和自己比对，
		// 等于给任何不带 Origin 的请求开门，白名单形同虚设。
		if !p.allowEmptyOrigin {
			vars.Warning("WebSocket 拒绝不带 Origin 的握手: ip=%s（确需放行请配置 ws.allow_empty_origin）", clientIP)
			return false
		}
		return true
	}

	host, err := originHost(origin)
	if err != nil {
		vars.Warning("WebSocket Origin 非法: %q, err=%v", origin, err)
		return false
	}
	return p.allows(host)
}

// originHost 解析 URL 并返回小写 host（含端口）
func originHost(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	host := u.Host
	if host == "" {
		host = u.Path // 允许白名单里只写 "example.com:8080" 这种无 scheme 形式
	}
	if host == "" {
		return "", errors.New("origin 缺少主机名")
	}
	return strings.ToLower(host), nil
}

// allows 只做 host 级精确比对，通配项要求严格子域
func (p originPolicy) allows(host string) bool {
	if len(p.allowedOrigins) == 0 {
		vars.Warning("WebSocket Origin 白名单为空，拒绝: %s", host)
		return false
	}

	for _, allowed := range p.allowedOrigins {
		a := strings.TrimSpace(allowed)
		if a == "*" {
			return true
		}
		if strings.HasPrefix(a, "*.") {
			// 必须匹配「.后缀」而不是裸后缀：HasSuffix("evil-example.com",
			// "example.com") 为真，修复前这条旁路能把白名单域名拼在任意前缀后过检。
			suffix := strings.ToLower(strings.TrimPrefix(a, "*."))
			if host != suffix && strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		h, err := originHost(a)
		if err != nil {
			continue
		}
		if h == host {
			return true
		}
	}

	vars.Warning("WebSocket Origin 验证失败: %s (允许: %v)", host, p.allowedOrigins)
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
	ws := wsCfg()
	if ws == nil || len(ws.TrustedProxies) == 0 {
		return false
	}
	ip := getDirectIP(r)
	for _, p := range ws.TrustedProxies {
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
	wsCfg := wsCfg()
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
	ws := wsCfg()
	// 每次监听都按当前配置新建 Upgrader：全局 sync.Once 会把首次配置永久钉死
	upgrader := newUpgrader(ws)
	readLimit := currentMaxMessageSize()

	r := gin.Default()

	// 使用中间件将className存储到gin.Context中
	r.Use(func(c *gin.Context) {
		c.Set("className", className)
		c.Next()
	})

	handler := func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("WebSocket处理发生panic错误: %v", err)
			}
		}()

		// ========== 连接认证 ==========
		if authFn := GetAuthFunc(); authFn != nil {
			clientIP := getClientIP(c.Request)
			directIP := getDirectIP(c.Request)

			if ws := wsCfg(); ws != nil && ws.AuthIntranetSkip && util.IsIntranetIP(directIP) {
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
			// Upgrade 失败时 gorilla 已经写好并结束了 HTTP 响应，
			// 这里再 http.NotFound 就是对已提交响应二次写入（http: superfluous
			// / panic 级），除了脏日志什么也改变不了。
			vars.Error("路径/ws链接错误: %v", err)
			return
		}
		wsConn.SetReadLimit(readLimit)

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
	}
	for _, p := range websocketListenPaths(ws) {
		r.GET(p, handler)
		vars.Info("WebSocket 路由已注册: GET %s", p)
	}

	ln, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:    ln.Addr().String(),
		Handler: r,
	}
	// 先登记再启动：Serve 协程跑起来之后才 append 的话，与 Stop 并发的窗口里
	// 这个监听器会躲过 Shutdown，端口一直占到进程退出。
	registerServer(server)

	go func() {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("WebSocket服务器发生panic错误: %v", err)
			}
		}()
		// 用启动时的配置快照，不在协程里回读全局：停机途中配置可能已换或清空
		tlsCfg := (*config.TLSConfig)(nil)
		if ws != nil {
			tlsCfg = ws.TLS
		}
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
	return nil
}

// ============ 新增改进功能 ============

// IsIntranetIP 检查是否为内网IP（改进版本）
func IsIntranetIP(ip string) bool {
	return util.IsIntranetIP(ip)
}
