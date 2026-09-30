package websocket

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// ============================================================================
// qa_w 修复回归（槽位 W）：
//   W6 server.go handler 的 panic 恢复路径不关已 hijack 的连接 → fd 泄漏
//   W7 gin.Default 的 Logger 会把 query 里的 token 原样打进日志 → 脱敏
// ============================================================================

// TestQaW_HandlerPanicClosesHijackedConn Upgrade 成功后 handler panic：
// 恢复路径必须关闭已被 hijack 的连接。修复前连接永不关闭，
// 客户端 ReadMessage 一直挂到读超时（服务端 fd 泄漏）。
func TestQaW_HandlerPanicClosesHijackedConn(t *testing.T) {
	prevAuth := GetAuthFunc()
	SetAuthFunc(nil)
	t.Cleanup(func() { SetAuthFunc(prevAuth) })

	gin.SetMode(gin.TestMode)
	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	r := gin.New()
	// 注入非字符串 className：handler 在 Upgrade 之后做 .(string) 断言时 panic
	r.Use(func(c *gin.Context) { c.Set("className", 12345) })
	r.GET("/ws", wsHandler("ignored", up, 1<<20))
	srv := httptest.NewServer(r)
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("✘ 握手失败: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("✘ panic 后不应还能读到数据")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("✘ handler panic 后未关闭已劫持连接（fd 泄漏），客户端读超时: %v", err)
	}
}

// TestQaW_SanitizeRawQuery 敏感 query 参数必须脱敏，普通参数保留。
func TestQaW_SanitizeRawQuery(t *testing.T) {
	got := sanitizeRawQuery("token=SECRET-abc&uid=42&auth_key=k1&sign=xyz&keep=1")
	for _, leaked := range []string{"SECRET-abc", "k1", "xyz"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("✘ 敏感参数泄漏进日志输出: %q", got)
		}
	}
	if !strings.Contains(got, "uid=42") || !strings.Contains(got, "keep=1") {
		t.Fatalf("✘ 普通参数被误伤: %q", got)
	}
	if s := sanitizeRawQuery(""); s != "" {
		t.Fatalf("✘ 空 query 应原样返回: %q", s)
	}
	if s := sanitizeRawQuery("%zz"); s == "" || strings.Contains(s, "%zz") {
		t.Fatalf("✘ 不可解析 query 应整体遮蔽: %q", s)
	}
}
