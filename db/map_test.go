package db

import (
	"testing"

	"touchgocore/syncmap"
)

func TestGetNamedPrefersAppThenGlobal(t *testing.T) {
	prevApp, prevGlobal := _dbApp, _DbMap
	t.Cleanup(func() {
		_dbApp = prevApp
		_DbMap = prevGlobal
	})

	_DbMap = syncmap.NewAny()
	_dbApp = nil
	StoreNamed("global-only", "g")

	appMap := syncmap.NewAny()
	UseRegistry(appMap)
	StoreNamed("app-only", "a")

	if v, ok := GetNamed("app-only"); !ok || v != "a" {
		t.Fatalf("app hit: %v %v", v, ok)
	}
	if v, ok := GetNamed("global-only"); !ok || v != "g" {
		t.Fatalf("global fallback: %v %v", v, ok)
	}
	if Registry() != appMap {
		t.Fatal("Registry should return App map")
	}
}
