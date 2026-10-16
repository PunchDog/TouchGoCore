package whatsapp

// 本文件是 QR 配对的 WebSocket 推送端点。
//
// 挂载范式（查证结论）：本仓的 touchgocore/gin 不暴露 *gin.Engine——Run 只挂一个
// NoRoute 根劫持，路由经 RegisterRouter 的「结构体方法 + RouterType/RouterPath」惯例
// 注册（gin/run.go:121、gin/dispatch.go:262）。因此这里按该惯例注册一个路由结构体，
// 而不是计划稿里的 engine.GET(...)。端点最终路径是 GET /whatsapp/qr/:session_id，
// 挂在 web.httpport 那个 Gin HTTP 端口上（需要 web 段配置存在，gin.Run 已启动）。
//
// 两个查证出的坑，都影响路径与写法：
//   - 路径**不能**用计划稿的 /ws/whatsapp/qr/...：根劫持对 /ws 前缀静默 404、
//     根本不查路由表（gin/dispatch.go:274-278，「ws 实际跑在独立端口」指的是
//     websocket 包的游戏消息端口）。所以前缀改成 /whatsapp/qr。
//   - RegisterRouter 包的 handler 在方法返回后无条件补写一次响应
//     （gin/run.go:226-255 的 sendResponse，无返回值即写 200 "success"）。
//     WebSocket 升级会 hijack 连接，补写会让 net/http 往 stderr 打
//     "response.Write on hijacked connection"；JSON 错误响应则会被追加成
//     "{...}success"。所以本文件的 handler 在写完自己的响应后把 c.Writer 换成
//     丢弃写件（hijackSafeWriter），把框架的补写吞掉。

import (
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	tgin "touchgocore/gin"
)

// waQRWSPath 是 QR 推送端点的注册路径（含 gin 风格参数段，分发器支持单段 ":name"，
// 查自 gin/routepath.go 的 IRouterPath 注释）。
const waQRWSPath = "/whatsapp/qr/:session_id"

// waQRRouter 是挂到 tgin.RegisterRouter 的路由结构体。
// 只有 Stream 一个 handler 方法；RouterType/RouterPath 是类型方法，注册器会跳过它们。
type waQRRouter struct{}

// RouterType 只放行 GET：WebSocket 握手就是一个 GET 升级请求。
func (r *waQRRouter) RouterType() []string { return []string{http.MethodGet} }

// RouterPath 用显式路径覆盖默认的 /waqrrouter/stream 推导。
func (r *waQRRouter) RouterPath() map[string]string {
	return map[string]string{"Stream": waQRWSPath}
}

var (
	// qrRouteOnce 保证只注册一次：WhatsappStart 可被多次调用（测试与重启形态），
	// RegisterRouter 对同路径重复注册会记 ERROR 并覆盖 handler（gin/run.go:213-216），
	// 而注册表没有注销 API，重复注册只留噪音没有收益。
	qrRouteOnce sync.Once
	// qrUpgrader 是握手用的升级器。缓冲 4KiB 与 websocket 包默认一致
	//（websocket/server.go:25）：QR 帧很小，大了是纯内存浪费。
	qrUpgrader = &websocket.Upgrader{
		ReadBufferSize:  4 * 1024,
		WriteBufferSize: 4 * 1024,
		// 跨域判定收紧：复用 web.allow_origins 白名单（WebConfig 注释的口径是
		// 「空则不允许跨域（生产默认）」）。gorilla 对同源握手自己放行，
		// 非同源才走到这个函数；绝不写 return true。
		CheckOrigin: func(req *http.Request) bool {
			return qrOriginAllowed(req, qrAllowedOrigins())
		},
	}
)

// registerQRRoutes 注册 QR 端点（幂等）。WhatsappStart 调用。
func registerQRRoutes() {
	qrRouteOnce.Do(func() {
		tgin.RegisterRouter(&waQRRouter{})
	})
}

// qrAllowedOrigins 取 web.allow_origins 快照；拿不到配置返回 nil（按「拒绝跨域」处理）。
func qrAllowedOrigins() []string {
	cfg := allCfg()
	if cfg == nil || cfg.Web == nil {
		return nil
	}
	return cfg.Web.AllowOrigins
}

// qrOriginAllowed 是跨域判定：不带 Origin 放行（原生客户端/服务间调用不发 Origin，
// 浏览器跨站握手必带——与 websocket 包 originPolicy 的口径一致），
// 带了则按白名单精确比对 host（大小写不敏感，白名单项容忍无 scheme 形态）。
func qrOriginAllowed(req *http.Request, allow []string) bool {
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
		if originHostOf(a) == originHostOf(origin) {
			return true
		}
	}
	return false
}

// originHostOf 取小写 host（含端口）；解析不出 host 时退回把原串当无 scheme 的 host 用。
func originHostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(raw)
}

// Stream 是端点 handler：按 session_id 找到进行中的配对会话，升级 WebSocket 后
// 把 QR 事件逐帧推给对端，直到会话终局（库关闭订阅通道）或对端断开。
//
// 对端断开的检测走读泵而不是请求 ctx：RegisterRouter 给每个请求套了 15s 超时 ctx，
// 且参数路由（含 ":session_id"）不允许按路径覆盖超时（gin/routepath.go 注释、
// gin/run.go:226-231）——拿 c.Request.Context() 当选退信号会让每条 QR 连接
// 在第 15 秒被框架掐断。gorilla 的契约是一读一写各一个 goroutine，
// 读泵独占读侧，读到错误（对端关闭/半开断裂）即散场。
func (r *waQRRouter) Stream(c *gin.Context) {
	sid := c.Param("session_id")
	mgr, err := currentWA()
	if err != nil {
		respondRaw(c, http.StatusServiceUnavailable, gin.H{"error": "whatsapp 消息侧未启动"})
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

	conn, err := qrUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// 升级失败时 gorilla 已写好 HTTP 响应（含 CheckOrigin 拒绝的 403），
		// 这里不再补写，只把框架的 sendResponse 吞掉。
		c.Writer = hijackSafeWriter{ResponseWriter: c.Writer}
		return
	}
	defer conn.Close()
	c.Writer = hijackSafeWriter{ResponseWriter: c.Writer}

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
			if err := conn.WriteJSON(ev); err != nil {
				return // 写失败即对端已断：退订收尾，配对会话本身不受影响
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
// 之前用原写件完成 Hijack，安装之后 Status()/Written() 的读数也仍然是真实的。
type hijackSafeWriter struct{ gin.ResponseWriter }

func (w hijackSafeWriter) Write(b []byte) (int, error)       { return len(b), nil }
func (w hijackSafeWriter) WriteString(s string) (int, error) { return len(s), nil }
func (w hijackSafeWriter) WriteHeader(code int)              {}
func (w hijackSafeWriter) WriteHeaderNow()                   {}
