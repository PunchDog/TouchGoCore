package gin

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// IRouterPath 显式路径扩展（方案 A）的回归用例：
//  1. 实现 IRouterPath 的类按显式路径注册；
//  2. RouterPath 方法本身不会被注册为路由 handler；
//  3. 未实现 IRouterPath 的类仍走默认推导，行为不变（proxywork 零回归）。
// 复用 run_route_test.go 的 isolateRegistry / invokeRoute / routerKeys。
// ============================================================================

type pathRecv struct{}

func (*pathRecv) RouterType() []string { return []string{"POST"} }
func (*pathRecv) RouterPath() map[string]string {
	return map[string]string{"Login": "/wst/api/m/login"}
}
func (*pathRecv) Login(_ *gin.Context) string { return "login-ok" }

type plainRecv struct{}

func (*plainRecv) RouterType() []string        { return []string{"GET"} }
func (*plainRecv) Hello(_ *gin.Context) string { return "hi" }

// TestIRouterPathExplicitPath 显式路径生效，且 RouterPath/RouterType 不注册为路由。
func TestIRouterPathExplicitPath(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&pathRecv{}, nil)

	keys := routerKeys()
	if len(keys) != 1 {
		t.Fatalf("✘ 应只注册 1 条路由，实际 %d: %v", len(keys), keys)
	}
	if keys[0] != "/wst/api/m/login|POST" {
		t.Fatalf("✘ 显式路径未生效: %v", keys)
	}
	if got := invokeRoute(t, keys[0]); got != "login-ok" {
		t.Fatalf("✘ handler 响应 %q != login-ok", got)
	}
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), "routerpath") {
			t.Fatalf("✘ RouterPath 被错误注册为路由: %v", keys)
		}
	}
}

// TestDefaultDerivationUnchanged 未实现 IRouterPath 的类仍走默认推导。
func TestDefaultDerivationUnchanged(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&plainRecv{}, nil)

	keys := routerKeys()
	if len(keys) != 1 || keys[0] != "/plainrecv/hello|GET" {
		t.Fatalf("✘ 默认推导行为被改变: %v", keys)
	}
	if got := invokeRoute(t, keys[0]); got != "hi" {
		t.Fatalf("✘ handler 响应 %q != hi", got)
	}
}

// TestPartialRouterPathFallsBack 实现了 IRouterPath 但方法未列出时回退默认推导。
func TestPartialRouterPathFallsBack(t *testing.T) {
	isolateRegistry(t)
	RegisterRouter(&partialRecv{}, nil)

	keys := routerKeys()
	if len(keys) != 2 {
		t.Fatalf("✘ 应注册 2 条路由，实际 %d: %v", len(keys), keys)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found["/wst/api/m/explicit|POST"] {
		t.Fatalf("✘ 显式路径缺失: %v", keys)
	}
	if !found["/partialrecv/fallback|POST"] {
		t.Fatalf("✘ 未列出方法未回退默认推导: %v", keys)
	}
}

type partialRecv struct{}

func (*partialRecv) RouterType() []string { return []string{"POST"} }
func (*partialRecv) RouterPath() map[string]string {
	return map[string]string{"Explicit": "/wst/api/m/explicit"}
}
func (*partialRecv) Explicit(_ *gin.Context) string { return "explicit" }
func (*partialRecv) Fallback(_ *gin.Context) string { return "fallback" }
