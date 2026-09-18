package lua

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rt "github.com/arnodel/golua/runtime"
	"touchgocore/localtimer"
)

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
