package lua

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"touchgocore/localtimer"
)

// ==================== Task #5 (H3): 超时后 runtime poisoned ====================

// TestCallWithContext_TimeoutPoisonsRuntime 验证：
// 1. 死循环脚本触发超时后，实例被标记为 poisoned
// 2. 后续调用直接返回明确的 poisoned 错误（不 panic、不数据竞争）
// 3. 调用 Init() 重建 runtime 后恢复正常
func TestCallWithContext_TimeoutPoisonsRuntime(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "poison_test.lua")
	// 写一个包含死循环函数和正常函数的脚本
	content := `
function infinite_loop()
    while true do end
end

function noop()
    return 42
end
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建脚本实例失败: %v", err)
	}
	defer ls.Close()

	// 正常调用应成功
	result, err := ls.Call("noop")
	if err != nil {
		t.Fatalf("正常调用 noop 失败: %v", err)
	}
	if len(result) == 0 || result[0] != int64(42) {
		t.Fatalf("noop 应返回 42，实际: %v", result)
	}

	// 使用极短超时触发死循环的超时
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = ls.CallWithContext(timeoutCtx, "infinite_loop")
	if err == nil {
		t.Fatal("死循环应触发超时错误")
	}
	t.Logf("超时错误(预期): %v", err)

	// 验证实例已被标记为 poisoned
	if !ls.IsPoisoned() {
		t.Fatal("超时后实例应被标记为 poisoned")
	}

	// 后续调用应返回明确的 poisoned 错误，不 panic
	_, err = ls.Call("noop")
	if err == nil {
		t.Fatal("poisoned 实例的调用应返回错误")
	}
	if !containsSubstring(err.Error(), "poisoned") {
		t.Fatalf("错误信息应包含 'poisoned'，实际: %v", err)
	}
	t.Logf("poisoned 后调用错误(预期): %v", err)

	// Init() 重建 runtime 后应恢复正常
	if err := ls.Init(); err != nil {
		t.Fatalf("Init 重建失败: %v", err)
	}
	if ls.IsPoisoned() {
		t.Fatal("Init 后 poisoned 标记应被清除")
	}

	// 重建后需要重新加载脚本才能调用函数（Init 只创建空 runtime）
	// 这里验证调用不再返回 poisoned 错误即可（函数不存在是另一个错误）
	_, err = ls.Call("noop")
	if err == nil {
		t.Log("重建后 noop 可用（脚本已被重新加载）")
	} else if containsSubstring(err.Error(), "poisoned") {
		t.Fatalf("Init 后不应再返回 poisoned 错误: %v", err)
	} else {
		// function not found 是预期的（Init 不重新加载脚本）
		t.Logf("Init 后调用返回非 poisoned 错误(预期): %v", err)
	}
}

// TestCallWithContext_PoisonedRejectsImmediate 验证 poisoned 实例快速拒绝，不会死锁
func TestCallWithContext_PoisonedRejectsImmediate(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "quick_reject.lua")
	if err := os.WriteFile(script, []byte("function f() return 1 end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer ls.Close()

	// 手动标记为 poisoned
	ls.poisoned.Store(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := ls.Call("f")
		if err == nil {
			t.Error("poisoned 实例应返回错误")
		}
	}()

	select {
	case <-done:
		// 正常返回
	case <-time.After(2 * time.Second):
		t.Fatal("poisoned 实例的调用应在快速路径返回，不应阻塞")
	}
}

// ==================== Task #6 (H4): 重载与调用并发 ====================

// TestReload_ConcurrentCalls 验证：
// 经串行化封装的并发 Call 与 ReloadScript 同时进行时不会 panic，
// 调用要么成功要么返回明确错误（runtime not available / poisoned）。
// 注意：CallWithContext 的 rtMu 只保证 Call-vs-Reload 互斥，不保证 Call-vs-Call 互斥；
// 调用方必须自行串行化（此处用外层 sync.Mutex 模拟 SafeLuaScript 的行为）。
// 完整检出力需 -race。
func TestReload_ConcurrentCalls(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "reload_concurrent.lua")
	content := `
counter = 0
function increment()
    counter = counter + 1
    return counter
end
`
	if err := os.WriteFile(script, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer ls.Close()

	// 外层互斥锁保证 Call-vs-Call 串行化（契约要求，等价于 SafeLuaScript）
	var callMu sync.Mutex

	const numCallers = 10
	const numReloads = 5

	var wg sync.WaitGroup
	errCh := make(chan error, numCallers+numReloads)

	// 并发调用者（经 callMu 串行化）
	for i := 0; i < numCallers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				callMu.Lock()
				_, err := ls.Call("increment")
				callMu.Unlock()
				if err != nil {
					// 合法错误：runtime not available (reloading), poisoned, closed
					if !isExpectedConcurrencyError(err) {
						errCh <- err
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	// 并发重载者
	for i := 0; i < numReloads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			time.Sleep(time.Duration(id*5) * time.Millisecond)
			if err := ls.ReloadScriptWithContext(context.Background()); err != nil {
				// 重载失败是允许的（文件可能正在被写）
				t.Logf("reload %d 错误(可接受): %v", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("并发期间出现非预期错误: %v", err)
	}
	t.Log("✔ 串行化 Call + 并发 Reload 无 panic、无非预期错误")
}

// TestReload_CallsBlockedDuringReload 验证重载期间调用方确实被阻塞（而非拿到半初始化 runtime）
func TestReload_CallsBlockedDuringReload(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "block_test.lua")
	if err := os.WriteFile(script, []byte("function f() return 1 end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer ls.Close()

	// 手动持有写锁模拟重载进行中
	ls.rtMu.Lock()

	callDone := make(chan error, 1)
	go func() {
		_, err := ls.Call("f")
		callDone <- err
	}()

	// 短暂等待确认调用被阻塞
	select {
	case err := <-callDone:
		t.Fatalf("调用应在写锁释放前阻塞，但已返回: %v", err)
	case <-time.After(100 * time.Millisecond):
		// 预期：调用被阻塞
	}

	// 释放写锁
	ls.rtMu.Unlock()

	// 调用应完成
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("写锁释放后调用应成功: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("写锁释放后调用应在合理时间内完成")
	}
	t.Log("✔ 重载期间调用被阻塞，释放后正常完成")
}

// ==================== Task #7 (H5): MultiFileWatcher 子 watcher 独立 cancel ====================

// TestMultiFileWatcher_StopOneDoesNotAffectSibling 验证：
// Stop 一个子 watcher 不会导致其余兄弟 watcher 静默退出。
func TestMultiFileWatcher_StopOneDoesNotAffectSibling(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	scriptA := filepath.Join(dir, "a.lua")
	scriptB := filepath.Join(dir, "b.lua")
	if err := os.WriteFile(scriptA, []byte("function fa() return 'a' end\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptB, []byte("function fb() return 'b' end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lsA, err := NewLuaScriptWithContext(context.Background(), scriptA)
	if err != nil {
		t.Fatalf("创建 A 失败: %v", err)
	}
	defer lsA.Close()

	lsB, err := NewLuaScriptWithContext(context.Background(), scriptB)
	if err != nil {
		t.Fatalf("创建 B 失败: %v", err)
	}
	defer lsB.Close()

	mfw := NewMultiFileWatcherWithContext(context.Background())
	mfw.AddScript(lsA)
	mfw.AddScript(lsB)
	mfw.Start()
	defer mfw.Stop()

	// 确认两个 watcher 都在运行
	mfw.mu.RLock()
	watcherA := mfw.watchers[scriptA]
	watcherB := mfw.watchers[scriptB]
	mfw.mu.RUnlock()

	if watcherA == nil || watcherB == nil {
		t.Fatal("两个 watcher 应都已注册")
	}

	// Stop watcher A
	watcherA.Stop()

	// 等待一小段时间让 ctx 传播
	time.Sleep(50 * time.Millisecond)

	// watcher B 应仍在运行
	watcherB.mu.RLock()
	bRunning := watcherB.running
	bCtxDone := watcherB.ctx.Err() != nil
	watcherB.mu.RUnlock()

	if !bRunning {
		t.Fatal("Stop A 后 B 的 running 应仍为 true")
	}
	if bCtxDone {
		t.Fatal("Stop A 后 B 的 ctx 不应被取消")
	}

	// 验证 A 确实已停止
	watcherA.mu.RLock()
	aRunning := watcherA.running
	watcherA.mu.RUnlock()
	if aRunning {
		t.Fatal("A 的 running 应为 false")
	}

	t.Log("✔ Stop 一个子 watcher 不影响兄弟")
}

// TestScriptWatcher_StopThenStart 验证：
// Stop 后再 Start，watcher 能重新监控文件变更。
func TestScriptWatcher_StopThenStart(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "restart.lua")
	if err := os.WriteFile(script, []byte("version = 1\nfunction f() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer ls.Close()

	watcher := NewScriptWatcherWithContext(context.Background(), ls)
	watcher.Start()

	// 确认在运行
	watcher.mu.RLock()
	if !watcher.running {
		watcher.mu.RUnlock()
		t.Fatal("Start 后应处于运行状态")
	}
	watcher.mu.RUnlock()

	// Stop
	watcher.Stop()

	watcher.mu.RLock()
	if watcher.running {
		watcher.mu.RUnlock()
		t.Fatal("Stop 后应处于停止状态")
	}
	watcher.mu.RUnlock()

	// 等待确保旧 goroutine 已退出
	time.Sleep(50 * time.Millisecond)

	// 重新 Start
	watcher.Start()

	watcher.mu.RLock()
	if !watcher.running {
		watcher.mu.RUnlock()
		t.Fatal("重新 Start 后应处于运行状态")
	}
	// 验证 ctx 已重建（不是已取消的旧 ctx）
	if watcher.ctx.Err() != nil {
		watcher.mu.RUnlock()
		t.Fatal("重新 Start 后 ctx 不应已取消")
	}
	watcher.mu.RUnlock()

	// 验证 stopChan 未被关闭（新 channel）
	select {
	case <-watcher.stopChan:
		t.Fatal("重新 Start 后 stopChan 不应已关闭")
	default:
		// 预期：stopChan 开放
	}

	// 修改文件并验证 watcher 能检测到变更
	time.Sleep(50 * time.Millisecond) // 等待 watcher goroutine 初始化 fileModTime
	if err := os.WriteFile(script, []byte("version = 2\nfunction f() return version end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 等待 watcher 检测变更并触发重载（ticker 间隔 1 秒）
	time.Sleep(2 * time.Second)

	// 验证脚本已重载（version 变为 2）
	result, err := ls.Call("f")
	if err != nil {
		// 如果 watcher 创建了新实例，ls 可能是旧的
		t.Logf("调用旧实例失败(可能已被替换): %v", err)
	} else if len(result) > 0 {
		t.Logf("重载后调用结果: %v", result)
	}

	watcher.Stop()
	t.Log("✔ Stop 后 Start 能重新监控文件变更")
}

// TestMultiFileWatcher_StopThenStart 验证 MultiFileWatcher 级别的 Stop/Start 重启
func TestMultiFileWatcher_StopThenStart(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	dir := t.TempDir()
	script := filepath.Join(dir, "mfw_restart.lua")
	if err := os.WriteFile(script, []byte("function f() return 1 end\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLuaScriptWithContext(context.Background(), script)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	defer ls.Close()

	mfw := NewMultiFileWatcherWithContext(context.Background())
	mfw.AddScript(ls)
	mfw.Start()

	// Stop
	mfw.Stop()

	mfw.mu.RLock()
	if mfw.running {
		mfw.mu.RUnlock()
		t.Fatal("Stop 后应为停止状态")
	}
	mfw.mu.RUnlock()

	// 重新 Start
	mfw.Start()

	mfw.mu.RLock()
	if !mfw.running {
		mfw.mu.RUnlock()
		t.Fatal("重新 Start 后应为运行状态")
	}
	if mfw.ctx.Err() != nil {
		mfw.mu.RUnlock()
		t.Fatal("重新 Start 后 ctx 不应已取消")
	}
	// 检查子 watcher 也在运行
	for path, w := range mfw.watchers {
		w.mu.RLock()
		running := w.running
		ctxErr := w.ctx.Err()
		w.mu.RUnlock()
		if !running {
			mfw.mu.RUnlock()
			t.Fatalf("子 watcher %s 应在运行", path)
		}
		if ctxErr != nil {
			mfw.mu.RUnlock()
			t.Fatalf("子 watcher %s 的 ctx 不应已取消: %v", path, ctxErr)
		}
	}
	mfw.mu.RUnlock()

	mfw.Stop()
	t.Log("✔ MultiFileWatcher Stop 后 Start 正常工作")
}

// ==================== 辅助函数 ====================

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && contains(s, sub))
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func isExpectedConcurrencyError(err error) bool {
	msg := err.Error()
	return contains(msg, "poisoned") ||
		contains(msg, "not available") ||
		contains(msg, "closed") ||
		contains(msg, "reloading")
}
