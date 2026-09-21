package gin

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// QA · M1 验收补充用例（新增，非交付源码）：
//   A2(c) 同一个注册表内，IRouterPath 显式路径与默认推导路径并存、互不干扰。
// 复用 run_route_test.go 的 isolateRegistry / invokeRoute / routerKeys。
// ============================================================================

// qaExplicitRecv 实现 IRouterPath，Login 走显式路径。
type qaExplicitRecv struct{}

func (*qaExplicitRecv) RouterType() []string { return []string{"POST"} }
func (*qaExplicitRecv) RouterPath() map[string]string {
	return map[string]string{"Login": "/wst/api/m/login"}
}
func (*qaExplicitRecv) Login(_ *gin.Context) string { return "explicit-ok" }

// qaDerivedRecv 不实现 IRouterPath，完全走默认推导。
type qaDerivedRecv struct{}

func (*qaDerivedRecv) RouterType() []string             { return []string{"POST"} }
func (*qaDerivedRecv) AddUrlList(_ *gin.Context) string { return "derived-ok" }

// TestQAMixedExplicitAndDerivedPaths 显式与推导混用：两条路由都在、路径互不污染、handler 各自正确。
func TestQAMixedExplicitAndDerivedPaths(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&qaExplicitRecv{}, nil)
	RegisterRouter(&qaDerivedRecv{}, nil)

	keys := routerKeys()
	if len(keys) != 2 {
		t.Fatalf("✘ 应注册 2 条路由，实际 %d: %v", len(keys), keys)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	const explicitKey = "/wst/api/m/login|POST"
	const derivedKey = "/qaderivedrecv/addurllist|POST"
	if !found[explicitKey] {
		t.Fatalf("✘ 显式路径缺失 %q: %v", explicitKey, keys)
	}
	if !found[derivedKey] {
		t.Fatalf("✘ 默认推导路径缺失 %q: %v", derivedKey, keys)
	}
	if got := invokeRoute(t, explicitKey); got != "explicit-ok" {
		t.Fatalf("✘ 显式 handler 响应 %q != explicit-ok", got)
	}
	if got := invokeRoute(t, derivedKey); got != "derived-ok" {
		t.Fatalf("✘ 推导 handler 响应 %q != derived-ok", got)
	}
}

// TestQANoPathInterfaceStillTwoSegment 反证：不实现 IRouterPath 时，路径严格是「/小写类型/小写方法」两段。
func TestQANoPathInterfaceStillTwoSegment(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&qaDerivedRecv{}, nil)

	keys := routerKeys()
	if len(keys) != 1 || keys[0] != "/qaderivedrecv/addurllist|POST" {
		t.Fatalf("✘ 两段推导规则被改变: %v", keys)
	}
}
