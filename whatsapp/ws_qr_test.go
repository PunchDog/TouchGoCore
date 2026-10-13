package whatsapp

// QR WebSocket 端点的端到端测试：httptest 起真 HTTP 服务、gorilla 客户端真拨号。
// 路由在本测试里直接挂到独立 gin engine 上（生产路径是 tgin.RegisterRouter 的
// 根劫持分发，handler 本体是同一个方法）；跨域判定有正反两条断言。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"go.mau.fi/whatsmeow"

	"touchgocore/config"
	"touchgocore/corectx"
)

// qrTestEnv 是一套端到端测试现场：假管理器 + 假配对会话 + httptest 服务。
type qrTestEnv struct {
	mgr     *waManager
	fake    *fakePairSession
	session *QRSession
	url     string // ws://.../whatsapp/qr/<sid>
	srv     *httptest.Server
}

// startQREnv 装配现场并把运行配置快照（web.allow_origins 从这里读）写进门面。
func startQREnv(t *testing.T, allowOrigins []string) *qrTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st := newFakeStore()
	tr := installFakePairFactory(t, st, nil)
	m := newTestManager(t, st, nil)
	sess, err := m.StartPairing(context.Background())
	if err != nil {
		t.Fatalf("发起配对失败: %v", err)
	}

	waMgr.Store(m)
	setRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Web: &config.WebConfig{AllowOrigins: allowOrigins},
	}))
	t.Cleanup(func() {
		waMgr.Store(nil)
		setRunCtx(context.Background())
	})

	engine := gin.New()
	r := &waQRRouter{}
	engine.GET(waQRWSPath, r.Stream)
	srv := httptest.NewServer(engine)
	t.Cleanup(srv.Close)

	return &qrTestEnv{
		mgr:     m,
		fake:    tr.last(),
		session: sess,
		url:     "ws" + strings.TrimPrefix(srv.URL, "http") + "/whatsapp/qr/" + sess.ID,
		srv:     srv,
	}
}

// dial 带指定 Origin 头拨号；origin 为空则不带头。
func (e *qrTestEnv) dial(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var hdr http.Header
	if origin != "" {
		hdr = http.Header{"Origin": []string{origin}}
	}
	return websocket.DefaultDialer.Dial(e.url, hdr)
}

// TestQRWSAllowedOriginReceivesCodes 白名单内的 Origin：握手成功，二维码与终局事件
// 逐帧送达，会话结束后服务端按 1000 正常关闭。
func TestQRWSAllowedOriginReceivesCodes(t *testing.T) {
	env := startQREnv(t, []string{"https://ok.example"})
	conn, _, err := env.dial(t, "https://ok.example")
	if err != nil {
		t.Fatalf("白名单 Origin 握手失败: %v", err)
	}
	defer conn.Close()
	waitUntil(t, 2*time.Second, "服务端订阅就绪", func() bool {
		env.session.mu.Lock()
		defer env.session.mu.Unlock()
		return len(env.session.subs) > 0
	})

	env.fake.emit(whatsmeow.QRChannelItem{
		Event: whatsmeow.QRChannelEventCode, Code: "QR-WS-1", Timeout: 20 * time.Second,
	}, false)
	var ev QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&ev); err != nil {
		t.Fatalf("读取二维码帧失败: %v", err)
	}
	if ev.Event != "code" || ev.Code != "QR-WS-1" || ev.SessionID != env.session.ID {
		t.Fatalf("帧内容不符: %+v", ev)
	}

	// 终局：success 之后泵协程关订阅，服务端应当写 close(1000)。
	env.fake.pairSuccess("8613800000009@s.whatsapp.net")
	var last QREvent
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.ReadJSON(&last); err != nil {
		t.Fatalf("读取成功帧失败: %v", err)
	}
	if last.Event != "success" || last.JID != "8613800000009@s.whatsapp.net" {
		t.Fatalf("成功帧不符: %+v", last)
	}
	_, _, err = conn.ReadMessage()
	var ce *websocket.CloseError
	if err == nil || !asCloseError(err, &ce) || ce.Code != websocket.CloseNormalClosure {
		t.Fatalf("终局后应当收到 1000 正常关闭，实得 err=%v", err)
	}
}

// asCloseError 是 errors.As 的窄包装（本文件只有这一处用）。
func asCloseError(err error, target **websocket.CloseError) bool {
	ce, ok := err.(*websocket.CloseError)
	if ok {
		*target = ce
	}
	return ok
}

// TestQRWSRejectsForeignOrigin 白名单外的 Origin 必须被握手层拒掉（403）：
// 跨域判定的反面断言。CheckOrigin 写 return true 会让任何网页都能连进来偷二维码——
// 二维码等于配对凭据，谁扫上谁就是账号的联合设备。
func TestQRWSRejectsForeignOrigin(t *testing.T) {
	env := startQREnv(t, []string{"https://ok.example"})
	conn, resp, err := env.dial(t, "https://evil.example")
	if err == nil {
		conn.Close()
		t.Fatal("白名单外 Origin 竟然握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("拒绝应当是 403，实得 resp=%v err=%v", resp, err)
	}
}

// TestQRWSNoOriginAllowed 不带 Origin 的握手放行：原生客户端/服务间调用不发 Origin，
// 浏览器跨站握手必带——与 websocket 包 originPolicy 的口径一致。
func TestQRWSNoOriginAllowed(t *testing.T) {
	env := startQREnv(t, nil) // 白名单为空：只放行不带 Origin 的握手
	conn, _, err := env.dial(t, "")
	if err != nil {
		t.Fatalf("不带 Origin 的握手被拒: %v", err)
	}
	conn.Close()
}

// TestQRWSWildcardOrigin 白名单显式 "*" 时放行任意 Origin（配置口径与 web.allow_origins 一致）。
func TestQRWSWildcardOrigin(t *testing.T) {
	env := startQREnv(t, []string{"*"})
	conn, _, err := env.dial(t, "https://anywhere.example")
	if err != nil {
		t.Fatalf("通配白名单应当放行: %v", err)
	}
	conn.Close()
}

// TestQRWSUnknownSession404 查无会话按 404 JSON 拒绝，且响应体干净
// （不被框架 sendResponse 的 "success" 追加污染——hijackSafeWriter 的行为断言）。
func TestQRWSUnknownSession404(t *testing.T) {
	env := startQREnv(t, nil)
	resp, err := http.Get(env.srv.URL + "/whatsapp/qr/qr-does-not-exist")
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

// TestQRWSManagerDown503 消息侧未启动时端点报 503 而不是 panic。
func TestQRWSManagerDown503(t *testing.T) {
	env := startQREnv(t, nil)
	waMgr.Store(nil) // 模拟未启动；t.Cleanup 会再置一次 nil，无副作用
	resp, err := http.Get(env.srv.URL + "/whatsapp/qr/" + env.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未启动应当 503，实得 %d", resp.StatusCode)
	}
}
