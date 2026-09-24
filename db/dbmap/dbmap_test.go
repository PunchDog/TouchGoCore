package dbmap

import (
	"testing"

	"touchgocore/syncmap"
)

func TestAppRegistry_AppThenGlobalFallback(t *testing.T) {
	prevApp := appRegistry
	t.Cleanup(func() { appRegistry = prevApp })

	appRegistry = nil
	Store("global-only", "g")

	appMap := syncmap.NewAny()
	UseAppRegistry(appMap)
	Store("app-only", "a")

	if v, ok := Get("app-only"); !ok || v != "a" {
		t.Fatalf("app hit: %v %v", v, ok)
	}
	if v, ok := Get("global-only"); !ok || v != "g" {
		t.Fatalf("global fallback: %v %v", v, ok)
	}
	if Registry() != appMap {
		t.Fatal("Registry should return App map")
	}
}

func TestAppRegistry_GlobalOnlyWhenUnbound(t *testing.T) {
	prevApp := appRegistry
	t.Cleanup(func() { appRegistry = prevApp })
	appRegistry = nil

	Store("k", 1)
	if v, ok := Get("k"); !ok || v != 1 {
		t.Fatalf("global only: %v %v", v, ok)
	}
	if Registry() != Global {
		t.Fatal("Registry should return Global when App unbound")
	}
}

func TestAppRegistry_DeleteBoth(t *testing.T) {
	prevApp := appRegistry
	t.Cleanup(func() { appRegistry = prevApp })

	appRegistry = nil
	Store("shared", "v1")
	appMap := syncmap.NewAny()
	UseAppRegistry(appMap)
	appMap.Store("shared", "v2") // App 覆盖全局

	if v, _ := Get("shared"); v != "v2" {
		t.Fatalf("App 应覆盖全局: %v", v)
	}

	Delete("shared")
	if _, ok := Get("shared"); ok {
		t.Fatal("Delete 后应同时清掉 Global 和 App")
	}
}
