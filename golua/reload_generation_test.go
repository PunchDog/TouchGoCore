package lua

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/localtimer"
)

// ==================== reload.go 换代快照（Stop→Start）回归 ====================
//
// 事故形状：watch 协程原先回读 sw.ctx / sw.stopChan 字段。Stop→Start 快速换代会把
// 这两个字段替换为新一代对象，旧协程 select 的却是新通道——既收不到自己那一代的
// 关闭信号（协程泄漏），又可能被新一代的 Stop 误杀（running=true 却无人监控）。
// 修复：Start 在 mu 内取快照并以参数传给 watch；Stop 只 cancel/close 自己捕获的那一代。
// 本机无 C 工具链（-race 不可用），用「协程数收敛 + 换代后监控仍有效」替代观测。

func setupLuaTestInstances(t *testing.T) {
	t.Helper()
	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	t.Cleanup(StopLua)
}

func writeScript(t *testing.T, dir, name string, version int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := "version = " + strconv.Itoa(version) + "\nfunction getv() return version end\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// waitFor 在预算内轮询 cond；超时返回 false。
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestScriptWatcher_StopStartChurn 验证：
//  1. Stop→Start 反复 50 次不 panic、不泄漏 watch 协程；
//  2. 换代结束后监控仍然有效——文件变更在 1~2 个 tick 内触发重载；
//  3. 重载后的脚本实例可用且读到新内容。
func TestScriptWatcher_StopStartChurn(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	script := writeScript(t, dir, "churn.lua", 1)
	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}
	defer ls.Close()

	watcher := NewScriptWatcherWithContext(context.Background(), ls)

	var reloadOK atomic.Int64
	var reloaded atomic.Pointer[LuaScript]
	watcher.AddCallbackWithContext(func(_ context.Context, s *LuaScript, success bool, _ error) {
		if success {
			reloadOK.Add(1)
			reloaded.Store(s)
		}
	})

	goroutinesBefore := runtime.NumGoroutine()
	watcher.Start()
	for i := 0; i < 50; i++ {
		watcher.Stop()
		watcher.Start()
	}

	// 旧的 50+1 代 watch 协程靠「本代 stopCh 已关闭」立即退出；留一点调度余量
	time.Sleep(200 * time.Millisecond)
	if delta := runtime.NumGoroutine() - goroutinesBefore; delta > 2 {
		t.Fatalf("Stop/Start 换代后旧 watch 协程应全部退出，协程数仍多出 %d（疑似回读字段被新一代通道挂住）", delta)
	}

	// 换代结束后监控必须仍然有效：改文件 → 等重载（ticker 1s，给 3.5s 预算）
	if err := os.WriteFile(script, []byte("version = 2\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return reloadOK.Load() >= 1 }, 3500*time.Millisecond) {
		t.Fatal("Stop→Start 反复换代后，文件变更未触发热重载（监控已静默失效）")
	}

	cur := reloaded.Load()
	if cur == nil {
		t.Fatal("回调未带回脚本实例")
	}
	result, err := cur.Call("getv")
	if err != nil {
		t.Fatalf("重载后调用 getv 失败: %v", err)
	}
	if len(result) == 0 || result[0] != int64(2) {
		t.Fatalf("重载后 getv 应返回 2，实际: %v", result)
	}

	watcher.Stop()
	t.Log("✔ Stop→Start 反复换代：无泄漏、监控持续有效")
}

// TestScriptWatcher_StopDuringReloadDoesNotKillNextGeneration 验证：
// Stop 捕获的是自己那一代的 cancel/stopChan——重载进行中 Stop，紧接着 Start，
// 新一代不得被旧 Stop 的后续动作误杀。
func TestScriptWatcher_StopDuringReloadDoesNotKillNextGeneration(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	script := writeScript(t, dir, "stop_reload.lua", 1)
	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}
	defer ls.Close()

	watcher := NewScriptWatcherWithContext(context.Background(), ls)
	var mu sync.Mutex
	var reloadOK atomic.Int64
	var reloaded atomic.Pointer[LuaScript]
	watcher.AddCallbackWithContext(func(_ context.Context, s *LuaScript, success bool, _ error) {
		if success {
			mu.Lock()
			defer mu.Unlock()
			reloadOK.Add(1)
			reloaded.Store(s)
		}
	})

	watcher.Start()

	// Windows 文件时间戳粒度约 15.6ms：Start 与首次写入间隔过小会得到相同 ModTime，
	// checkFileChange 的 After 判定失效——先跨过粒度再改文件
	time.Sleep(100 * time.Millisecond)

	// 触发第一轮重载
	if err := os.WriteFile(script, []byte("version = 2\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return reloadOK.Load() >= 1 }, 3500*time.Millisecond) {
		t.Fatal("第一轮重载未触发")
	}

	// 重载刚完成后立即 Stop→Start，再触发第二轮：新一代必须仍然在监控
	watcher.Stop()
	watcher.Start()
	time.Sleep(1200 * time.Millisecond) // 跨过 Start 时的 tick，确保第二轮走新代
	if err := os.WriteFile(script, []byte("version = 3\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return reloadOK.Load() >= 2 }, 3500*time.Millisecond) {
		t.Fatal("Stop→Start 后新一代未触发第二轮重载（旧 Stop 误杀了新一代）")
	}
	cur := reloaded.Load()
	result, err := cur.Call("getv")
	if err != nil {
		t.Fatalf("第二轮重载后调用失败: %v", err)
	}
	if len(result) == 0 || result[0] != int64(3) {
		t.Fatalf("应读到 version=3，实际: %v", result)
	}

	watcher.Stop()
	t.Log("✔ Stop 只作用于自己捕获的那一代")
}

// ==================== MultiFileWatcher.AddScript 运行期添加 ====================

// TestMultiFileWatcher_AddScriptWhileRunning 验证：
// mfw 已运行时 AddScript，新 watcher 立即启动（不必等下一轮整体 Start），
// 且文件变更能在 1~2 个 tick 内被该新 watcher 检测到并热重载。
func TestMultiFileWatcher_AddScriptWhileRunning(t *testing.T) {
	setupLuaTestInstances(t)

	dir := t.TempDir()
	scriptA := writeScript(t, dir, "a_run.lua", 1)
	lsA, err := NewLuaScriptWithContext(context.Background(), scriptA)
	if err != nil {
		t.Fatalf("创建 A 失败: %v", err)
	}
	defer lsA.Close()

	mfw := NewMultiFileWatcherWithContext(context.Background())
	mfw.AddScript(lsA)
	mfw.Start()
	defer mfw.Stop()

	// 运行期间新脚本加入
	scriptB := writeScript(t, dir, "b_run.lua", 1)
	lsB, err := NewLuaScriptWithContext(context.Background(), scriptB)
	if err != nil {
		t.Fatalf("创建 B 失败: %v", err)
	}
	defer lsB.Close()
	mfw.AddScript(lsB)

	mfw.mu.RLock()
	wB := mfw.watchers[scriptB]
	mfw.mu.RUnlock()
	if wB == nil {
		t.Fatal("B 的 watcher 未注册")
	}

	// 结构性断言：已 running 且 ctx 未被取消
	wB.mu.RLock()
	running := wB.running
	ctxDead := wB.ctx.Err() != nil
	wB.mu.RUnlock()
	if !running {
		t.Fatal("mfw 运行期 AddScript 后，新 watcher 应立即进入 running")
	}
	if ctxDead {
		t.Fatal("新 watcher 的 ctx 不应已取消")
	}

	// 功能性断言：真正监控到文件变更（仅 running=true 但协程没起来会在这里暴露）
	var reloadOKB atomic.Int64
	wB.AddCallbackWithContext(func(_ context.Context, _ *LuaScript, success bool, _ error) {
		if success {
			reloadOKB.Add(1)
		}
	})
	time.Sleep(1200 * time.Millisecond) // 跨过新 watcher Start 时的第一个 tick，避免竞走时序噪声
	if err := os.WriteFile(scriptB, []byte("version = 2\nfunction getv() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return reloadOKB.Load() >= 1 }, 3500*time.Millisecond) {
		t.Fatal("运行期添加的 watcher 未实际监控文件变更（AddScript 只登记未启动）")
	}

	// 兄弟 A 不受影响：仍在运行
	mfw.mu.RLock()
	wA := mfw.watchers[scriptA]
	mfw.mu.RUnlock()
	wA.mu.RLock()
	aRunning := wA.running
	aCtxDead := wA.ctx.Err() != nil
	wA.mu.RUnlock()
	if !aRunning || aCtxDead {
		t.Fatalf("A 的 watcher 应不受影响: running=%v ctxDone=%v", aRunning, aCtxDead)
	}

	t.Log("✔ mfw 运行期 AddScript 立即生效且不影响兄弟 watcher")
}
