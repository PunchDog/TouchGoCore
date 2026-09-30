package gin

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// ":name" 参数段匹配的回归用例：
//   - 精确未命中时逐段匹配参数路由，":id" 段捕获非空单段并回填 ctx.Param；
//   - 方法门控与字面路由同口径（精确方法优先，再通配）；
//   - 尾斜杠（空参数段）/ 段数不符落 404；方法不匹配落 405+Allow（A-F8）；
//   - 字面路由优先于参数路由（同段数时精确命中不被参数模式抢占）。
// 复用 run_route_test.go 的 isolateRegistry / freeTCPPort。
// ============================================================================

type paramRecv struct{}

func (*paramRecv) RouterType() []string { return []string{"GET"} }
func (*paramRecv) RouterPath() map[string]string {
	return map[string]string{"Send": "/wst/api/m/mobile/sendMessage/:id"}
}

// Send 回显捕获到的 :id，验证 ctx.Param 已被分发回填。
func (*paramRecv) Send(ctx *gin.Context) string { return ctx.Param("id") }

// serveRoot 用真实 gin 引擎挂 NoRoute 根 handler 跑一次请求，返回状态码与响应体。
// 必须走 engine.ServeHTTP：裸 recorder 读不到 c.Status 分支的状态码。
func serveRoot(t *testing.T, method, path string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(nil))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code, w.Body.String()
}

// TestParamRouteMatchAndCapture 参数路由命中并捕获 :id。
func TestParamRouteMatchAndCapture(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&paramRecv{})

	if code, body := serveRoot(t, http.MethodGet, "/wst/api/m/mobile/sendMessage/5"); code != http.StatusOK || body != "5" {
		t.Fatalf("✘ 参数路由未命中或 :id 未捕获: %d %q（期望 200 \"5\"）", code, body)
	}
	// 不同取值各自捕获。
	if code, body := serveRoot(t, http.MethodGet, "/wst/api/m/mobile/sendMessage/abc123"); code != http.StatusOK || body != "abc123" {
		t.Fatalf("✘ :id 捕获错误: %d %q（期望 200 \"abc123\"）", code, body)
	}
}

// TestParamRouteMissBranches 空参数段 / 段数不符应 404；方法不匹配应 405+Allow（A-F8）。
func TestParamRouteMissBranches(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&paramRecv{})

	for _, tc := range []struct {
		name, method, path string
	}{
		{"尾斜杠空参数", http.MethodGet, "/wst/api/m/mobile/sendMessage/"},
		{"段数过多", http.MethodGet, "/wst/api/m/mobile/sendMessage/5/extra"},
		{"段数不足", http.MethodGet, "/wst/api/m/mobile/sendMessage"},
	} {
		if code, _ := serveRoot(t, tc.method, tc.path); code != http.StatusNotFound {
			t.Fatalf("✘ %s：%s %s 应 404，实际 %d", tc.name, tc.method, tc.path, code)
		}
	}
	// 模式命中但方法未注册（GET 注册，POST 请求）：405 + Allow。
	code, _ := serveRoot(t, http.MethodPost, "/wst/api/m/mobile/sendMessage/5")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ 方法不匹配：%s 应 405，实际 %d", "POST /wst/api/m/mobile/sendMessage/5", code)
	}
}

// TestLiteralRouteBeatsParam 同段数下字面路径必须优先于参数模式命中。
func TestLiteralRouteBeatsParam(t *testing.T) {
	isolateRegistry(t)
	// 参数模式 /lit/x/:id 与字面 /lit/x/one 段数相同；请求 /lit/x/one 应命中字面。
	setRoute("/lit/x/one", methodAny, echoHandler("literal"))
	RegisterRouter(&litParamRecv{})

	if code, body := serveRoot(t, http.MethodGet, "/lit/x/one"); code != http.StatusOK || body != "literal" {
		t.Fatalf("✘ 字面路由未优先于参数路由: %d %q（期望 200 \"literal\"）", code, body)
	}
	if code, body := serveRoot(t, http.MethodGet, "/lit/x/two"); code != http.StatusOK || body != "param:two" {
		t.Fatalf("✘ 非字面取值未落到参数路由: %d %q（期望 200 \"param:two\"）", code, body)
	}
}

type litParamRecv struct{}

func (*litParamRecv) RouterType() []string { return nil } // 通配方法
func (*litParamRecv) RouterPath() map[string]string {
	return map[string]string{"Get": "/lit/x/:id"}
}
func (*litParamRecv) Get(ctx *gin.Context) string { return "param:" + ctx.Param("id") }

// TestParamRouteWildcardMethod 未声明 RouterType 的参数路由（通配）应对任意方法放行。
func TestParamRouteWildcardMethod(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&litParamRecv{})
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		code, body := serveRoot(t, m, "/lit/x/"+strconv.Itoa(7))
		if code != http.StatusOK || body != "param:7" {
			t.Fatalf("✘ 通配参数路由 %s 未命中: %d %q", m, code, body)
		}
	}
}
