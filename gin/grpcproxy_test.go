package gin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// 反向 gRPC 路由代理（gin 侧）回归：字面 / :name 参数路由经 routeEntry.proxy 转发，
// 命中后把 MatchedPattern + Params 传给 proxy，响应回写网页端；Unregister 按属主精确回收。
// ============================================================================

// fakeProxy 记录收到的 ProxyRequest，并返回预置响应或错误。
type fakeProxy struct {
	got *ProxyRequest
	rsp *ProxyResponse
	err error
}

func (f *fakeProxy) Do(_ context.Context, req *ProxyRequest) (*ProxyResponse, error) {
	f.got = req
	if f.err != nil {
		return nil, f.err
	}
	return f.rsp, nil
}

// serve 用根劫持 handler 真实分发一次请求。
func serve(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := newRootHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	c.Request = httptest.NewRequest(method, path, reader)
	h(c)
	return w
}

func TestGRPCProxyLiteralRoute(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusCreated, Header: http.Header{"X-Proxied": []string{"yes"}}, Body: []byte("hello-from-client")}}
	RegisterGRPCRoute("backend", 1, "/proxy/hello", http.MethodGet, fp)

	w := serve(t, http.MethodGet, "/proxy/hello", "q=1")
	if w.Code != http.StatusCreated {
		t.Fatalf("✘ 期望 201，实际 %d", w.Code)
	}
	if w.Body.String() != "hello-from-client" {
		t.Fatalf("✘ 响应体不符: %q", w.Body.String())
	}
	if w.Header().Get("X-Proxied") != "yes" {
		t.Fatalf("✘ 响应头未回写: %v", w.Header())
	}
	if fp.got == nil {
		t.Fatal("✘ proxy 未被调用")
	}
	if fp.got.Path != "/proxy/hello" || fp.got.MatchedPattern != "/proxy/hello" || fp.got.Method != http.MethodGet {
		t.Fatalf("✘ 请求快照不符: %+v", fp.got)
	}
	// 方法不匹配（GET 注册，POST 请求）应 405 + Allow（A-F8）：路径已注册、方法未注册。
	w = serve(t, http.MethodPost, "/proxy/hello", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ POST 未注册方法应 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("✘ 405 应带 Allow: GET，实际 %q", allow)
	}
}

func TestGRPCProxyParamRoute(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("user")}}
	RegisterGRPCRoute("backend", 1, "/user/:id", http.MethodGet, fp)

	w := serve(t, http.MethodGet, "/user/42", "")
	if w.Code != http.StatusOK {
		t.Fatalf("✘ 参数路由代理期望 200，实际 %d", w.Code)
	}
	if fp.got == nil {
		t.Fatal("✘ proxy 未被调用")
	}
	if fp.got.Path != "/user/42" {
		t.Fatalf("✘ 真实路径应为 /user/42，实际 %q", fp.got.Path)
	}
	if fp.got.MatchedPattern != "/user/:id" {
		t.Fatalf("✘ MatchedPattern 应为模板 /user/:id，实际 %q", fp.got.MatchedPattern)
	}
	if fp.got.Params["id"] != "42" {
		t.Fatalf("✘ 参数捕获 id=42 缺失，实际 %v", fp.got.Params)
	}
}

func TestGRPCProxyMethodAny(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("any")}}
	// method 传空 → 通配键，任意方法都命中。
	RegisterGRPCRoute("backend", 1, "/wild", "", fp)

	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if w := serve(t, m, "/wild", ""); w.Code != http.StatusOK {
			t.Fatalf("✘ %s /wild 应命中通配代理，实际 %d", m, w.Code)
		}
	}
}

func TestGRPCProxyErrorReturnsBadGateway(t *testing.T) {
	isolateRegistry(t)
	fp := &fakeProxy{err: errors.New("client gone")}
	RegisterGRPCRoute("backend", 1, "/boom", http.MethodGet, fp)
	if w := serve(t, http.MethodGet, "/boom", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("✘ proxy 出错应回 502，实际 %d body=%q", w.Code, w.Body.String())
	}
}

func TestGRPCProxyUnregisterByOwner(t *testing.T) {
	isolateRegistry(t)
	// 本地路由（fn 型，owner 为空）不受影响
	setRoute("/local", methodAny, echoHandler("L"))
	// backend1：字面 + 参数；backend2：同一 paramRoute 的另一个方法
	RegisterGRPCRoute("backend1", 1, "/b1/hello", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend1", 1, "/b1/user/:id", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend2", 2, "/shared/:id", http.MethodPost, &fakeProxy{rsp: &ProxyResponse{Status: 200}})
	RegisterGRPCRoute("backend1", 1, "/shared/:id", http.MethodGet, &fakeProxy{rsp: &ProxyResponse{Status: 200}})

	UnregisterGRPCRoutes("backend1", 1)

	// backend1 字面路由应 404
	if w := serve(t, http.MethodGet, "/b1/hello", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ backend1 字面路由未回收，实际 %d", w.Code)
	}
	// backend1 参数路由应 404
	if w := serve(t, http.MethodGet, "/b1/user/7", ""); w.Code != http.StatusNotFound {
		t.Fatalf("✘ backend1 参数路由未回收，实际 %d", w.Code)
	}
	// 本地路由仍在
	if w := serve(t, http.MethodGet, "/local", ""); w.Code != http.StatusOK || w.Body.String() != "L" {
		t.Fatalf("✘ 本地路由被误删: %d %q", w.Code, w.Body.String())
	}
	// /shared/:id 的 backend1(GET) 回收，但 backend2(POST) 保留；
	// GET 请求撞上仍在的 POST 模式 -> 405 + Allow（A-F8 口径），不再是 404。
	w := serve(t, http.MethodGet, "/shared/9", "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("✘ /shared GET(backend1) 应已回收且回 405，实际 %d", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("✘ 405 应带 Allow: POST，实际 %q", allow)
	}
	fp2 := &fakeProxy{rsp: &ProxyResponse{Status: http.StatusOK, Body: []byte("post-ok")}}
	RegisterGRPCRoute("backend2", 2, "/shared/:id", http.MethodPost, fp2) // 覆盖确认仍在 backend2
	if w := serve(t, http.MethodPost, "/shared/9", ""); w.Code != http.StatusOK {
		t.Fatalf("✘ /shared POST(backend2) 应保留，实际 %d", w.Code)
	}
}
