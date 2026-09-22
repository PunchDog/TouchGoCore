package gin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// IRouterTimeout 可选接口（超时表由 RegisterRouter 第二参数收敛而来）的回归用例：
//  1. 命中路径的超时覆盖默认 15s；
//  2. 未实现 IRouterTimeout、或路径未列出时回退默认 15s；
//  3. 与 IRouterPath 联动：超时按显式路径索引也能命中；
//  4. RouterTimeout 方法本身不会被注册成路由 handler。
// 复用 run_route_test.go 的 isolateRegistry / routerKeys。
// ============================================================================

type timeoutRecv struct{}

func (*timeoutRecv) RouterType() []string { return []string{"POST"} }
func (*timeoutRecv) RouterTimeout() map[string]int64 {
	return map[string]int64{"/timeoutrecv/slow": 6}
}
func (*timeoutRecv) Slow(_ *gin.Context) string  { return "slow" }
func (*timeoutRecv) Quick(_ *gin.Context) string { return "quick" }

// explicitTimeoutRecv 同时实现 IRouterPath 与 IRouterTimeout，超时键用的是显式路径。
type explicitTimeoutRecv struct{}

func (*explicitTimeoutRecv) RouterType() []string { return []string{"POST"} }
func (*explicitTimeoutRecv) RouterPath() map[string]string {
	return map[string]string{"Login": "/wst/api/m/login"}
}
func (*explicitTimeoutRecv) RouterTimeout() map[string]int64 {
	return map[string]int64{"/wst/api/m/login": 45}
}
func (*explicitTimeoutRecv) Login(_ *gin.Context) string { return "login" }

// invokeForDeadline 把 handler 挂到真实 gin 引擎上跑一次请求，返回它给请求 ctx 设的超时时长。
// 必须走引擎：超时要按 ctx.FullPath() 查表，CreateTestContext 的 FullPath 是空串。
// handler 里的 defer cancel() 在返回时已执行，但 WithTimeout 的 deadline 取消后仍可读。
func invokeForDeadline(t *testing.T, path string) time.Duration {
	t.Helper()
	routerMu.Lock()
	var fn func(ctx *gin.Context)
	// 注册表 key 是「路径|方法列表」，这里只按路径取 handler
	for k, v := range routerMap {
		if p, _, _ := strings.Cut(k, "|"); p == path {
			fn = v
			break
		}
	}
	routerMu.Unlock()
	if fn == nil {
		t.Fatalf("✘ 路由 %q 未注册（现有: %v）", path, routerKeys())
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	var deadline time.Time
	// 第二个 handler 在同一 Context 上读到前者替换过的 Request
	engine.POST(path, fn, func(c *gin.Context) {
		deadline, _ = c.Request.Context().Deadline()
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))

	if deadline.IsZero() {
		t.Fatalf("✘ 路由 %q 的请求 ctx 没有超时", path)
	}
	return time.Until(deadline)
}

// within 判断实测剩余时长是否落在期望秒数附近（gin 测试上下文与反射调用有几毫秒开销）。
func within(d time.Duration, seconds int) bool {
	want := time.Duration(seconds) * time.Second
	return d > want-2*time.Second && d < want+2*time.Second
}

func TestRouterTimeoutOverridesDefault(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&timeoutRecv{})

	if got := invokeForDeadline(t, "/timeoutrecv/slow"); !within(got, 6) {
		t.Fatalf("✘ 命中 RouterTimeout 的路由超时 %v，应约 6s", got)
	}
	// 同一类型内未列出的路径仍是默认 15s
	if got := invokeForDeadline(t, "/timeoutrecv/quick"); !within(got, 15) {
		t.Fatalf("✘ 未列出路径的超时 %v，应回退默认 15s", got)
	}
	for _, k := range routerKeys() {
		if strings.Contains(strings.ToLower(k), "routertimeout") {
			t.Fatalf("✘ RouterTimeout 被错误注册为路由: %v", routerKeys())
		}
	}
}

func TestRouterTimeoutDefaultWithoutInterface(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&plainRecv{})

	if got := invokeForDeadline(t, "/plainrecv/hello"); !within(got, 15) {
		t.Fatalf("✘ 未实现 IRouterTimeout 的类型超时 %v，应回退默认 15s", got)
	}
}

func TestRouterTimeoutWithExplicitPath(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&explicitTimeoutRecv{})

	keys := routerKeys()
	if len(keys) != 1 || keys[0] != "/wst/api/m/login|POST" {
		t.Fatalf("✘ 显式路径未生效: %v", keys)
	}
	if got := invokeForDeadline(t, "/wst/api/m/login"); !within(got, 45) {
		t.Fatalf("✘ 按显式路径索引的超时未命中，实际 %v，应约 45s", got)
	}
}
