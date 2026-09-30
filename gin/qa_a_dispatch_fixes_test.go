package gin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 槽位 A 审查修复（A-F1/F2/F3/F4/F5/F8）回归用例：
//   - A-F1：HTTP 分发（真实 engine.ServeHTTP）与 RegisterGRPCRoute/UnregisterGRPCRoutes
//     并发压测——修复前 lookupRoute 无锁读 routerMap，会 fatal concurrent map read/write；
//   - A-F2：代理注册不得覆盖 owner 不同的既有条目（含本地 fn 路由）；
//   - A-F3：owner+session 双键回收，旧会话延迟回收不得抹掉新会话条目；
//   - A-F4：请求体超限回 413，上限内正常转发；
//   - A-F5：hop-by-hop 头与 Content-Length 剔除、非法 status 回 502；
//   - A-F8：路径在而方法不在 -> 405 + Allow；通配条目不触发 405。
// ============================================================================

// nopProxy 无状态代理替身：并发压测下多 goroutine 共享一个实例也安全
// （fakeProxy 会记录 got，存在写竞争，压测不用它）。
type nopProxy struct {
	status int
	body   string
}

func (n *nopProxy) Do(_ context.Context, _ *ProxyRequest) (*ProxyResponse, error) {
	return &ProxyResponse{Status: n.status, Body: []byte(n.body)}, nil
}

// TestQaAConcurrentDispatchVsRegistration A-F1 先红后绿的压测主体：
// 读侧走真实 engine.ServeHTTP（根劫持 -> matchRoute -> lookupRoute），
// 写侧高频 Register/Unregister（字面 + 参数路由都有）。
// 修复前（lookupRoute 无锁读）：runtime fatal error: concurrent map read and map write，
// 测试进程直接崩溃；修复后（RLock）：全绿。
func TestQaAConcurrentDispatchVsRegistration(t *testing.T) {
	isolateRegistry(t)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(nil))

	setRoute("/qa/stable", methodAny, echoHandler("stable"))
	proxy := &nopProxy{status: http.StatusOK, body: "proxied"}

	const writers, readers = 4, 8
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var served, hits atomic.Int64

	// 写侧：注册/回收交错，模拟后端连接-断连风暴（rpc 消息循环的运行期写）。
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sess := uint64(n + 1)
			lit := fmt.Sprintf("/qa/churn/%d", n)
			par := fmt.Sprintf("/qa/churn/%d/:id", n)
			for {
				select {
				case <-stop:
					return
				default:
				}
				RegisterGRPCRoute("qa-backend", sess, lit, http.MethodGet, proxy)
				RegisterGRPCRoute("qa-backend", sess, par, http.MethodGet, proxy)
				UnregisterGRPCRoutes("qa-backend", sess)
			}
		}(i)
	}

	// 读侧：真实 ServeHTTP，混合命中/未命中路径，逼出 routerMap 与 paramRoutes 的并发读。
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, p := range []string{
					fmt.Sprintf("/qa/churn/%d", n%writers),
					fmt.Sprintf("/qa/churn/%d/42", n%writers),
					"/qa/stable",
					fmt.Sprintf("/qa/miss/%d", n),
				} {
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
					served.Add(1)
					if w.Code == http.StatusOK {
						hits.Add(1)
					}
				}
			}
		}(i)
	}

	time.Sleep(800 * time.Millisecond)
	close(stop)
	wg.Wait()

	if served.Load() == 0 {
		t.Fatal("✘ 读侧没有发出任何请求，用例没跑到并发路径")
	}
	if hits.Load() == 0 {
		t.Fatal("✘ 读侧从未命中，churn 注册没有生效，压测覆盖面不足")
	}
	// 稳定路由必须始终命中
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/qa/stable", nil))
	if w.Code != http.StatusOK || w.Body.String() != "stable" {
		t.Fatalf("✘ 稳定路由在压测后被破坏: %d %q", w.Code, w.Body.String())
	}
}

// TestQaARegisterConflictRejected A-F2：owner 不同拒绝覆盖，owner 相同允许覆盖。
func TestQaARegisterConflictRejected(t *testing.T) {
	isolateRegistry(t)

	// 1. 本地 fn 路由（owner==""）不得被任何代理注册覆盖
	setRoute("/pay/callback", methodAny, echoHandler("local-fn"))
	intruder := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("hijacked")}}
	RegisterGRPCRoute("evil", 1, "/pay/callback", methodAny, intruder)
	if w := serve(t, http.MethodGet, "/pay/callback", ""); w.Body.String() != "local-fn" {
		t.Fatalf("✘ 本地路由被代理条目覆盖: %q", w.Body.String())
	}
	if intruder.got != nil {
		t.Fatal("✘ 被拒的代理注册仍收到了请求")
	}

	// 2. 其他客户端的代理条目不得被覆盖
	proxyA := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("A")}}
	proxyB := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("B")}}
	RegisterGRPCRoute("clientA", 1, "/owned/route", http.MethodGet, proxyA)
	RegisterGRPCRoute("clientB", 1, "/owned/route", http.MethodGet, proxyB)
	if w := serve(t, http.MethodGet, "/owned/route", ""); w.Body.String() != "A" {
		t.Fatalf("✘ clientA 条目被 clientB 覆盖: %q", w.Body.String())
	}
	if proxyB.got != nil {
		t.Fatal("✘ 被拒注册的 proxyB 收到了请求")
	}

	// 3. 同 owner（重连/重注册）允许覆盖
	proxyA2 := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("A2")}}
	RegisterGRPCRoute("clientA", 2, "/owned/route", http.MethodGet, proxyA2)
	if w := serve(t, http.MethodGet, "/owned/route", ""); w.Body.String() != "A2" {
		t.Fatalf("✘ 同 owner 覆盖未生效: %q", w.Body.String())
	}

	// 4. 参数路由同口径
	pA := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("pa")}}
	pB := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("pb")}}
	RegisterGRPCRoute("clientA", 2, "/owned/:id", http.MethodGet, pA)
	RegisterGRPCRoute("clientB", 1, "/owned/:id", http.MethodGet, pB)
	if w := serve(t, http.MethodGet, "/owned/7", ""); w.Body.String() != "pa" {
		t.Fatalf("✘ 参数路由被异 owner 覆盖: %q", w.Body.String())
	}
}

// TestQaASessionGenerationReclaim A-F3：同名重连交错——旧会话延迟回收不得抹掉新会话条目。
func TestQaASessionGenerationReclaim(t *testing.T) {
	isolateRegistry(t)
	oldSess := &nopProxy{status: 200, body: "old"}
	newSess := &nopProxy{status: 200, body: "new"}

	// 旧会话（id=10）注册字面 + 参数路由
	RegisterGRPCRoute("backend", 10, "/sess/route", http.MethodGet, oldSess)
	RegisterGRPCRoute("backend", 10, "/sess/p/:id", http.MethodGet, oldSess)

	// 同名重连：新会话（id=11）同 owner 覆盖注册
	RegisterGRPCRoute("backend", 11, "/sess/route", http.MethodGet, newSess)
	RegisterGRPCRoute("backend", 11, "/sess/p/:id", http.MethodGet, newSess)

	// 旧会话此时才执行延迟回收（closeSession 的 TOCTOU 窗口）：
	// 双键（owner+session）判定下，新会话条目必须完好。
	UnregisterGRPCRoutes("backend", 10)

	if w := serve(t, http.MethodGet, "/sess/route", ""); w.Code != http.StatusOK || w.Body.String() != "new" {
		t.Fatalf("✘ 新会话字面路由被旧会话回收抹掉: %d %q", w.Code, w.Body.String())
	}
	if w := serve(t, http.MethodGet, "/sess/p/3", ""); w.Code != http.StatusOK || w.Body.String() != "new" {
		t.Fatalf("✘ 新会话参数路由被旧会话回收抹掉: %d %q", w.Code, w.Body.String())
	}

	// 错误 session 的回收同样不生效
	UnregisterGRPCRoutes("backend", 999)
	if w := serve(t, http.MethodGet, "/sess/route", ""); w.Code != http.StatusOK {
		t.Fatalf("✘ 无关 session 回收误删条目: %d", w.Code)
	}

	// 新会话自己回收才真正摘除
	UnregisterGRPCRoutes("backend", 11)
	if w := serve(t, http.MethodGet, "/sess/route", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ 新会话回收后应 404，实际 %d", w.Code)
	}
	if w := serve(t, http.MethodGet, "/sess/p/3", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ 新会话参数路由回收后应 404，实际 %d", w.Code)
	}
}

// TestQaARequestBodyLimit A-F4：超限 413，上限内正常转发。
func TestQaARequestBodyLimit(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: 200, Body: []byte("ok")}}
	RegisterGRPCRoute("backend", 1, "/big", http.MethodPost, fp)

	// 恰好等于上限：放行
	atLimit := strings.Repeat("a", maxProxyRequestBody)
	w := serve(t, http.MethodPost, "/big", atLimit)
	if w.Code != http.StatusOK {
		t.Fatalf("✘ 上限内请求应 200，实际 %d", w.Code)
	}
	if fp.got == nil || len(fp.got.Body) != maxProxyRequestBody {
		t.Fatalf("✘ 上限内请求体未完整转发: got=%v", fp.got)
	}

	// 超限 1 字节：413，且不进 proxy
	fp.got = nil
	over := strings.Repeat("b", maxProxyRequestBody+1)
	w = serve(t, http.MethodPost, "/big", over)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("✘ 超限请求应 413，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "413") {
		t.Fatalf("✘ 413 响应体口径不符: %q", w.Body.String())
	}
	if fp.got != nil {
		t.Fatal("✘ 超限请求不应转发给远端")
	}
}

// TestQaAResponseHeaderFilterAndStatusClamp A-F5。
func TestQaAResponseHeaderFilterAndStatusClamp(t *testing.T) {
	isolateRegistry(t)

	// 1. hop-by-hop 头与 Content-Length 剔除，端到端头保留
	fp := &fakeProxy{rsp: &ProxyResponse{
		Status: 200,
		Header: http.Header{
			"Connection":          {"close"},
			"Keep-Alive":          {"timeout=5"},
			"Proxy-Authenticate":  {"Basic"},
			"Proxy-Authorization": {"Basic abc"},
			"Te":                  {"trailers"},
			"Trailer":             {"Expires"},
			"Transfer-Encoding":   {"chunked"},
			"Upgrade":             {"websocket"},
			"Content-Length":      {"999"},
			"X-End-To-End":        {"keep"},
		},
		Body: []byte("hello"),
	}}
	RegisterGRPCRoute("backend", 1, "/hdr", http.MethodGet, fp)
	w := serve(t, http.MethodGet, "/hdr", "")
	for _, h := range []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		if v := w.Header().Get(h); v != "" {
			t.Fatalf("✘ hop-by-hop 头 %s 被透传: %q", h, v)
		}
	}
	if v := w.Header().Get("Content-Length"); v == "999" {
		t.Fatalf("✘ 远端 Content-Length=999 被透传（应由 Go 自行成帧）")
	}
	if w.Header().Get("X-End-To-End") != "keep" {
		t.Fatalf("✘ 端到端头被误删: %v", w.Header())
	}

	// 2. 非法 status 钳制为 502
	for _, bad := range []int{-100, -1, 99, 600, 1000} {
		isolateRegistry(t)
		badFp := &fakeProxy{rsp: &ProxyResponse{Status: bad, Body: []byte("x")}}
		RegisterGRPCRoute("backend", 1, "/bad-status", http.MethodGet, badFp)
		if w := serve(t, http.MethodGet, "/bad-status", ""); w.Code != http.StatusBadGateway {
			t.Fatalf("✘ status=%d 应回 502，实际 %d", bad, w.Code)
		}
	}

	// 3. 边界内 status 透传。
	// 注：1xx（如 100）无法用 httptest.ResponseRecorder 断言——recorder 把
	// 1xx 按 informational 处理、不落 w.Code（Go 标准库行为），这里不测；
	// 100 的钳制边界由「99 回 502」的反向用例间接覆盖。
	for _, good := range []int{204, 301, 599} {
		isolateRegistry(t)
		okFp := &fakeProxy{rsp: &ProxyResponse{Status: good}}
		RegisterGRPCRoute("backend", 1, "/good-status", http.MethodGet, okFp)
		// 必须走真实引擎：gin 的 WriteHeaderNow 由引擎在 handler 返回后触发，
		// 裸 handler 直调时无 body 的状态码不会落到 recorder。
		if w := serveRootFull(t, http.MethodGet, "/good-status"); w.Code != good {
			t.Fatalf("✘ status=%d 应透传，实际 %d", good, w.Code)
		}
	}
}

// TestQaAMethodNotAllowed A-F8：405 + Allow；通配不触发；完全未注册仍 404 无 Allow。
func TestQaAMethodNotAllowed(t *testing.T) {
	isolateRegistry(t)
	setRoute("/m", http.MethodPost, echoHandler("P"))
	setRoute("/m", http.MethodGet, echoHandler("G"))
	setRoute("/any", methodAny, echoHandler("A"))
	RegisterGRPCRoute("backend", 1, "/pm/:id", http.MethodDelete, &nopProxy{status: 200, body: "d"})

	// 多方法路径：Allow 按字典序列出全部已注册方法
	w := serveRootFull(t, http.MethodDelete, "/m")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ DELETE /m 应 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("✘ Allow 头不符: %q（期望 \"GET, POST\"）", allow)
	}

	// 通配条目：任意方法都命中，不会走到 405
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if w := serveRootFull(t, m, "/any"); w.Code != http.StatusOK {
			t.Fatalf("✘ 通配路径 %s 应 200，实际 %d", m, w.Code)
		}
	}

	// 参数模式命中但方法不匹配：405 + Allow: DELETE
	w = serveRootFull(t, http.MethodGet, "/pm/9")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodDelete {
		t.Fatalf("✘ 参数路由方法不匹配应 405+Allow:DELETE，实际 %d %q", w.Code, w.Header().Get("Allow"))
	}

	// 完全未注册：404 且无 Allow 头
	w = serveRootFull(t, http.MethodGet, "/nothing/here")
	if w.Code != http.StatusNotFound {
		t.Fatalf("✘ 未注册路径应 404，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "" {
		t.Fatalf("✘ 404 不应带 Allow 头: %q", allow)
	}
}

// serveRootFull 用真实 gin 引擎跑一次请求，返回 recorder（可读头与状态码）。
func serveRootFull(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.NoRoute(newRootHandler(nil))
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}
