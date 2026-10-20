package zalo

// 本文件是 QR WebSocket 端点的端到端测试：httptest 起真 HTTP 服务、gorilla
// 客户端真拨号。路由在测试里直接挂到独立 gin engine（生产路径是
// tgin.RegisterRouter 的根劫持分发，handler 本体是同一个方法）。
//
// 扫描会话由 ZaloStartPairing 创建（fake LoginQR 阻塞），事件由测试直接
// sess.emit/emitTerminal 推入；跨域判定有正反两条断言。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"touchgocore/config"
	"touchgocore/corectx"
)

// wsZaloQREnv 是一套端到端测试现场：假 manager + 阻塞的假 QR SDK + httptest 服务。
type wsZaloQREnv struct {
	mgr     *zaManager
	sdk     *pairFakeSDK
	session *QRSession
	url     string // ws://.../zalo/qr/<sid>
	srv     *httptest.Server
}

// wsStartZaloQREnv 装配现场，并把 web.allow_origins 快照写进运行配置（CheckOrigin 从这里读）。
func wsStartZaloQREnv(t *testing.T, allowOrigins []string) *wsZaloQREnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	sdk := &pairFakeSDK{block: true}
	m := pairNewManager(t, newFakeZAStore(), sdk, 0)
	pairInstallManager(t, m)
	sess := pairStart(t, m, "acct")

	setRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Web: &config.WebConfig{AllowOrigins: allowOrigins},
	}))
	t.Cleanup(func() { setRunCtx(context.Background()) })

	engine := gin.New()
	r := &zaQRRouter{}
	engine.GET(zaQRWSPath, r.Stream)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &wsZaloQREnv{
		mgr:     m,
		sdk:     sdk,
		session: sess,
		url:     "ws" + strings.TrimPrefix(srv.URL, "http") + "/zalo/qr/" + sess.ID,
		srv:     srv,
	}
}

// dial 带指定 Origin 头拨号；origin 为空则不带头。
func (e *wsZaloQREnv) dial(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	return websocket.DefaultDialer.Dial(e.url, hdr)
}

// wsWaitSubs 等到 WS handler 完成订阅（handler 先订阅再升级）。
func wsWaitSubs(t *testing.T, s *QRSession) {
	t.Helper()
	mgrWaitTrue(t, "WS 订阅登记", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.subs) > 0
	})
}

// TestZAZaloQRWSAllowedOriginReceivesFrames 白名单内的 Origin：握手成功，二维码与
// 终局事件逐帧送达，会话结束后服务端按 1000 正常关闭。
func TestZAZaloQRWSAllowedOriginReceivesFrames(t *testing.T) {
	env := wsStartZaloQREnv(t, []string{"https://ok.example"})
	conn, _, err := env.dial(t, "https://ok.example")
	if err != nil {
		t.Fatalf("白名单 Origin 握手失败: %v", err)
	}
	defer conn.Close()
	wsWaitSubs(t, env.session)

	env.session.emit(QREvent{Event: qrEventQR, QRCode: "QR-WS-1", ImageBase64: "aGVsbG8="})
	var ev QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("读取二维码帧失败: %v", err)
	}
	if ev.Event != qrEventQR || ev.QRCode != "QR-WS-1" || ev.SessionID != env.session.ID {
		t.Fatalf("帧内容不符: %+v", ev)
	}

	// 终局：success 之后会话关闭订阅，服务端应当写 close(1000)。
	env.session.emitTerminal(QREvent{Event: qrEventSuccess, UID: "uid-ws"})
	var last QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&last); err != nil {
		t.Fatalf("读取成功帧失败: %v", err)
	}
	if last.Event != qrEventSuccess || last.UID != "uid-ws" || last.SessionID != env.session.ID {
		t.Fatalf("成功帧不符: %+v", last)
	}
	_, _, err = conn.ReadMessage()
	var ce *websocket.CloseError
	if err == nil || !errors.As(err, &ce) || ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("终局后应当收到 1000 正常关闭，实得 err=%v", err)
	}
}

// TestZAZaloQRWSOriginPolicy 跨域白名单三档裁决：
//   - 白名单外拒 403：二维码等于登录凭据，写 return true 会让任何网页连进来偷码；
//   - 不带 Origin 放行：原生客户端/服务间调用不发 Origin，浏览器跨站握手必带；
//   - 白名单显式 "*" 放行任意 Origin。
func TestZAZaloQRWSOriginPolicy(t *testing.T) {
	cases := []struct {
		name   string
		allow  []string
		origin string
		wantOK bool
	}{
		{"白名单外拒绝", []string{"https://ok.example"}, "https://evil.example", false},
		{"不带 Origin 放行", nil, "", true},
		{"通配放行任意 Origin", []string{"*"}, "https://anywhere.example", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := wsStartZaloQREnv(t, c.allow)
			conn, resp, err := env.dial(t, c.origin)
			if c.wantOK {
				if err != nil {
					t.Fatalf("Origin %q 应当放行: %v", c.origin, err)
				}
				conn.Close()
				return
			}
			if err == nil {
				conn.Close()
				t.Fatal("白名单外 Origin 竟然握手成功")
			}
			if resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("拒绝应当是 403，实得 resp=%v err=%v", resp, err)
			}
		})
	}
}

// TestZAZaloQRWSUnknownSession404 查无会话按 404 JSON 拒绝，且响应体干净
// （不被框架 sendResponse 的 "success" 追加污染——hijackSafeWriter 的行为断言）。
func TestZAZaloQRWSUnknownSession404(t *testing.T) {
	env := wsStartZaloQREnv(t, nil)
	resp, err := http.Get(env.srv.URL + "/zalo/qr/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("查无会话应当 404，实得 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("404 响应不是合法 JSON（可能被 sendResponse 追加污染）: %v", err)
	}
	if body["error"] != "session not found" {
		t.Fatalf("404 响应体不符: %v", body)
	}
}

// TestZAZaloQRWSManagerDown503 通道未启动时端点报 503 而不是 panic。
func TestZAZaloQRWSManagerDown503(t *testing.T) {
	env := wsStartZaloQREnv(t, nil)
	zaMgr.Store(nil) // 模拟未启动；t.Cleanup 会恢复旧值，无副作用
	resp, err := http.Get(env.srv.URL + "/zalo/qr/" + env.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未启动应当 503，实得 %d", resp.StatusCode)
	}
}
