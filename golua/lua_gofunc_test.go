package lua

import (
	"fmt"
	"testing"

	rt "github.com/arnodel/golua/runtime"
)

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
