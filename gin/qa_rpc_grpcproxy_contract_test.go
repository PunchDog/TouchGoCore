package gin

import (
	"net/http"
	"testing"
)

// ============================================================================
// 槽位 RPC 复核修复回归（T6-G）：
//   - P2-1：RegisterGRPCRoute 返回 accepted——被拒条目必须回 false，
//     供 rpc 服务端只把成功落库的条目计入注册回执；
//   - P3-3：状态钳制收紧为 [200,599]，1xx 信息性状态不得作为最终状态放行。
// ============================================================================

// TestQaRpcRegisterGRPCRouteAcceptedFlag P2-1：accepted 语义契约。
// 修复前本函数无返回值，本用例无法编译（红）；修复后逐分支断言（绿）。
func TestQaRpcRegisterGRPCRouteAcceptedFlag(t *testing.T) {
	isolateRegistry(t)
	proxyA := &nopProxy{status: 200, body: "A"}
	proxyB := &nopProxy{status: 200, body: "B"}

	// 首次注册：落库，accepted=true
	if !RegisterGRPCRoute("qa-rpc-ownerA", 1, "/qa-rpc-acc/x", http.MethodGet, proxyA) {
		t.Fatal("✘ A 首次注册应 accepted=true")
	}
	// 跨 owner 覆盖：被拒，accepted=false（修复前裸 return、rpc 侧照样计数）
	if RegisterGRPCRoute("qa-rpc-ownerB", 2, "/qa-rpc-acc/x", http.MethodGet, proxyB) {
		t.Fatal("✘ 跨 owner 覆盖被拒应返回 accepted=false")
	}
	// 同 path 不同方法是不同键：B 可注册，accepted=true
	if !RegisterGRPCRoute("qa-rpc-ownerB", 2, "/qa-rpc-acc/x", http.MethodPost, proxyB) {
		t.Fatal("✘ 同 path 不同方法应 accepted=true")
	}
	// 同属主覆盖（重连/重注册）：accepted=true
	if !RegisterGRPCRoute("qa-rpc-ownerA", 3, "/qa-rpc-acc/x", http.MethodGet, proxyA) {
		t.Fatal("✘ 同属主覆盖应 accepted=true")
	}
	// 参数路由同口径
	if !RegisterGRPCRoute("qa-rpc-ownerA", 1, "/qa-rpc-acc/:id", http.MethodGet, proxyA) {
		t.Fatal("✘ 参数路由首次注册应 accepted=true")
	}
	if RegisterGRPCRoute("qa-rpc-ownerB", 1, "/qa-rpc-acc/:id", http.MethodGet, proxyB) {
		t.Fatal("✘ 参数路由跨 owner 覆盖应 accepted=false")
	}
	// 非法参数：一律 false
	if RegisterGRPCRoute("", 1, "/qa-rpc-acc/y", "", proxyA) {
		t.Fatal("✘ 空 client 名应 accepted=false")
	}
	if RegisterGRPCRoute("qa-rpc-ownerA", 1, "", "", proxyA) {
		t.Fatal("✘ 空 path 应 accepted=false")
	}
	if RegisterGRPCRoute("qa-rpc-ownerA", 1, "/qa-rpc-acc/y", "", nil) {
		t.Fatal("✘ nil stream 应 accepted=false")
	}

	UnregisterGRPCRoutes("qa-rpc-ownerA", 3)
	UnregisterGRPCRoutes("qa-rpc-ownerA", 1)
	UnregisterGRPCRoutes("qa-rpc-ownerB", 2)
}

// TestQaRpcProxyInformationalStatusBecomes502 P3-3：1xx 是信息性响应，
// 不得作为最终状态透传（101 无 hijack 属异常；Go net/http 把 WriteHeader(1xx)
// 当 interim 处理，网页端会拿到畸形/悬挂响应）。修复前 1xx 通过钳制，
// w.Code 不是 502（红）；修复后统一 502（绿）。
func TestQaRpcProxyInformationalStatusBecomes502(t *testing.T) {
	for _, s := range []int{100, 101, 199} {
		isolateRegistry(t)
		RegisterGRPCRoute("backend", 1, "/qa-rpc-1xx", http.MethodGet, &nopProxy{status: s})
		if w := serveRootFull(t, http.MethodGet, "/qa-rpc-1xx"); w.Code != http.StatusBadGateway {
			t.Fatalf("✘ status=%d 应钳制为 502，实际 %d", s, w.Code)
		}
	}
	// 200/204 等合法最终状态不受影响
	isolateRegistry(t)
	RegisterGRPCRoute("backend", 1, "/qa-rpc-ok", http.MethodGet, &nopProxy{status: 204})
	if w := serveRootFull(t, http.MethodGet, "/qa-rpc-ok"); w.Code != 204 {
		t.Fatalf("✘ status=204 应透传，实际 %d", w.Code)
	}
}
