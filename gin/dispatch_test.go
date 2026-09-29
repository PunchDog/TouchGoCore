package gin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 根劫持 + 手动分发（NoRoute）的回归用例：
//  1. splitRouteKey / buildDispatchIndex 的键解析与索引构建；
//  2. newRootHandler：命中路径+方法 -> 调用；方法不在白名单 -> 404；未注册路径 -> 404；
//     /ws 前缀静默 404；/static 前缀走文件服务；
//  3. Run 集成：显式/默认路由经根 handler 真实分发命中。
// 复用 run_route_test.go 的 isolateRegistry / freeTCPPort。
// ============================================================================

func newTestCtx(method, path string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, nil)
	return c, w
}

func echoHandler(body string) func(*gin.Context) {
	return func(c *gin.Context) { c.String(http.StatusOK, body) }
}

// TestSplitRouteKey 键解析：纯净路径 + 方法白名单集合。
func TestSplitRouteKey(t *testing.T) {
	cases := []struct {
		key        string
		wantPath   string
		wantNil    bool
		wantMethos []string
	}{
		{"/b", "/b", true, nil},
		{"/a|GET&&POST", "/a", false, []string{"GET", "POST"}},
		{"/c|PUT", "/c", false, []string{"PUT"}},
	}
	for _, c := range cases {
		path, set := splitRouteKey(c.key)
		if path != c.wantPath {
			t.Fatalf("✘ %q 路径解析为 %q，期望 %q", c.key, path, c.wantPath)
		}
		if c.wantNil {
			if set != nil {
				t.Fatalf("✘ %q 期望 nil（放行所有方法），实际 %v", c.key, set)
			}
			continue
		}
		if len(set) != len(c.wantMethos) {
			t.Fatalf("✘ %q 方法集合大小 %d，期望 %d", c.key, len(set), len(c.wantMethos))
		}
		for _, m := range c.wantMethos {
			if !set[m] {
				t.Fatalf("✘ %q 缺少方法 %s: %v", c.key, m, set)
			}
		}
	}
}

// TestBuildDispatchIndex routerMap 快照归一为只读索引。
func TestBuildDispatchIndex(t *testing.T) {
	routes := map[string]func(*gin.Context){
		"/a|GET&&POST": echoHandler("A"),
		"/b":           echoHandler("B"),
	}
	idx := buildDispatchIndex(routes)
	if len(idx) != 2 {
		t.Fatalf("✘ 索引大小 %d，期望 2", len(idx))
	}
	if idx["/a"] == nil || idx["/a"].methods == nil || !idx["/a"].methods["GET"] || !idx["/a"].methods["POST"] {
		t.Fatalf("✘ /a 条目错误: %+v", idx["/a"])
	}
	if idx["/b"] == nil || idx["/b"].methods != nil {
		t.Fatalf("✘ /b 应为放行所有方法: %+v", idx["/b"])
	}
}

// TestNewRootHandlerDispatch 命中/方法不匹配/未注册三类分支。
func TestNewRootHandlerDispatch(t *testing.T) {
	idx := map[string]*routeEntry{
		"/a": {fn: echoHandler("A"), methods: nil},
		"/b": {fn: echoHandler("B"), methods: map[string]bool{"POST": true}},
	}
	h := newRootHandler(idx, nil)

	// 命中：无方法限制
	c, w := newTestCtx(http.MethodGet, "/a")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "A" {
		t.Fatalf("✘ /a GET 未命中: %d %q", w.Code, w.Body.String())
	}
	// 命中：方法在白名单
	c, w = newTestCtx(http.MethodPost, "/b")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "B" {
		t.Fatalf("✘ /b POST 未命中: %d %q", w.Code, w.Body.String())
	}
	// 方法不在白名单 -> 404
	c, w = newTestCtx(http.MethodGet, "/b")
	h(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("✘ /b GET 方法不匹配应 404，实际 %d", w.Code)
	}
	// 未注册路径（含任意深度）-> 404
	for _, p := range []string{"/unknown", "/api1/api2", "/api1/api2/api3"} {
		c, w = newTestCtx(http.MethodGet, p)
		h(c)
		if w.Code != http.StatusNotFound {
			t.Fatalf("✘ %s 应 404，实际 %d", p, w.Code)
		}
	}
}

// TestNewRootHandlerWSSilent /ws 前缀静默 404：不得走带 warning 的业务 JSON 分支。
// （c.Status 不写 body，gin 会渲染默认 "404 page not found"；与未命中的 JSON 不同）
func TestNewRootHandlerWSSilent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(map[string]*routeEntry{}, nil))
	for _, p := range []string{"/ws", "/ws/sub"} {
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("✘ %s 应 404，实际 %d", p, w.Code)
		}
		// 未命中分支会写 JSON（含 error/code）；静默分支不应如此。
		if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "application/json") {
			t.Fatalf("✘ %s 不应走带 warning 的业务 JSON 分支，实际 Content-Type=%q body=%q", p, ct, w.Body.String())
		}
	}
}

// TestNewRootHandlerStatic /static 前缀走文件服务。
func TestNewRootHandlerStatic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newRootHandler(map[string]*routeEntry{}, &dir)

	c, w := newTestCtx(http.MethodGet, "/static/f.txt")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "hello-file" {
		t.Fatalf("✘ 静态文件返回错误: %d %q", w.Code, w.Body.String())
	}
}

type dpRecv struct{}

func (*dpRecv) RouterType() []string       { return []string{"GET"} }
func (*dpRecv) Ping(_ *gin.Context) string { return "pong" }

// TestDispatchRunIntegration 经 Run 起真实服务器，验证根劫持分发命中已注册路由，
// 方法不匹配/未注册路径/ws 均 404。
func TestDispatchRunIntegration(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&dpRecv{})

	port := freeTCPPort(t)
	cfg := &config.Cfg{Web: &config.WebConfig{HTTPPort: port}}
	if err := Run(corectx.WithCfg(context.Background(), cfg)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = Stop(context.Background()) }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	client := &http.Client{Timeout: 3 * time.Second}
	do := func(method, path string) (int, string) {
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("✘ %s %s 请求失败: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if status, body := do("GET", "/dprecv/ping"); status != http.StatusOK || body != "pong" {
		t.Fatalf("✘ 命中分发失败: %d %q", status, body)
	}
	if status, _ := do("POST", "/dprecv/ping"); status != http.StatusNotFound {
		t.Fatalf("✘ 方法不匹配应 404，实际 %d", status)
	}
	if status, _ := do("GET", "/deep/not/registered"); status != http.StatusNotFound {
		t.Fatalf("✘ 未注册路径应 404，实际 %d", status)
	}
	if status, _ := do("GET", "/ws"); status != http.StatusNotFound {
		t.Fatalf("✘ /ws 应静默 404，实际 %d", status)
	}
}
