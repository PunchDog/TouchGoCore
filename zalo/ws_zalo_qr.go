package zalo

// 本文件是 Zalo 扫码配对的 WebSocket 推送端点：GET /zalo/qr/:session_id。
//
// 挂载范式与 Origin 口径照抄 whatsapp/ws_qr.go（已核实）：
//   - 本仓 gin 不暴露 *gin.Engine，路由经 tgin.RegisterRouter 的「结构体方法 +
//     RouterType/RouterPath」惯例注册（gin/run.go:121、gin/dispatch.go:262）；
//   - RegisterRouter 在 handler 返回后无条件补写一次响应（gin/run.go:226-255），
//     WebSocket 升级会 hijack 连接，补写会触发 net/http 的
//     "response.Write on hijacked connection"；JSON 错误响应则会被追加成
//     "{...}success"。所以本文件的 handler 写完自己的响应后把 c.Writer 换成
//     hijackSafeWriter，把框架的补写吞掉；
//   - 对端断开用读泵检测，不能等 c.Request.Context()：RegisterRouter 给每个请求
//     套了 15s 超时 ctx，拿它当选退信号会让每条 QR 连接在第 15 秒被掐断。
//
// 安全口径：端点无鉴权（与 WhatsApp 一致），二维码等同登录凭据，靠不可猜的
// 16 字节 hex sessionID + Origin 白名单（web.allow_origins）+ 部署层限制。
// 本文件不记录任何日志，更不会输出 code/image。

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"touchgocore/corectx"
	tgin "touchgocore/gin"
)

// zaQRWSPath 是 QR 推送端点的注册路径（含 gin 风格参数段，分发器支持单段 ":name"）。
const zaQRWSPath = "/zalo/qr/:session_id"

const (
	// zaQRWSReadLimit 限制客户端上行帧大小：这个端点只读推送，收到大帧只可能是
	// 恶意/异常客户端，限制后避免为一条连接分配无上限内存。
	zaQRWSReadLimit = 4 << 10
	// zaQRWSWriteTimeout 是单帧写超时：慢客户端不能无限占住推送协程。
	zaQRWSWriteTimeout = 10 * time.Second
)

// zaQRRouter 是挂到 tgin.RegisterRouter 的路由结构体。
// 只有 Stream 一个 handler 方法；RouterType/RouterPath 是类型方法，注册器会跳过。
type zaQRRouter struct{}

// RouterType 只放行 GET：WebSocket 握手就是一个 GET 升级请求。
func (r *zaQRRouter) RouterType() []string { return []string{http.MethodGet} }

// RouterPath 用显式路径覆盖默认的 /zaqrrouter/stream 推导。
func (r *zaQRRouter) RouterPath() map[string]string {
	return map[string]string{"Stream": zaQRWSPath}
}

var (
	// zaQRRouteOnce 保证只注册一次：ZaloStart 可被多次调用（测试与重启形态），
	// RegisterRouter 对同路径重复注册会记 ERROR 并覆盖 handler，且没有注销 API。
	zaQRRouteOnce sync.Once
	// zaQRUpgrader 是握手用的升级器。缓冲 4KiB 与 websocket 包默认一致：
	// QR 帧很小（单张 PNG base64 几十 KB 的事件也走默认写缓冲即可），大了是纯浪费。
	zaQRUpgrader = &websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 4 * 1024,
		// 跨域判定复用 web.allow_origins（WebConfig 注释口径：空则不允许跨域）。
		// gorilla 对同源握手自己放行，非同源才走到这里；绝不写 return true。
		CheckOrigin: func(req *http.Request) bool {
			return zaQROriginAllowed(req, zaQRAllowedOrigins())
		},
	}
)

// registerQRRoutes 注册 QR 端点（幂等）。由 ZaloStart 调用。
func registerQRRoutes() {
	zaQRRouteOnce.Do(func() {
		tgin.RegisterRouter(&zaQRRouter{})
	})
}

// zaQRAllowedOrigins 取 web.allow_origins 快照；拿不到配置返回 nil（按「拒绝跨域」处理）。
// runCtx 在 zalo.go 里。
func zaQRAllowedOrigins() []string {
	cfg := corectx.CfgFrom(runCtx())
	if cfg == nil || cfg.Web == nil {
		return nil
	}
	return cfg.Web.AllowOrigins
}

// zaQROriginAllowed 是跨域判定：不带 Origin 放行（原生客户端/服务间调用不发 Origin，
// 浏览器跨站握手必带），带了则按白名单精确比对 host（大小写不敏感，白名单项容忍
// 无 scheme 形态）；白名单显式 "*" 放行任意 Origin。
func zaQROriginAllowed(req *http.Request, allow []string) bool {
	origin := strings.TrimSpace(req.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a == "*" {
			return true
		}
		if a == "" {
			continue
		}
		if zaQROriginHostOf(a) == zaQROriginHostOf(origin) {
			return true
		}
	}
	return false
}

// zaQROriginHostOf 取小写 host（含端口）；解析不出 host 时退回把原串当无 scheme 的 host 用。
func zaQROriginHostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(raw)
}

// Stream 是端点 handler：按 session_id 找到进行中的扫码会话，升级 WebSocket 后把
// QREvent 逐帧推给对端，直到会话终局（订阅通道关闭 → 写 close 1000）或对端断开。
func (r *zaQRRouter) Stream(c *gin.Context) {
	sid := c.Param("session_id")
	mgr, err := currentManager()
	if err != nil {
		respondRaw(c, http.StatusServiceUnavailable, gin.H{"error": "zalo 通道未启动"})
		return
	}
	sess, ok := mgr.LookupQRSession(sid)
	if !ok {
		// 查无会话与「会话已结束」不可区分也不该区分：都按 404，
		// 不给探测者反馈哪些 id 曾经存在过。
		respondRaw(c, http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	// 先订阅再升级：升级成功到订阅生效之间若来事件会丢帧；
	// 反过来先订阅、升级失败再退订，最多空持一个订阅。
	sub, unsub := sess.Subscribe()
	defer unsub()

	conn, err := zaQRUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// 升级失败时 gorilla 已写好 HTTP 响应（含 CheckOrigin 拒绝的 403），
		// 这里不再补写，只把框架的 sendResponse 吞掉。
		c.Writer = hijackSafeWriter{ResponseWriter: c.Writer}
		return
	}
	defer conn.Close()
	c.Writer = hijackSafeWriter{ResponseWriter: c.Writer}
	// 本端点只推送；限制上行帧并给读侧一个上限，避免恶意客户端撑爆内存。
	conn.SetReadLimit(zaQRWSReadLimit)

	dead := make(chan struct{})
	go func() {
		defer close(dead)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				// 会话终局：正常关闭，让前端按 close code 1000 收场而不是读错误。
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(zaQRWSWriteTimeout))
			if err := conn.WriteJSON(ev); err != nil {
				return // 写失败即对端已断：退订收尾，扫码会话本身不受影响
			}
		case <-dead:
			return
		}
	}
}

// respondRaw 写一个 JSON 响应并把 c.Writer 换成丢弃写件。
// 换写件的原因见文件头注释：RegisterRouter 的 sendResponse 会在方法返回后
// 追加一次 "success"，不换就会污染已写好的响应体。
func respondRaw(c *gin.Context, status int, payload any) {
	c.JSON(status, payload)
	c.Writer = hijackSafeWriter{ResponseWriter: c.Writer}
}

// hijackSafeWriter 吞掉安装之后的一切写入。
//
// 只覆盖三个写入口（Write/WriteString/WriteHeader）与 WriteHeaderNow；
// Header/Hijack/Flush/Status/Size/Written 等仍委托原写件——升级路径要在安装它
// 之前用原写件完成 Hijack，安装之后 Status()/Written() 的读数也仍然真实。
type hijackSafeWriter struct{ gin.ResponseWriter }

func (w hijackSafeWriter) Write(b []byte) (int, error)       { return len(b), nil }
func (w hijackSafeWriter) WriteString(s string) (int, error) { return len(s), nil }
func (w hijackSafeWriter) WriteHeader(code int)              {}
func (w hijackSafeWriter) WriteHeaderNow()                   {}
