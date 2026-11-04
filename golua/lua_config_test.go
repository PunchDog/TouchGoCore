package lua

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/localtimer"
)

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
