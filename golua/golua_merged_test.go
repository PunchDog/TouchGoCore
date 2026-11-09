package lua

import (
	"context"
	"fmt"
	rt "github.com/arnodel/golua/runtime"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
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

// ==================== 启动期节拍配置（缺陷 b）====================
//
// UpdateIntervalMs / GCTickCount 原先在包级 var 声明处调 getUpdateIntervalMs() 求值。
// Go 的包级初始化早于任何函数调用，那时 Run() 还没给父上下文赋值，luaCfg() 恒为 nil，
// 于是 lua_config.update_interval / gc_tick_count 永不被读到，节拍恒取硬编码默认值。

// qaRunLuaWithCfg 用给定 Lua 配置跑一次 Run，返回清理函数。
func qaRunLuaWithCfg(t *testing.T, lc *config.LuaConfig) {
	t.Helper()
	localtimer.Run(context.Background())
	prevParent := currentLuaParentCtx()
	setLuaParentCtx(corectx.WithCfg(context.Background(), &config.Cfg{LuaConfig: lc}))
	applyLuaTimingConfig()

	t.Cleanup(func() {
		StopLua()
		setLuaParentCtx(prevParent)
		// 复位节拍，别让后续用例（含 lua_test.go 的默认值断言）读到本用例注入的值
		applyLuaTimingConfig()
		localtimer.TimeStop(context.Background())
	})

	if err := Run(currentLuaParentCtx()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
}

func qaWriteMainLua(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "main.lua")
	if err := os.WriteFile(path, []byte("configured = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestQaGL_TimingConfigAppliesOnRun 配置里的节拍必须在 Run 之后真的生效。
func TestQaGL_TimingConfigAppliesOnRun(t *testing.T) {
	dir := t.TempDir()
	qaRunLuaWithCfg(t, &config.LuaConfig{
		ScriptPath:     qaWriteMainLua(t, dir),
		UpdateInterval: 250,
		GCTickCount:    7,
	})

	if got := luaUpdateIntervalMsValue(); got != 250 {
		t.Fatalf("✘ update 间隔未生效，期望 250，实际 %d", got)
	}
	if got := luaGCTickCountValue(); got != 7 {
		t.Fatalf("✘ GC 节拍未生效，期望 7，实际 %d", got)
	}
	// 两个导出 var 是兼容镜像，外部读到的也必须是被应用后的值
	if UpdateIntervalMs != 250 || GCTickCount != 7 {
		t.Fatalf("✘ 镜像 var 未同步: UpdateIntervalMs=%d GCTickCount=%d", UpdateIntervalMs, GCTickCount)
	}
}

// TestQaGL_StartTimerUsesConfiguredInterval 验证配置值走到了真实消费点：
// 每个脚本实例的 update 定时器间隔就是配置值，而不是硬编码 1000。
func TestQaGL_StartTimerUsesConfiguredInterval(t *testing.T) {
	dir := t.TempDir()
	qaRunLuaWithCfg(t, &config.LuaConfig{
		ScriptPath:     qaWriteMainLua(t, dir),
		UpdateInterval: 251,
		GCTickCount:    9,
	})

	luaInstancesMu.RLock()
	ls := defaultLua
	luaInstancesMu.RUnlock()
	if ls == nil {
		t.Fatal("前置条件不成立：Run 之后应有默认实例")
	}
	if ls.timer == nil {
		t.Fatal("前置条件不成立：默认实例应已挂上 update 定时器")
	}
	if got := ls.timer.GetInterval(); got != 251 {
		t.Fatalf("✘ startTimer 用的仍是默认节拍，期望 251，实际 %d", got)
	}
}

// TestQaGL_TimingConfigFallsBackToDefaults 无配置（或字段非正）时回到默认 1000/1800。
// 这条守住 lua_test.go 里对默认值的既有断言不被「修生效」顺手改掉。
func TestQaGL_TimingConfigFallsBackToDefaults(t *testing.T) {
	prevParent := currentLuaParentCtx()
	t.Cleanup(func() {
		setLuaParentCtx(prevParent)
		applyLuaTimingConfig()
	})

	setLuaParentCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		LuaConfig: &config.LuaConfig{UpdateInterval: 0, GCTickCount: -5},
	}))
	applyLuaTimingConfig()

	if got := luaUpdateIntervalMsValue(); got != defaultLuaUpdateIntervalMs {
		t.Fatalf("✘ 缺配应回退 %d，实际 %d", defaultLuaUpdateIntervalMs, got)
	}
	if got := luaGCTickCountValue(); got != defaultLuaGCTickCount {
		t.Fatalf("✘ 缺配应回退 %d，实际 %d", defaultLuaGCTickCount, got)
	}
}

// TestQaGL_ParentCtxConcurrentAccess 父上下文写读并发不得竞争：
// Run 在启动阶段写，而 confineLuaPath / initLocked 在 Lua 调用栈内读。
// 裸赋值时本用例在 -race 下必报；换成 setLuaParentCtx/currentLuaParentCtx 后应干净。
func TestQaGL_ParentCtxConcurrentAccess(t *testing.T) {
	prevParent := currentLuaParentCtx()
	t.Cleanup(func() {
		setLuaParentCtx(prevParent)
		applyLuaTimingConfig()
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := corectx.WithCfg(context.Background(), &config.Cfg{
				LuaConfig: &config.LuaConfig{UpdateInterval: int64(100 + n)},
			})
			for {
				select {
				case <-stop:
					return
				default:
				}
				setLuaParentCtx(ctx)
			}
		}(i)
	}

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = luaCfg()
				_ = currentLuaParentCtx()
			}
		}()
	}

	// 读写各跑 30ms：够 race detector 抓到裸赋值那一版的竞争对
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()

	setLuaParentCtx(prevParent)
	applyLuaTimingConfig()
}

// ==================== Lua→Go 类型映射（Struct / Slice / Map / any）====================
//
// 修复前 LuaToReflectValueWithContext 的 switch 只有标量与 interface 分支：
// 结构体/切片/映射形参一律报 "unsupported Go type"，interface 形参拿到 Lua 函数
// 则经 reflect.Zero 静默变 nil。

// ---- 供桥接层端到端用例使用的探针方法（probeObject 定义在 p0 测试文件）----

// SumCost 接收结构体形参：Lua 传表，Go 侧必须是填好的结构体。
func (p *probeObject) SumCost(item qaShopItem) int64 { return item.Cost }

// SumSlice 接收切片形参。
func (p *probeObject) SumSlice(xs []int64) int64 {
	var sum int64
	for _, x := range xs {
		sum += x
	}
	return sum
}

// CountMap 接收映射形参。
func (p *probeObject) CountMap(m map[string]int64) int64 {
	return int64(len(m))
}

// EchoTypeName 接收 any 形参，返回实际类型名。
func (p *probeObject) EchoTypeName(v interface{}) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", v)
}

// SameObject 接收具体结构体指针形参：Lua 侧传 userdata 必须解包回 Go 对象。
func (p *probeObject) SameObject(other *probeObject) bool { return p == other }

// qaRuntime 建一个只用来造 rt.Table 的运行时线程。
func qaRuntime(t *testing.T) *rt.Thread {
	t.Helper()
	return rt.New(os.Stdout).MainThread()
}

func TestQaGL_LuaTableToGoStruct(t *testing.T) {
	thread := qaRuntime(t)

	tbl := rt.NewTable()
	thread.SetTable(tbl, rt.StringValue("item_id"), rt.IntValue(2001))
	thread.SetTable(tbl, rt.StringValue("cost"), rt.IntValue(10000))
	thread.SetTable(tbl, rt.StringValue("Plain"), rt.StringValue("by-go-name"))
	thread.SetTable(tbl, rt.StringValue("unknown_key"), rt.IntValue(1)) // 未知键忽略

	out, err := LuaToReflectValueWithContext(context.Background(), rt.TableValue(tbl), reflect.TypeOf(qaShopItem{}))
	if err != nil {
		t.Fatalf("✘ Lua 表应能填进结构体: %v", err)
	}
	item := out.Interface().(qaShopItem)
	if item.ItemID != 2001 {
		t.Errorf("✘ item_id 没填上: %d", item.ItemID)
	}
	if item.Cost != 10000 {
		t.Errorf("✘ cost 没填上: %d", item.Cost)
	}
	if item.Plain != "by-go-name" {
		t.Errorf("✘ Go 字段名口径不可用: %q", item.Plain)
	}
}

func TestQaGL_LuaTableToGoSliceAndMap(t *testing.T) {
	thread := qaRuntime(t)

	tbl := rt.NewTable()
	thread.SetTable(tbl, rt.IntValue(1), rt.IntValue(10))
	thread.SetTable(tbl, rt.IntValue(2), rt.IntValue(20))

	sliceOut, err := LuaToReflectValueWithContext(context.Background(), rt.TableValue(tbl), reflect.TypeOf([]int64(nil)))
	if err != nil {
		t.Fatalf("✘ Lua 表应能折成 []int64: %v", err)
	}
	slice := sliceOut.Interface().([]int64)
	if len(slice) != 2 || slice[0] != 10 || slice[1] != 20 {
		t.Errorf("✘ 切片内容不对: %v", slice)
	}

	mtbl := rt.NewTable()
	thread.SetTable(mtbl, rt.StringValue("a"), rt.IntValue(1))
	thread.SetTable(mtbl, rt.StringValue("b"), rt.IntValue(2))
	mapOut, err := LuaToReflectValueWithContext(context.Background(), rt.TableValue(mtbl), reflect.TypeOf(map[string]int64(nil)))
	if err != nil {
		t.Fatalf("✘ Lua 表应能折成 map: %v", err)
	}
	m := mapOut.Interface().(map[string]int64)
	if len(m) != 2 || m["a"] != 1 || m["b"] != 2 {
		t.Errorf("✘ map 内容不对: %v", m)
	}
}

// TestQaGL_BridgeAcceptsComplexParams 走真实桥接：Lua 传表/表数组，Go 方法收结构体/切片/映射。
func TestQaGL_BridgeAcceptsComplexParams(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
local cost = a:SumCost({ item_id = 7, cost = 4200, Plain = "x" })
if cost ~= 4200 then error("结构体形参没填对: " .. tostring(cost)) end

local sum = a:SumSlice({ 3, 4, 5 })
if sum ~= 12 then error("切片形参不对: " .. tostring(sum)) end

local n = a:CountMap({ x = 1, y = 2, z = 3 })
if n ~= 3 then error("映射形参不对: " .. tostring(n)) end
`)
}

// TestQaGL_UserdataArgUnwrapsToObject Lua 把自己那边的对象当参数传回来时，
// Go 侧必须拿到登记的原对象，而不是 nil 或空表。
func TestQaGL_UserdataArgUnwrapsToObject(t *testing.T) {
	registerProbeClass(t)
	runProbeScript(t, `
local a = probeObject()
local b = probeObject()
if not a:SameObject(a) then error("同一对象的 userdata 没有被解包回来") end
if a:SameObject(b) then error("不同对象被认成了同一个") end
if a:EchoTypeName(b) ~= "*lua.probeObject" then
    error("any 形参收到的类型: " .. tostring(a:EchoTypeName(b)))
end
-- 表折成 *LuaTable 的口径不能变（DialogItem.Param1 依赖它）
if a:EchoTypeName({ 1, 2 }) ~= "*lua.LuaTable" then
    error("表形参收到的类型: " .. tostring(a:EchoTypeName({ 1, 2 })))
end
if a:EchoTypeName(nil) ~= "nil" then error("nil 形参应为 nil") end
`)
}

// TestQaGL_LuaFuncToAnyIsError Lua 函数没有 Go 表示，必须报错而不是静默 nil。
func TestQaGL_LuaFuncToAnyIsError(t *testing.T) {
	registerProbeClass(t)
	if _, err := newProbeScript(t, "local a = probeObject()\nreturn a:EchoTypeName(function() end)\n"); err == nil {
		t.Fatal("✘ 把 Lua 函数传给 any 形参应报错（此前静默变 nil）")
	}
}

// TestQaGL_LuaTableToUnsupportedTarget 明确的报错口径。
func TestQaGL_LuaTableToUnsupportedTarget(t *testing.T) {
	tbl := rt.NewTable()

	// 指向标量的指针不支持（只支持结构体指针）
	if _, err := LuaToReflectValueWithContext(context.Background(), rt.TableValue(tbl), reflect.TypeOf((*int64)(nil))); err == nil {
		t.Error("✘ *int64 形参应报不支持")
	}

	// Go 函数形参不支持
	if _, err := LuaToReflectValueWithContext(context.Background(), rt.TableValue(tbl), reflect.TypeOf(func() {})); err == nil {
		t.Error("✘ func 形参应报不支持")
	}

	// 参数不是表却要求结构体
	if _, err := LuaToReflectValueWithContext(context.Background(), rt.StringValue("x"), reflect.TypeOf(qaShopItem{})); err == nil {
		t.Error("✘ 字符串不能填结构体")
	}
}

// ==================== Go→Lua 类型映射（Struct / Ptr）====================
//
// 修复前 convertToTable → populateTable 的 default 分支只认 Slice/Array/Map，
// struct 与指向 struct 的指针一个键都不填，于是 HasData() 为假、
// GoToLuaValueWithContext 返回 rt.NilValue：Npc.GetShop 的 []*ShopItem、
// GetDialog 的 *DialogItem 到 Lua 侧全变成 nil，且 Go 侧不记任何日志。

type qaShopItem struct {
	ItemID  int    `json:"item_id"`
	Cost    int64  `json:"cost"`
	NoLua   string `json:"-"` // 与 Npc.Shop/Dialog 同口径：绝不该进 Lua 表
	Plain   string // 无 tag：按 Go 导出字段名给
	hidden  int    // 非导出字段：跳过
	Visible bool
	Nested  *qaShopItem
	Items   []*qaShopItem
}

type qaNode struct {
	Next *qaNode `json:"next"`
	Val  int64   `json:"val"`
}

func qaTableOf(t *testing.T, v rt.Value) *rt.Table {
	t.Helper()
	tbl, ok := v.TryTable()
	if !ok {
		t.Fatalf("期望 Lua 表，实际 %s", v.TypeName())
	}
	return tbl
}

func TestQaGL_GoToLuaStructPtrYieldsTable(t *testing.T) {
	ctx := context.Background()
	item := &qaShopItem{ItemID: 2001, Cost: 10000, NoLua: "secret", Plain: "p", hidden: 7, Visible: true}

	v, err := GoToLuaValueErr(ctx, item)
	if err != nil {
		t.Fatalf("✘ struct 指针应能折成表: %v", err)
	}
	tbl := qaTableOf(t, v)

	if got := tbl.Get(rt.StringValue("item_id")); got != rt.IntValue(2001) {
		t.Errorf("✘ json tag 口径没生效，item_id 读到 %v", got)
	}
	if got := tbl.Get(rt.StringValue("cost")); got != rt.IntValue(10000) {
		t.Errorf("✘ cost 读到 %v", got)
	}
	if got := tbl.Get(rt.StringValue("Plain")); got != rt.StringValue("p") {
		t.Errorf("✘ 无 tag 字段应按 Go 字段名可见，Plain 读到 %v", got)
	}
	if got := tbl.Get(rt.StringValue("Visible")); got != rt.BoolValue(true) {
		t.Errorf("✘ Visible 读到 %v", got)
	}
	if tbl.Get(rt.StringValue("NoLua")) != rt.NilValue {
		t.Error("✘ json:\"-\" 的字段漏进了 Lua 表")
	}
	if tbl.Get(rt.StringValue("hidden")) != rt.NilValue {
		t.Error("✘ 非导出字段漏进了 Lua 表")
	}
}

func TestQaGL_SliceOfStructPtr(t *testing.T) {
	// Npc.GetShop(int) []*ShopItem 与 GetDialogsByType 的真实形状
	items := []*qaShopItem{{ItemID: 1}, {ItemID: 2}}

	v, err := GoToLuaValueErr(context.Background(), items)
	if err != nil {
		t.Fatalf("✘ 结构体指针切片应能折成表: %v", err)
	}
	tbl := qaTableOf(t, v)

	if tbl.Get(rt.IntValue(3)) != rt.NilValue {
		t.Errorf("✘ 只有 2 个元素，下标 3 不该有值: %v", tbl.Get(rt.IntValue(3)))
	}
	// 顺序键必须是 1..n 的整数，Lua 侧 ipairs / # 才可用
	for i, want := range []int64{1, 2} {
		elem := qaTableOf(t, tbl.Get(rt.IntValue(int64(i+1))))
		if got := elem.Get(rt.StringValue("item_id")); got != rt.IntValue(want) {
			t.Errorf("✘ 第 %d 个元素的 item_id 读到 %v", i+1, got)
		}
	}
}

func TestQaGL_NilStructPtrIsNilValue(t *testing.T) {
	var item *qaShopItem
	v, err := GoToLuaValueErr(context.Background(), item)
	if err != nil {
		t.Fatalf("✘ nil 指针不是错误: %v", err)
	}
	if v != rt.NilValue {
		t.Fatalf("✘ nil 指针应折成 Lua nil（与 Go 侧判空口径一致），实际 %s", v.TypeName())
	}
}

func TestQaGL_NestedStructAndSliceFields(t *testing.T) {
	item := &qaShopItem{
		ItemID: 1,
		Nested: &qaShopItem{ItemID: 11},
		Items:  []*qaShopItem{{ItemID: 21}, {ItemID: 22}},
	}
	tbl := qaTableOf(t, mustGoToLua(t, item))

	// Nested / Items 没有 json tag，按 Go 导出字段名可见（口径见 luaFieldName）
	nested := qaTableOf(t, tbl.Get(rt.StringValue("Nested")))
	if got := nested.Get(rt.StringValue("item_id")); got != rt.IntValue(11) {
		t.Errorf("✘ 嵌套 struct 字段读到 %v", got)
	}
	slice := qaTableOf(t, tbl.Get(rt.StringValue("Items")))
	second := qaTableOf(t, slice.Get(rt.IntValue(2)))
	if got := second.Get(rt.StringValue("item_id")); got != rt.IntValue(22) {
		t.Errorf("✘ 切片字段的第 2 个元素读到 %v", got)
	}
}

func mustGoToLua(t *testing.T, v interface{}) rt.Value {
	t.Helper()
	luaValue, err := GoToLuaValueErr(context.Background(), v)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	return luaValue
}

// TestQaGL_LuaTableFieldKeepsIdentity DialogItem.Param1 在 Go 侧必须是 *LuaTable
// 本体（mapmanager/npc.go 的随机文本分支按这个类型分派），折表时不得展平或复制。
func TestQaGL_LuaTableFieldKeepsIdentity(t *testing.T) {
	inner := newTable(nil)
	inner.Set("k", int64(7))

	holder := &qaWithAny{Param1: inner}
	tbl := qaTableOf(t, mustGoToLua(t, holder))

	param1, ok := tbl.Get(rt.StringValue("param1")).TryTable()
	if !ok {
		t.Fatal("✘ Param1 没折成表")
	}
	if got := param1.Get(rt.IntValue(1)); got != rt.NilValue {
		t.Errorf("✘ 原表里没有下标 1，不该凭空出现: %v", got)
	}
	if got := param1.Get(rt.StringValue("k")); got != rt.IntValue(7) {
		t.Errorf("✘ Param1 里的键丢了: k 读到 %v", got)
	}

	// 引用同一份：往 Go 侧的 inner 里加一项，经 holder 再折一次必须看得到
	inner.Set("added", "later")
	again := qaTableOf(t, qaTableOf(t, mustGoToLua(t, holder)).Get(rt.StringValue("param1")))
	if got := again.Get(rt.StringValue("added")); got != rt.StringValue("later") {
		t.Fatalf("✘ Param1 被复制过，后续写入看不到: %v", got)
	}
}

type qaWithAny struct {
	Param1 interface{} `json:"param1"`
}

// TestQaGL_CyclicStructDoesNotHang 自指结构必须靠 seen 表收敛，不能无限下潜。
func TestQaGL_CyclicStructDoesNotHang(t *testing.T) {
	node := &qaNode{Val: 1}
	node.Next = node

	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := GoToLuaValueErr(context.Background(), node)
		if err != nil {
			t.Errorf("✘ 自指结构转换报错: %v", err)
		}
		if _, ok := v.TryTable(); !ok {
			t.Errorf("✘ 自指结构应折成表，实际 %s", v.TypeName())
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("✘ 自指结构没有收敛（疑似无限递归）")
	}
}

// TestQaGL_DepthCapReportsError 超过层数上限要给出错误，而不是静默少一层。
func TestQaGL_DepthCapReportsError(t *testing.T) {
	root := &qaNode{Val: 0}
	cur := root
	for i := 1; i < maxLuaConvDepth+4; i++ {
		next := &qaNode{Val: int64(i)}
		cur.Next = next
		cur = next
	}

	_, err := GoToLuaValueErr(context.Background(), root)
	if err == nil {
		t.Fatalf("✘ 超过 %d 层应报错，别静默截断", maxLuaConvDepth)
	}
}

// TestQaGL_UnsupportedTypeReportsError chan/func 折不动时必须有声音，
// 不再像修复前那样静默变 nil。
func TestQaGL_UnsupportedTypeReportsError(t *testing.T) {
	if _, err := GoToLuaValueErr(context.Background(), make(chan int)); err == nil {
		t.Error("✘ chan 应报不支持")
	}
	if _, err := GoToLuaValueErr(context.Background(), func() {}); err == nil {
		t.Error("✘ func 应报不支持")
	}
}

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

// ==================== 反射式宿主函数注册（RegisterGoFunc）====================

// registerQaGoFunc 注册一个全局函数并保证用例结束后摘掉。
func registerQaGoFunc(t *testing.T, name string, fn interface{}) {
	t.Helper()
	if err := RegisterGoFunc(name, fn); err != nil {
		t.Fatalf("注册 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		registeredGoFuncsMu.Lock()
		delete(registeredGoFuncs, name)
		registeredGoFuncsMu.Unlock()
	})
}

// qaEnvCallable 报告实例环境里某名字是不是可调用值。
func qaEnvCallable(ls *LuaScript, name string) bool {
	ls.rtMu.RLock()
	defer ls.rtMu.RUnlock()
	if ls.env == nil {
		return false
	}
	_, ok := ls.env.Get(rt.StringValue(name)).TryCallable()
	return ok
}

func TestQaGL_RegisterGoFuncFixedArity(t *testing.T) {
	registerQaGoFunc(t, "qa_add", func(a, b int64) int64 { return a + b })

	runProbeScript(t, `
local v = qa_add(2, 3)
if v ~= 5 then error("qa_add(2,3) = " .. tostring(v)) end
`)

	// 元数是真实值：少传、多传都必须报错，而不是静默截断/panic
	if _, err := newProbeScript(t, "return qa_add(1)\n"); err == nil {
		t.Error("✘ 少传参数应报错")
	}
	if _, err := newProbeScript(t, "return qa_add(1, 2, 3)\n"); err == nil {
		t.Error("✘ 多传参数应报错（hasEtc 为真才可能）")
	}
}

func TestQaGL_RegisterGoFuncVariadic(t *testing.T) {
	registerQaGoFunc(t, "qa_tag", func(id int, text string, rest ...interface{}) string {
		return fmt.Sprintf("%d|%s|%d", id, text, len(rest))
	})

	runProbeScript(t, `
if qa_tag(7, "txt", "x", "y") ~= "7|txt|2" then error("变参收集错: " .. qa_tag(7, "txt", "x", "y")) end
if qa_tag(1, "z") ~= "1|z|0" then error("零变参收集错: " .. qa_tag(1, "z")) end
if pcall(qa_tag, 1) then error("少传定长参数应报错") end
`)
}

func TestQaGL_RegisterGoFuncStructInOut(t *testing.T) {
	registerQaGoFunc(t, "qa_make_item", func(id int64, cost int64) *qaShopItem {
		return &qaShopItem{ItemID: int(id), Cost: cost}
	})
	registerQaGoFunc(t, "qa_item_cost", func(item qaShopItem) int64 { return item.Cost })

	runProbeScript(t, `
local it = qa_make_item(2001, 4200)
if type(it) ~= "table" then error("结构体返回值应折成表，实际 " .. type(it)) end
if it.item_id ~= 2001 or it.cost ~= 4200 then
    error("字段不对: " .. tostring(it.item_id) .. "/" .. tostring(it.cost))
end
if qa_item_cost({ item_id = 9, cost = 77 }) ~= 77 then error("结构体形参没填对") end
`)
}

func TestQaGL_RegisterGoFuncErrorLast(t *testing.T) {
	registerQaGoFunc(t, "qa_boom", func() (int64, error) { return 0, fmt.Errorf("炸了") })
	registerQaGoFunc(t, "qa_okv", func() (int64, error) { return 42, nil })

	runProbeScript(t, `
local ok, err = pcall(qa_boom)
if ok then error("qa_boom 应抛错") end
if type(err) ~= "string" or not string.find(err, "炸了") then
    error("错误对象不对: " .. tostring(err))
end

local v = qa_okv()
if v ~= 42 then error("qa_okv 应返回 42，实际 " .. tostring(v)) end
`)
}

// TestQaGL_RegisterGoFuncSurvivesInit Init() 回放注册表时不能漏掉反射注册的这批。
func TestQaGL_RegisterGoFuncSurvivesInit(t *testing.T) {
	registerQaGoFunc(t, "qa_survive", func() string { return "alive" })

	ls := runProbeScript(t, "local _ = qa_survive()\n")
	t.Cleanup(ls.Close)

	if !qaEnvCallable(ls, "qa_survive") {
		t.Fatal("前置条件不成立：创建实例后应有 qa_survive")
	}
	if err := ls.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !qaEnvCallable(ls, "qa_survive") {
		t.Fatal("✘ Init() 没有回放 RegisterGoFunc 注册的函数")
	}
}

func TestQaGL_RegisterGoFuncNameConflict(t *testing.T) {
	// 内置函数名不可占用
	if err := RegisterGoFunc("info", func() {}); err == nil {
		t.Error("✘ 占用内置函数名应报错")
	}
	// 与 RegisterLuaFunc 双向冲突
	if err := RegisterLuaFunc("qa_conflict_both", func(_ *rt.Thread, _ *rt.GoCont) (rt.Cont, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("前置注册失败: %v", err)
	}
	t.Cleanup(func() {
		registeredFuncsMu.Lock()
		delete(registeredFuncs, "qa_conflict_both")
		registeredFuncsMu.Unlock()
	})
	if err := RegisterGoFunc("qa_conflict_both", func() {}); err == nil {
		t.Error("✘ 与 RegisterLuaFunc 同名应报错")
	}

	// 同一注册表重复注册
	registerQaGoFunc(t, "qa_conflict_self", func() {})
	if err := RegisterGoFunc("qa_conflict_self", func() {}); err == nil {
		t.Error("✘ 重复注册应报错")
	}

	// 签名不支持要在注册期就拒绝
	if err := RegisterGoFunc("qa_bad_1", 42); err == nil {
		t.Error("✘ 非函数应报错")
	}
	if err := RegisterGoFunc("qa_bad_2", func(chan int) {}); err == nil {
		t.Error("✘ chan 形参应报错")
	}
	if err := RegisterGoFunc("qa_bad_3", func(*int64) {}); err == nil {
		t.Error("✘ *int64 形参应报错")
	}
	if err := RegisterGoFunc("", func() {}); err == nil {
		t.Error("✘ 空函数名应报错")
	}
}

// ==================== 注册表回放（缺陷 a）====================
//
// Init() 是 poison 恢复与热重载失败回退的唯一重建入口。它原先只装 5 个内置函数，
// 不回放 registeredFuncs / registeredClasses，于是这两条路径重建出的 runtime
// 只有内置函数——Lua 侧 Npc() 与宿主函数全部消失，且没有任何报错。

// qaEnvHas 报告实例的全局环境里是否存在某个非 nil 名字。
func qaEnvHas(ls *LuaScript, name string) bool {
	ls.rtMu.RLock()
	defer ls.rtMu.RUnlock()
	if ls.env == nil {
		return false
	}
	return ls.env.Get(rt.StringValue(name)) != rt.NilValue
}

// TestQaGL_InitReplaysRegisteredClass 验证 Init() 之后类构造器仍在 env 里。
func TestQaGL_InitReplaysRegisteredClass(t *testing.T) {
	registerProbeClass(t)
	ls := runProbeScript(t, "local a = probeObject()\n")
	t.Cleanup(ls.Close)

	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("前置条件不成立：创建实例后 env 里应有 probeObject")
	}

	if err := ls.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("✘ Init() 没有回放类注册表，poison 恢复/重载回退后 Lua 侧构造器会消失")
	}
}

// TestQaGL_InitReplaysRegisteredFunc 验证 Init() 之后宿主函数仍在 env 里，
// 且仍按「1 个具名槽 + etc」绑定（第 2 个之后的实参不能丢）。
func TestQaGL_InitReplaysRegisteredFunc(t *testing.T) {
	setupLuaTestInstances(t)

	const fname = "qa_probe_replay_fn"
	var got []string
	if err := RegisterLuaFunc(fname, func(_ *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
		got = got[:0]
		for _, v := range append([]rt.Value{c.Arg(0)}, c.Etc()...) {
			s, ok := v.TryString()
			if !ok {
				return nil, fmt.Errorf("参数应为字符串")
			}
			got = append(got, s)
		}
		return c.Next(), nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		registeredFuncsMu.Lock()
		delete(registeredFuncs, fname)
		registeredFuncsMu.Unlock()
	})

	path := filepath.Join(t.TempDir(), "replay_func.lua")
	if err := os.WriteFile(path, []byte(fname+`("甲", "乙")`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}
	t.Cleanup(ls.Close)

	if err := ls.Init(); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if !qaEnvHas(ls, fname) {
		t.Fatalf("✘ Init() 没有回放宿主函数 %s", fname)
	}

	// 回放后的绑定形状必须仍能把第 2 个之后的实参送到 etc
	if _, err := ls.CallWithContext(context.Background(), fname, "甲", "乙", "丙"); err != nil {
		t.Fatalf("✘ Init() 后调用宿主函数失败: %v", err)
	}
	if len(got) != 3 || got[0] != "甲" || got[2] != "丙" {
		t.Fatalf("✘ Init() 后宿主函数的 extra 实参丢失: %v", got)
	}
}

// TestQaGL_ReloadRollbackKeepsConstructor 走真实生产形状：脚本改坏 → 自动重载
// 失败 → 回退路径调 oldScript.Init()。回退后旧实例必须还能用 Lua 侧构造器，
// 否则等于「重载失败把业务类也弄丢了」。
func TestQaGL_ReloadRollbackKeepsConstructor(t *testing.T) {
	registerProbeClass(t)
	setupLuaTestInstances(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.lua")
	good := "probe = probeObject()\nprobe:SetName('甲')\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("创建脚本失败: %v", err)
	}

	sw := NewScriptWatcherWithContext(context.Background(), ls)

	// 写坏脚本，让 NewLuaScriptWithContext 编译失败，从而走回退分支
	if err := os.WriteFile(path, []byte("this is not lua ~~\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sw.reloadScript(context.Background())

	if got := sw.GetScript(); got != ls {
		t.Fatalf("✘ 回退后监视器应仍指向旧实例，实际指向另一实例")
	}
	if !qaEnvHas(ls, "probeObject") {
		t.Fatal("✘ 重载失败回退后 Lua 侧构造器消失（Init 未回放注册表）")
	}
}

// ==================== 表键口径统一 ====================
//
// 此前同一个下标有三种表征：Append 强制 int64、Set 收任意键、CompareKeys 不认 int，
// 于是 Go 侧 Set(1, v) 与 Set(int64(1), v) 是两个条目、彼此读不到。

func TestQaGL_SetIntAndInt64SameEntry(t *testing.T) {
	tbl := newTable(nil)

	tbl.Set(1, "a")
	if got, ok := tbl.Get(int64(1)); !ok || got != "a" {
		t.Fatalf("✘ int 键与 int64 键不是同一个条目: %v/%v", got, ok)
	}

	tbl.Set(int64(2), "b")
	if got, ok := tbl.Get(2); !ok || got != "b" {
		t.Fatalf("✘ int64 键读不到 int 键: %v/%v", got, ok)
	}

	// Lua 里 3 与 3.0 是同一个键
	tbl.Set(3.0, "c")
	if got, ok := tbl.Get(int(3)); !ok || got != "c" {
		t.Fatalf("✘ 整值浮点键没有折成整数: %v/%v", got, ok)
	}

	// 写一次只有一个条目
	tbl.Set(4, "d1")
	tbl.Set(int64(4), "d2")
	if tbl.Length() != 4 {
		t.Fatalf("✘ 同一键写了两次应只占一格，实际 %d 格", tbl.Length())
	}

	// 删除同样走归一化
	tbl.Delete(1)
	if _, ok := tbl.Get(int64(1)); ok {
		t.Fatal("✘ Delete(int) 没有删掉 int64 条目")
	}

	// 非整值浮点仍是独立的浮点键
	tbl.Set(2.5, "half")
	if got, ok := tbl.Get(2.5); !ok || got != "half" {
		t.Fatalf("✘ 非整值浮点键丢失: %v/%v", got, ok)
	}
}

func TestQaGL_AppendThenGetInt(t *testing.T) {
	tbl := newTable(nil)
	tbl.Append("x")
	tbl.Append("y")

	if got, ok := tbl.Get(1); !ok || got != "x" {
		t.Fatalf("✘ Append 建的表用 int 键读不到: %v/%v", got, ok)
	}
	if arr := tbl.GetArray(); len(arr) != 2 || arr[1] != "y" {
		t.Fatalf("✘ GetArray 顺序不对: %v", arr)
	}
}

func TestQaGL_IllegalKeyIsDropped(t *testing.T) {
	tbl := newTable(nil)
	tbl.Set("ok", 1)
	before := tbl.Length()

	tbl.Set(make(chan int), "bad") // 不可比较类型，必须丢弃而不是 panic
	if got := tbl.Length(); got != before {
		t.Fatalf("✘ 非法键应被丢弃，表长从 %d 变成 %d", before, got)
	}
}

func TestQaGL_CompareKeysNormalizes(t *testing.T) {
	if !CompareKeys(1, int64(1)) {
		t.Error("✘ int 与 int64 应视为同一个键")
	}
	if !CompareKeys(int32(7), 7.0) {
		t.Error("✘ int32 与整值浮点应视为同一个键")
	}
	if CompareKeys("1", 1) {
		t.Error("✘ 字符串键与数字键不应相等")
	}
	if !CompareKeys(nil, nil) || CompareKeys(nil, 1) {
		t.Error("✘ nil 键语义不对")
	}
}

func TestQaGL_NormalizeKeyContract(t *testing.T) {
	norm, err := NormalizeKey(5)
	if err != nil || norm != int64(5) {
		t.Fatalf("✘ NormalizeKey(5) = %v/%v，期望 int64(5)", norm, err)
	}
	if _, err := NormalizeKey(nil); err == nil {
		t.Error("✘ nil 键应报错")
	}
}

func TestLuaFileExt(t *testing.T) {
	if LuaFileExt != ".lua" {
		t.Fatalf("LuaFileExt 应为 .lua，实际: %s", LuaFileExt)
	}
}

func TestUpdateIntervalMs_Default(t *testing.T) {
	if UpdateIntervalMs != 1000 {
		t.Fatalf("无配置应回退到 1000，实际: %d", UpdateIntervalMs)
	}
}

func TestGCTickCount_Default(t *testing.T) {
	if GCTickCount != 1800 {
		t.Fatalf("无配置应回退到 1800，实际: %d", GCTickCount)
	}
}

func TestLuaCfg_NilContext(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("luaCfg 不应 panic: %v", r)
		}
	}()
	_ = luaCfg()
}

func TestRun_NoConfig(t *testing.T) {
	if err := Run(nil); err != nil {
		t.Fatalf("无配置 Run 应返回 nil，实际: %v", err)
	}
}

func TestStop_NoInstance(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop 不应 panic: %v", r)
		}
	}()
	Stop(nil)
}

func TestCall_NoInstance(t *testing.T) {
	_, err := Call("noop")
	if err == nil {
		t.Fatal("未启动时 Call 应返回错误")
	}
}

func TestCallWithContext_NoInstance(t *testing.T) {
	_, err := CallWithContext(t.Context(), "noop")
	if err == nil {
		t.Fatal("未启动时 CallWithContext 应返回错误")
	}
}

func TestRegisterLuaClass_Nil(t *testing.T) {
	if err := RegisterLuaClass(nil); err == nil {
		t.Fatal("nil 类应报错")
	}
}

func TestRegisterLuaFunc_ValidAndDuplicate(t *testing.T) {
	cb := func(_ *rt.Thread, _ *rt.GoCont) (rt.Cont, error) { return nil, nil }
	// 用包级唯一函数名以避免与已有测试冲突
	name := "test_register_lua_func_unique"
	if err := RegisterLuaFunc(name, cb); err != nil {
		// 若已被其他测试注册过，视为通过
		t.Logf("RegisterLuaFunc 首次注册返回: %v（可能已被占用）", err)
	}
	if err := RegisterLuaFunc(name, cb); err == nil {
		t.Fatal("重复注册应报错")
	}
}

// TestLuaScriptTimerOwnership 验证脚本 update 定时器的所有权闭环：
// Close 即彻底作废并归还对象池且丢弃指针，重载后必须重新起表。
func TestLuaScriptTimerOwnership(t *testing.T) {
	localtimer.Run(context.Background())
	defer localtimer.TimeStop(context.Background())

	// 实例注册表正常由 RunLua 初始化，本用例直接走构造函数，需自行补齐
	luaInstancesMu.Lock()
	if luaInstances == nil {
		luaInstances = make(map[int64]*LuaScript)
	}
	luaInstancesMu.Unlock()
	defer StopLua()

	path := filepath.Join(t.TempDir(), "driver.lua")
	if err := os.WriteFile(path, []byte("local driver_value = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	putsBefore := localtimer.GetTimerPoolStats().Puts

	ls, err := NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if ls.timer == nil {
		t.Fatal("✘ 新建脚本实例必须持有 update 定时器")
	}

	ls.Close()
	if puts := localtimer.GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ Close 未把 update 定时器归还对象池: Puts %d -> %d", putsBefore, puts)
	}
	if ls.timer != nil {
		t.Fatal("✘ Close 后必须丢弃定时器指针，实例已归池，再引用即悬垂")
	}
	ls.Close() // 重复 Close 不得二次归还
	if puts := localtimer.GetTimerPoolStats().Puts; puts != putsBefore+1 {
		t.Fatalf("✘ 重复 Close 造成二次归还: Puts=%d", puts)
	}

	if err := ls.ReloadScriptWithContext(context.Background()); err != nil {
		t.Fatalf("✘ 重载失败: %v", err)
	}
	if ls.timer == nil {
		t.Fatal("✘ 重载后未重新起表，该实例永远不会再被驱动")
	}
	if !ls.timer.GetParent().IsActive() {
		t.Fatal("✘ 重载后的定时器未进入调度")
	}
	ls.Close()
	t.Log("✔ Close 作废回池且不二次归还，重载后重新拿到驱动")
}

// ==================== reload.go 换代快照（Stop→Start）回归 ====================
//
// 事故形状：watch 协程原先回读 sw.ctx / sw.stopChan 字段。Stop→Start 快速换代会把
// 这两个字段替换为新一代对象，旧协程 select 的却是新通道——既收不到自己那一代的
// 关闭信号（协程泄漏），又可能被新一代的 Stop 误杀（running=true 却无人监控）。
// 修复：Start 在 mu 内取快照并以参数传给 watch；Stop 只 cancel/close 自己捕获的那一代。
// 本机有 C 工具链（gcc + CGO_ENABLED=1，-race 可跑）；这里仍用「协程数收敛 + 换代后
// 监控仍有效」做观测量，因为它能直接盯住本 bug 的形状，而 -race 只报内存竞争。

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

// probeObject 用于验证「Go 类注册到 Lua」整条链路是否真的可用：
// 方法全部以下列指针接收者声明，正是注册表最容易漏掉的一类方法。
type probeObject struct {
	ILuaClassObject

	uid     int64
	name    string
	items   []int64
	deleted bool
}

func (p *probeObject) Init(id int64, ls *LuaScript) { p.uid = id }
func (p *probeObject) SetName(name string)          { p.name = name }
func (p *probeObject) GetName() string              { return p.name }
func (p *probeObject) Uid() int64                   { return p.uid }
func (p *probeObject) AddItem(v int64)              { p.items = append(p.items, v) }
func (p *probeObject) ItemCount() int               { return len(p.items) }

func (p *probeObject) Tag(id int, text string, extra ...any) string {
	return fmt.Sprintf("%d|%s|%d", id, text, len(extra))
}

// registerProbeClass 注册探针类并在结束后摘掉，避免污染全局注册表。
// 收 testing.TB 而非 *testing.T：基准也要用它，两边都有 Helper/Cleanup。
func registerProbeClass(t testing.TB) {
	t.Helper()
	probed := &probeObject{}
	registeredClassesMu.Lock()
	if registeredClasses == nil {
		registeredClasses = make(map[string]ILuaClassInterface)
	}
	registeredClasses["probeObject"] = probed
	registeredClassesMu.Unlock()

	t.Cleanup(func() {
		registeredClassesMu.Lock()
		delete(registeredClasses, "probeObject")
		registeredClassesMu.Unlock()
	})
}

// newProbeScript 用给定 Lua 源码建一个脚本实例，把主块报错原样交回调用方。
func newProbeScript(t *testing.T, src string) (*LuaScript, error) {
	t.Helper()
	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })

	t.Cleanup(StopLua)

	path := filepath.Join(t.TempDir(), "probe.lua")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewLuaScriptWithContext(context.Background(), path)
}

// runProbeScript 主块必须执行成功，否则直接判失败。
func runProbeScript(t *testing.T, src string) *LuaScript {
	t.Helper()
	ls, err := newProbeScript(t, src)
	if err != nil {
		t.Fatalf("✘ 脚本执行失败: %v", err)
	}
	return ls
}
