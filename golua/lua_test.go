package lua

import (
	"testing"

	rt "github.com/arnodel/golua/runtime"
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
