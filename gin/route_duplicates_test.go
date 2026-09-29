package gin

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 重复注册判定按「路径 + 方法」两级：
//  1. 同路径不同方法各自注册、互不覆盖（旧版整条 entry 替换会静默丢失先注册的方法）；
//  2. 同路径同方法才算重复注册，后者覆盖前者；
//  3. 精确方法条目优先于通配条目（RouterType 未声明时注册的放行所有方法条目）。
// 复用 run_route_test.go 的 isolateRegistry / invokeRoute。
// ============================================================================

// 两个类型都用显式路径注册到同一 URL /dup/api，方法白名单分别为 GET / POST。
type dupGetRecv struct{}

func (*dupGetRecv) RouterType() []string          { return []string{"GET"} }
func (*dupGetRecv) RouterPath() map[string]string { return map[string]string{"Do": "/dup/api"} }
func (*dupGetRecv) Do(_ *gin.Context) string      { return "get" }

type dupPostRecv struct{}

func (*dupPostRecv) RouterType() []string          { return []string{"POST"} }
func (*dupPostRecv) RouterPath() map[string]string { return map[string]string{"Do": "/dup/api"} }
func (*dupPostRecv) Do(_ *gin.Context) string      { return "post" }

// TestSamePathDifferentMethodsCoexist 两次 RegisterRouter 传入同一显式路径、
// 但方法分别为 GET / POST：不得判为重复注册，两个 handler 都必须可命中。
func TestSamePathDifferentMethodsCoexist(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&dupGetRecv{})
	RegisterRouter(&dupPostRecv{})

	if got := invokeRoute(t, "/dup/api", http.MethodGet); got != "get" {
		t.Fatalf("✘ GET handler 被 POST 注册覆盖，响应 %q（应为 get）", got)
	}
	if got := invokeRoute(t, "/dup/api", http.MethodPost); got != "post" {
		t.Fatalf("✘ POST handler 未各自注册，响应 %q（应为 post）", got)
	}
	// 既无精确条目也无通配条目的方法 -> 不命中注册表
	routerMu.Lock()
	hit := lookupRoute("/dup/api", http.MethodDelete)
	routerMu.Unlock()
	if hit != nil {
		t.Fatal("✘ DELETE 未注册却命中了路由")
	}
}

// TestSamePathSameMethodOverwrite 同路径同方法重复注册：后者覆盖前者。
func TestSamePathSameMethodOverwrite(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&getRecv{tag: "old"})
	RegisterRouter(&getRecv{tag: "new"})

	if got := invokeRoute(t, "/getrecv/ping", http.MethodGet); got != "ping:new" {
		t.Fatalf("✘ 同方法重复注册未被后者覆盖，响应 %q（应为 ping:new）", got)
	}
}

// TestExactMethodBeatsWildcard 同一路径先注册通配（RouterType 为 nil）、
// 再注册精确 GET：GET 命中精确 handler，其余方法回退通配 handler。
func TestExactMethodBeatsWildcard(t *testing.T) {
	isolateRegistry(t)
	setRoute("/mix/api", methodAny, echoHandler("any"))
	setRoute("/mix/api", http.MethodGet, echoHandler("get"))

	h := newRootHandler(nil)
	c, w := newTestCtx(http.MethodGet, "/mix/api")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "get" {
		t.Fatalf("✘ 精确方法未优先于通配: %d %q", w.Code, w.Body.String())
	}
	c, w = newTestCtx(http.MethodPost, "/mix/api")
	h(c)
	if w.Code != http.StatusOK || w.Body.String() != "any" {
		t.Fatalf("✘ 未回退通配条目: %d %q", w.Code, w.Body.String())
	}
}
