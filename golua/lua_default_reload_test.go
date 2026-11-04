package lua

import (
	"context"
	"os"
	"testing"
)

// ==================== 默认实例跟随自动重载换代（缺陷 c）====================
//
// ScriptWatcher.reloadScript 走的是「新建实例」路径：先 closeKeepingObjects 把旧
// 实例的 runtime 拆掉，再 NewLuaScriptWithContext 建新的。defaultLua 此前只在
// RunLua 里赋值，重载全程不碰它，于是重载之后包级门面 lua.Call 一直打到那个
// 空壳上，恒返回「lua runtime is not available (closed or reloading)」——
// 用户侧表现为「热重载一次，Go 调 Lua 就再也调不通了」。

func TestQaGL_DefaultLuaFollowsWatcherReload(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	path := writeScript(t, dir, "d.lua", 1)
	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}

	luaInstancesMu.Lock()
	defaultLua = ls
	luaInstancesMu.Unlock()

	sw := NewScriptWatcherWithContext(context.Background(), ls)

	// 改文件并同步触发一次重载（不经 ticker）
	if err := os.WriteFile(path, []byte("version = 2\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sw.reloadScript(context.Background())

	vals, err := CallWithContext(context.Background(), "getv")
	if err != nil {
		t.Fatalf("✘ 重载后 lua.Call 仍打到已被拆掉的旧实例: %v", err)
	}
	if len(vals) != 1 || vals[0] != int64(2) {
		t.Fatalf("✘ 默认实例没跟着换代，读到 %v", vals)
	}
}

// TestQaGL_DefaultLuaUntouchedWhenNotDefault 「只在归属匹配时替换」这条守门：
// 被监视器托管的实例未必是默认实例，无条件替换会把无关的默认实例顶掉。
func TestQaGL_DefaultLuaUntouchedWhenNotDefault(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	watched, err := NewLuaScriptWithContext(context.Background(), writeScript(t, dir, "watched.lua", 1))
	if err != nil {
		t.Fatalf("创建被监视脚本失败: %v", err)
	}
	decoy, err := NewLuaScriptWithContext(context.Background(), writeScript(t, dir, "decoy.lua", 1))
	if err != nil {
		t.Fatalf("创建默认脚本失败: %v", err)
	}

	luaInstancesMu.Lock()
	defaultLua = decoy
	luaInstancesMu.Unlock()

	sw := NewScriptWatcherWithContext(context.Background(), watched)
	if err := os.WriteFile(watched.initScriptPath, []byte("version = 2\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sw.reloadScript(context.Background())

	luaInstancesMu.RLock()
	after := defaultLua
	luaInstancesMu.RUnlock()
	if after != decoy {
		t.Fatal("✘ 重载无关实例时把默认实例顶掉了")
	}

	// 默认实例仍可用：门面调用照常拿到它的版本
	vals, err := CallWithContext(context.Background(), "getv")
	if err != nil {
		t.Fatalf("✘ 默认实例被误换代后调用失败: %v", err)
	}
	if len(vals) != 1 || vals[0] != int64(1) {
		t.Fatalf("✘ 默认实例读到的不是自己的版本: %v", vals)
	}
}
