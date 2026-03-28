package lua

import (
	"fmt"
	"testing"
	
	"github.com/aarzilli/golua/lua"
)

// TestLuaManager_Basic 测试 LuaManager 基本功能
func TestLuaManager_Basic(t *testing.T) {
	manager := NewLuaManager()
	
	// 创建第一个脚本实例
	script1, err := manager.NewScript("test.lua")
	if err != nil {
		// 预期错误：test.lua 不存在，但应该能创建实例
		if err.Error() == "加载 Lua 脚本失败: cannot open test.lua: No such file or directory" {
			t.Log("预期错误：test.lua 不存在")
		} else {
			t.Errorf("创建脚本失败: %v", err)
		}
	}
	
	if script1 == nil {
		t.Fatal("脚本实例不应为 nil")
	}
	
	if script1.UID != 1 {
		t.Errorf("第一个实例 UID 应为 1，实际为 %d", script1.UID)
	}
	
	// 获取实例
	script, ok := manager.GetScript(1)
	if !ok {
		t.Fatal("应能获取 UID=1 的脚本实例")
	}
	
	if script != script1 {
		t.Fatal("获取的实例应与创建的实例相同")
	}
	
	// 默认实例
	defaultScript, err := manager.DefaultScript()
	if err != nil {
		t.Fatalf("获取默认实例失败: %v", err)
	}
	
	if defaultScript != script1 {
		t.Fatal("默认实例应为第一个创建的实例")
	}
	
	// 创建第二个实例
	script2, err := manager.NewScript("test2.lua")
	if err != nil {
		t.Logf("创建第二个脚本: %v (预期文件不存在)", err)
	}
	
	if script2.UID != 2 {
		t.Errorf("第二个实例 UID 应为 2，实际为 %d", script2.UID)
	}
	
	// 统计信息
	stats := manager.Stats()
	if stats["scripts_total"] != 2 {
		t.Errorf("脚本总数应为 2，实际为 %d", stats["scripts_total"])
	}
	
	if stats["scripts_active"] != 2 {
		t.Errorf("活跃脚本数应为 2，实际为 %d", stats["scripts_active"])
	}
	
	// 关闭第一个实例
	err = manager.CloseScript(1)
	if err != nil {
		t.Errorf("关闭脚本失败: %v", err)
	}
	
	// 确认已关闭
	_, ok = manager.GetScript(1)
	if ok {
		t.Error("关闭后不应能获取实例")
	}
	
	// 统计信息更新
	stats = manager.Stats()
	if stats["scripts_closed"] != 1 {
		t.Errorf("已关闭脚本数应为 1，实际为 %d", stats["scripts_closed"])
	}
	
	if stats["scripts_active"] != 1 {
		t.Errorf("活跃脚本数应为 1，实际为 %d", stats["scripts_active"])
	}
	
	// 默认实例应更新为第二个
	defaultScript, err = manager.DefaultScript()
	if err != nil {
		t.Fatalf("获取默认实例失败: %v", err)
	}
	
	if defaultScript.UID != 2 {
		t.Errorf("默认实例 UID 应为 2，实际为 %d", defaultScript.UID)
	}
	
	// 关闭所有
	manager.CloseAll()
	
	// 确认全部关闭
	ids := manager.GetAllScriptIDs()
	if len(ids) != 0 {
		t.Errorf("关闭所有后应无活跃实例，实际有 %d 个", len(ids))
	}
	
	// 默认实例应为 nil
	_, err = manager.DefaultScript()
	if err == nil {
		t.Error("关闭所有后获取默认实例应失败")
	}
}

// TestLuaManager_RegisterFunc 测试注册全局函数
func TestLuaManager_RegisterFunc(t *testing.T) {
	manager := NewLuaManager()
	
	// 注册一个测试函数
	err := manager.RegisterFunc("testFunc", func(L *lua.State) int {
		L.PushString("Hello from Go")
		return 1
  })
	
	if err != nil {
		t.Fatalf("注册函数失败: %v", err)
	}
	
	// 重复注册应失败
	err = manager.RegisterFunc("testFunc", func(L *lua.State) int {
		return 0
  })
	
	if err == nil {
		t.Error("重复注册函数应失败")
	}
	
	// 创建脚本实例，函数应已注册
	script, err := manager.NewScript("test.lua")
	if err != nil {
		t.Logf("创建脚本: %v (预期文件不存在)", err)
	}
	
	if script == nil {
		t.Fatal("脚本实例不应为 nil")
	}
}

// TestLuaManager_Stats 测试统计信息
func TestLuaManager_Stats(t *testing.T) {
	manager := NewLuaManager()
	
	// 初始统计
	stats := manager.Stats()
	
	expected := map[string]int64{
		"scripts_total":    0,
		"scripts_closed":   0,
		"scripts_active":   0,
		"calls_total":     0,
		"funcs_registered": 0,
		"classes_registered": 0,
	}
	
	for key, val := range expected {
		if stats[key] != val {
			t.Errorf("统计字段 %s: 期望 %d, 实际 %d", key, val, stats[key])
		}
	}
	
	// 注册函数
	_ = manager.RegisterFunc("testFunc", func(L *lua.State) int {
		return 0
	})
	
	// 检查更新
	stats = manager.Stats()
	if stats["funcs_registered"] != 1 {
		t.Errorf("注册函数后 funcs_registered 应为 1，实际为 %d", stats["funcs_registered"])
	}
	
	// 调用函数
	_, _ = manager.Call("testFunc")
	
	stats = manager.Stats()
	if stats["calls_total"] != 1 {
		t.Errorf("调用函数后 calls_total 应为 1，实际为 %d", stats["calls_total"])
	}
}

// TestLuaManager_Global 测试全局管理器
func TestLuaManager_Global(t *testing.T) {
	// 全局管理器应能正常工作
	_, err := globalManager.NewScript("global_test.lua")
	if err != nil {
		t.Logf("全局管理器创建脚本: %v (预期文件不存在)", err)
	}
	
	// 向后兼容的全局函数
	err = RegisterLuaFunc("globalFunc", func(L *lua.State) int {
		return 0
	})
	
	if err != nil {
		t.Errorf("全局 RegisterLuaFunc 失败: %v", err)
	}
}

func ExampleLuaManager() {
	// 创建管理器
	manager := NewLuaManager()
	
	// 注册全局函数
	_ = manager.RegisterFunc("goAdd", func(L *lua.State) int {
		a := L.ToInteger(1)
		b := L.ToInteger(2)
		result := a + b
		L.PushInteger(int64(result)) // 转换为 int64
		return 1
	})
	
	// 创建脚本实例
	script, err := manager.NewScript("example.lua")
	if err != nil {
		fmt.Printf("创建脚本失败: %v\n", err)
		return
	}
	
	// 调用 Lua 函数
	results, err := script.Call("luaFunction", 10, 20)
	if err != nil {
		fmt.Printf("调用失败: %v\n", err)
		return
	}
	
	fmt.Printf("调用结果: %v\n", results)
}