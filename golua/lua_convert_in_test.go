package lua

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	rt "github.com/arnodel/golua/runtime"
)

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
