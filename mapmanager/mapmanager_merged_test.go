package mapmanager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	lua "touchgocore/golua"
	"touchgocore/localtimer"
)

// TestQaGL_NpcGettersFromLua 端到端验证「Go 方法返回 struct / struct 指针 / 切片」
// 在 Lua 侧到底是什么形状。
//
// 修复前 convert → populateTable 只认 Slice/Array/Map，Ptr/Struct 一个键都不填，
// HasData() 为假，于是 GetShop 返回的表是空的、GetDialog 返回 nil、GetDialogsByType
// 里每个元素都是 nil——脚本拿不到数据，Go 侧也不报错。键名口径是 json tag
// （ShopItem.item_id / DialogItem.text），与 HTTP/DB 两侧看到的逐字一致。
func TestQaGL_NpcGettersFromLua(t *testing.T) {
	if err := lua.RegisterLuaClass(&Npc{}); err != nil {
		t.Fatalf("注册 Npc 类失败: %v", err)
	}

	src := `
local n = Npc()
n:SetId(7001)
n:AddShop(1, 2001, 1000, 99, 10000, "day")
n:AddDialog(2, "第二", "normal", "p1")
n:AddDialog(1, "第一", "normal")

-- GetShop: []*ShopItem 必须是 1..n 顺序键的表
local s = n:GetShop(1)
if type(s) ~= "table" then error("GetShop 返回了 " .. type(s)) end
if #s ~= 1 then error("GetShop 元素数应为 1，实际 " .. tostring(#s)) end
if s[1].item_id ~= 2001 then error("item_id = " .. tostring(s[1].item_id)) end
if s[1].cost ~= 10000 then error("cost = " .. tostring(s[1].cost)) end
if s[1].update_time_type ~= "day" then error("update_time_type = " .. tostring(s[1].update_time_type)) end

-- GetDialog: (*DialogItem, bool) 必须是 (表, true)
local d, ok = n:GetDialog(1)
if ok ~= true then error("GetDialog 第二返回值应为 true，实际 " .. tostring(ok)) end
if type(d) ~= "table" then error("GetDialog 返回了 " .. type(d)) end
if d.id ~= 1 or d.text ~= "第一" then
    error("对话字段丢失: " .. tostring(d.id) .. "/" .. tostring(d.text))
end

-- 不存在的对话：nil + false，不能是空表
local missing, mok = n:GetDialog(99)
if missing ~= nil or mok ~= false then
    error("不存在的对话应返回 (nil,false)，实际 " .. type(missing) .. "/" .. tostring(mok))
end

-- AddDialog 的可变参数落到 param1
local d2 = n:GetDialog(2)
if d2.param1 ~= "p1" then error("param1 = " .. tostring(d2.param1)) end

-- GetDefaultDialog: 最小 id 的那条
local def = n:GetDefaultDialog()
if type(def) ~= "table" or def.id ~= 1 then error("默认对话不是 id=1 的那条") end

-- GetDialogsByType: 两个元素都是表
local all = n:GetDialogsByType("normal")
if type(all) ~= "table" or #all ~= 2 then error("GetDialogsByType 形状不对") end
if type(all[1]) ~= "table" or type(all[2]) ~= "table" then
    error("类型过滤结果里出现了非表元素")
end

-- 没有任何对话时 GetDefaultDialog 的 nil 指针必须仍是 nil
local empty = Npc()
if empty:GetDefaultDialog() ~= nil then error("空 NPC 的默认对话应为 nil") end
if empty:GetShop(1) ~= nil then error("空 NPC 的商店应为 nil") end
`

	dir := t.TempDir()
	path := filepath.Join(dir, "getter.lua")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })

	ls, err := lua.NewLuaScriptWithContext(context.Background(), path)
	if err != nil {
		t.Fatalf("✘ Lua 脚本执行失败（多半是 getter 返回值形状不对）: %v", err)
	}
	t.Cleanup(ls.Close)

	npcs := collectNpcs(t, ls)
	if len(npcs) != 2 {
		t.Fatalf("✘ 应登记 2 个 NPC，实际 %d", len(npcs))
	}
}

func collectNpcs(t *testing.T, ls *lua.LuaScript) []*Npc {
	t.Helper()
	var npcs []*Npc
	ls.RangeRegisteredObjects(func(_ int64, obj any) bool {
		if n, ok := obj.(*Npc); ok {
			npcs = append(npcs, n)
		}
		return true
	})
	return npcs
}
