package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"

	"github.com/gin-gonic/gin"
)

func TestWebsocketPath(t *testing.T) {
	cases := []struct {
		raw, fallback, want string
	}{
		{"", "/ws", "/ws"},
		{"ws", "/ws", "/ws"},
		{"/game", "/ws", "/game"},
		{"wss://example.com/inner", "/ws", "/inner"},
		{"http://127.0.0.1:8000/ws", "/ws", "/ws"},
	}
	for _, c := range cases {
		if got := websocketPath(c.raw, c.fallback); got != c.want {
			t.Fatalf("websocketPath(%q)=%q want %q", c.raw, got, c.want)
		}
	}
}

func TestWebsocketListenPathsDedup(t *testing.T) {
	paths := websocketListenPaths(&config.WebsocketConfig{URL: "/ws", InURL: "ws"})
	if len(paths) != 1 || paths[0] != "/ws" {
		t.Fatalf("paths=%v", paths)
	}
	paths = websocketListenPaths(&config.WebsocketConfig{URL: "/ws", InURL: "/inws"})
	if len(paths) != 2 {
		t.Fatalf("expected two paths, got %v", paths)
	}
}

func TestIsAllowedOrigin(t *testing.T) {
	allowedOrigins = []string{"https://example.com", "*.game.local"}
	if !isAllowedOrigin("https://example.com", "example.com") {
		t.Fatal("exact origin")
	}
	if !isAllowedOrigin("https://a.game.local", "a.game.local") {
		t.Fatal("wildcard")
	}
	if isAllowedOrigin("https://evil.com", "evil.com") {
		t.Fatal("should reject")
	}
}

func TestGetClientIPIgnoresUntrustedXFF(t *testing.T) {
	prev := wsRunCtx
	wsRunCtx = corectx.WithCfg(context.Background(), &config.Cfg{
		Ws: &config.WebsocketConfig{TrustedProxies: []string{"10.0.0.1"}},
	})
	t.Cleanup(func() { wsRunCtx = prev })

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if ip := getClientIP(req); ip != "8.8.8.8" {
		t.Fatalf("untrusted XFF should be ignored, got %s", ip)
	}

	req.RemoteAddr = "10.0.0.1:80"
	if ip := getClientIP(req); ip != "1.2.3.4" {
		t.Fatalf("trusted proxy XFF, got %s", ip)
	}
}

func TestExtractAuthToken(t *testing.T) {
	prev := wsRunCtx
	wsRunCtx = corectx.WithCfg(context.Background(), &config.Cfg{
		Ws: &config.WebsocketConfig{AuthTokenHeader: "X-Auth-Token", AuthTokenQuery: "token"},
	})
	t.Cleanup(func() { wsRunCtx = prev })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/ws?token=fromquery", nil)
	if got := extractAuthToken(c); got != "fromquery" {
		t.Fatalf("query token=%q", got)
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/ws?token=fromquery", nil)
	c.Request.Header.Set("X-Auth-Token", "fromheader")
	if got := extractAuthToken(c); got != "fromheader" {
		t.Fatalf("header should win, got %q", got)
	}
}
