package lua

import (
	"context"
	"testing"
)

// QA-GL: luaInstances 登记表泄漏回归。
//
// 旧实现：NewLuaScriptWithContext 每代把实例登记进 luaInstances[instanceID]，
// Close 与热重载换代都不 delete —— 热重载 N 次表长 N+1，旧实例被 map 钉住
// 无法 GC。修复：Close 以及 watcher 重载成功（旧实例被新实例取代）时，
// 经 unregisterLuaInstance 按 UID 归属校验后摘除旧代。

func qaGLCountInstances() int {
	luaInstancesMu.RLock()
	defer luaInstancesMu.RUnlock()
	return len(luaInstances)
}

func TestQaGL_CloseRemovesInstanceFromRegistry(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	path := writeScript(t, dir, "leak.lua", 1)

	base := qaGLCountInstances()
	for i := 0; i < 5; i++ {
		ls, err := NewLuaScriptWithContext(context.Background(), path)
		if err != nil {
			t.Fatalf("NewLuaScriptWithContext: %v", err)
		}
		if got := qaGLCountInstances(); got != base+1 {
			t.Fatalf("创建后 luaInstances=%d, 期望 %d", got, base+1)
		}
		ls.Close()
		// 旧实现不 delete：这里会是 base+i+1（确定性红）
		if got := qaGLCountInstances(); got != base {
			t.Fatalf("Close 后 luaInstances=%d, 期望回到 %d（实例泄漏）", got, base)
		}
	}
}

func TestQaGL_WatcherReloadKeepsRegistryBounded(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	path := writeScript(t, dir, "watch.lua", 1)
	ctx := context.Background()

	ls, err := NewLuaScriptWithContext(ctx, path)
	if err != nil {
		t.Fatalf("NewLuaScriptWithContext: %v", err)
	}
	sw := NewScriptWatcherWithContext(ctx, ls)

	base := qaGLCountInstances() // 含刚创建的 1 个
	for v := 2; v <= 6; v++ {
		writeScript(t, dir, "watch.lua", v)
		sw.reloadScript(ctx) // 换代：新实例登记、旧实例必须摘除
		// 旧实现每重载一次表长 +1（确定性红）
		if got := qaGLCountInstances(); got != base {
			t.Fatalf("第 %d 次重载后 luaInstances=%d, 期望保持 %d（旧代实例泄漏）", v-1, got, base)
		}
	}

	cur := sw.GetScript()
	if cur == nil {
		t.Fatal("GetScript = nil")
	}
	luaInstancesMu.RLock()
	registered, ok := luaInstances[cur.UID]
	luaInstancesMu.RUnlock()
	if !ok || registered != cur {
		t.Fatal("当前活跃实例未登记在 luaInstances 中")
	}

	// 新代脚本确实生效
	vals, err := cur.Call("getv")
	if err != nil {
		t.Fatalf("Call getv: %v", err)
	}
	if len(vals) == 0 || vals[0] != int64(6) {
		t.Fatalf("getv = %v, 期望 [6]（重载后的新版本）", vals)
	}
}
