package mapmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	lua "touchgocore/golua"
	"touchgocore/localtimer"
)

// loadTestMap 写一份最小地图 JSON 并注册进 _maplist，用例结束后摘掉。
func loadTestMap(t *testing.T, mapId uint32) *Map {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("map_%d.json", mapId))
	content := fmt.Sprintf(`{"mapid":%d,"node":[[%s]]}`, mapId, `{"isblock":false,"viewid":1}`)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Map{}
	if err := m.Load(path); err != nil {
		t.Fatalf("加载测试地图失败: %v", err)
	}
	t.Cleanup(func() { _maplist.Delete(mapId) })
	return m
}

// runLuaFile 用给定路径的 Lua 源建一个脚本实例。
func runLuaFile(t *testing.T, path string) (*lua.LuaScript, error) {
	t.Helper()
	localtimer.Run(context.Background())
	t.Cleanup(func() { localtimer.TimeStop(context.Background()) })
	return lua.NewLuaScriptWithContext(context.Background(), path)
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

// TestNpcLuaScriptProducesValidNpc 跑通仓库自带的 npc.lua，断言 Go 侧真正拿到
// 了配置里的数值。这里重点盯 AddShop 的实参序：Lua 曾照抄配置顺序传参，
// 价格和限购数会互换。
func TestNpcLuaScriptProducesValidNpc(t *testing.T) {
	if err := lua.RegisterLuaClass(&Npc{}); err != nil {
		t.Fatalf("注册 Npc 类失败: %v", err)
	}
	loadTestMap(t, 1001)

	ls, err := runLuaFile(t, "npc.lua")
	if err != nil {
		t.Fatalf("✘ npc.lua 执行失败: %v", err)
	}
	t.Cleanup(ls.Close)

	npcs := collectNpcs(t, ls)
	if len(npcs) != 1 {
		t.Fatalf("✘ 应登记 1 个 NPC，实际 %d 个", len(npcs))
	}
	n := npcs[0]

	if errs := n.Validate(); len(errs) > 0 {
		t.Fatalf("✘ 配置校验未通过: %v", errs)
	}
	if got := n.GetPathPoints(); len(got) != 4 {
		t.Fatalf("✘ 路径点应为 4 个，实际 %d", len(got))
	}

	items := n.GetShop(1)
	if len(items) != 2 {
		t.Fatalf("✘ 商店1 应有 2 件商品，实际 %d", len(items))
	}
	first := items[0]
	if first.ItemID != 2001 || first.CostType != 1000 {
		t.Errorf("✘ 商品身份错位: itemId=%d costType=%d", first.ItemID, first.CostType)
	}
	// 配置写的是 {2001, 1000, 10000, 99, "day"}：价格 10000，限购 99
	if first.Cost != 10000 || first.MaxBuyCnt != 99 {
		t.Errorf("✘ 价格/限购被传反: cost=%d maxBuyCnt=%d（应为 10000 / 99）", first.Cost, first.MaxBuyCnt)
	}
	if first.UpdateTimeType != RefreshTypeDay {
		t.Errorf("✘ 刷新类型不对: %s", first.UpdateTimeType)
	}
	if len(n.GetShop(2)) != 1 {
		t.Errorf("✘ 商店2 应有 1 件商品")
	}

	// 「每次触发都随机」的对话：param1 从 Lua 传过来是 *lua.LuaTable
	item, ok := n.GetDialog(6)
	if !ok {
		t.Fatal("✘ 缺少对话 6")
	}
	allowed := map[string]bool{"哼，别来烦我！": true, "我今天心情不好...": true, "走开走开！": true, "别挡路！": true}
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		text := item.GetDisplayText()
		if !allowed[text] {
			t.Fatalf("✘ 随机文本越界: %q", text)
		}
		seen[text] = true
	}
	if len(seen) < 2 {
		t.Fatalf("✘ rand_mode=every 的对话 200 次只出现 %d 种文本，随机没生效", len(seen))
	}
}

// TestLuaCreatedNpcsKeepIndependentIdentity 同一段脚本里创建多个 NPC 时，
// 对象必须各自独立：修复前构造器用 script.UID 当登记键，第二个对象会盖掉第一个。
func TestLuaCreatedNpcsKeepIndependentIdentity(t *testing.T) {
	if err := lua.RegisterLuaClass(&Npc{}); err != nil {
		t.Fatalf("注册 Npc 类失败: %v", err)
	}

	path := filepath.Join(t.TempDir(), "two_npc.lua")
	src := `
local a = Npc()
local b = Npc()
a:SetId(11)
b:SetId(22)
a:SetName("甲")
b:SetName("乙")
a:AddMapPoint(1, 2, 3)
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, err := runLuaFile(t, path)
	if err != nil {
		t.Fatalf("✘ 脚本执行失败: %v", err)
	}
	t.Cleanup(ls.Close)

	npcs := collectNpcs(t, ls)
	if len(npcs) != 2 {
		t.Fatalf("✘ 应登记 2 个 NPC，实际 %d 个", len(npcs))
	}
	byId := map[uint32]*Npc{}
	for _, n := range npcs {
		byId[n.GetId()] = n
	}
	if len(byId) != 2 {
		t.Fatalf("✘ 两个 NPC 拿到同一个业务 ID: %d / %d", npcs[0].GetId(), npcs[1].GetId())
	}
	if n := byId[11]; n == nil || n.GetName() != "甲" || len(n.GetPathPoints()) != 1 {
		t.Error("✘ NPC 11 的状态被覆盖或丢失")
	}
	if n := byId[22]; n == nil || n.GetName() != "乙" || len(n.GetPathPoints()) != 0 {
		t.Error("✘ NPC 22 串到了 NPC 11 的数据")
	}
}

// TestNpcConstructorRejectsArgs 构造器不吞参数：传了就报错，别让脚本以为 ID 设上了。
func TestNpcConstructorRejectsArgs(t *testing.T) {
	if err := lua.RegisterLuaClass(&Npc{}); err != nil {
		t.Fatalf("注册 Npc 类失败: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bad_ctor.lua")
	if err := os.WriteFile(path, []byte("local n = Npc(123)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runLuaFile(t, path); err == nil {
		t.Fatal("✘ Npc(123) 应报错，实际静默忽略了参数")
	}
}

// TestNpcZeroValueUsable 不经 Init 的 Npc（JSON 直造、Go 侧手工 new）不能 panic。
func TestNpcZeroValueUsable(t *testing.T) {
	n := &Npc{}
	if n.GetShop(1) != nil {
		t.Error("✘ 空商店应返回 nil")
	}
	if n.HasShop() || n.HasDialog() {
		t.Error("✘ 零值 NPC 不该有商店或对话")
	}
	if n.GetDefaultDialog() != nil {
		t.Error("✘ 零值 NPC 不该有默认对话")
	}
	if _, ok := n.GetDialog(1); ok {
		t.Error("✘ 零值 NPC 不该查到对话")
	}
	n.AddShop(1, 100, 1, 5, 2000, RefreshTypeDay)
	n.AddDialog(1, "你好", DialogTypeNormal)
	if len(n.GetShop(1)) != 1 || !n.HasDialog() {
		t.Error("✘ 零值 NPC 首次 Add 后应可用")
	}
	if errs := n.Validate(); len(errs) == 0 {
		t.Error("✘ 未设置 ID 的 NPC 校验应报错")
	}
}

// TestNpcAddShopConcurrent 「取列表 → 追加 → 回写」必须整段互斥。
func TestNpcAddShopConcurrent(t *testing.T) {
	n := &Npc{}
	const goroutines, perGoroutine = 8, 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				n.AddShop(1, g*1000+i, 1, 1, 10, RefreshTypeNever)
			}
		}(g)
	}
	wg.Wait()

	if got := len(n.GetShop(1)); got != goroutines*perGoroutine {
		t.Fatalf("✘ 并发加货后商品条数丢失: 应 %d 实际 %d", goroutines*perGoroutine, got)
	}
}

// TestNpcAccessorsReturnCopies 交出去的切片必须是快照。
func TestNpcAccessorsReturnCopies(t *testing.T) {
	n := &Npc{}
	n.AddMapPoint(1, 1, 1)
	n.AddShop(1, 100, 1, 1, 10, RefreshTypeNever)

	points := n.GetPathPoints()
	points[0] = [3]int32{9, 9, 9}
	if again := n.GetPathPoints(); again[0] != [3]int32{1, 1, 1} {
		t.Error("✘ 改动返回的路径点影响了内部状态")
	}

	items := n.GetShop(1)
	items[0] = &ShopItem{ItemID: 666}
	if again := n.GetShop(1); again[0].ItemID != 100 {
		t.Error("✘ 改动返回的商品列表影响了内部状态")
	}
}

// TestNpcSetMapIdIdempotent 重复挂载不得在地图上留下两份实例。
func TestNpcSetMapIdIdempotent(t *testing.T) {
	m := loadTestMap(t, 3001)
	other := loadTestMap(t, 3002)

	n := &Npc{}
	n.SetId(300)
	n.SetMapId(3001)
	n.SetMapId(3001)
	if got := len(m.SnapshotNpcs()); got != 1 {
		t.Fatalf("✘ 重复 SetMapId 挂了 %d 份", got)
	}

	n.SetMapId(3002)
	if got := len(m.SnapshotNpcs()); got != 0 {
		t.Fatalf("✘ 改挂后旧地图上还剩 %d 个 NPC", got)
	}
	snapshot := other.SnapshotNpcs()
	if len(snapshot) != 1 || snapshot[0] != n {
		t.Fatal("✘ 改挂后新地图上没有该 NPC")
	}

	list := n.GetPathPoints()
	if list != nil {
		t.Fatal("✘ 空白 NPC 不该有路径点")
	}
}

// TestNpcSetMapIdRejectsUnknownMap 目标地图不存在时一个字都不能改：修复前先把
// MapID 写进去再 return，NPC 停在「有地图号但没挂在任何图上」的状态，
// Validate 的「所属地图未设置」被这次赋值永久掩盖。
func TestNpcSetMapIdRejectsUnknownMap(t *testing.T) {
	m := loadTestMap(t, 3201)

	n := &Npc{}
	n.SetId(320)
	n.SetName("守卫")
	n.SetShape("guard")
	n.SetMapId(3201)

	n.SetMapId(999999)

	if n.MapID != 3201 {
		t.Errorf("✘ 未知地图改写了 MapID: %d", n.MapID)
	}
	if n.attachedTo != m {
		t.Error("✘ 未知地图把 NPC 从旧地图上弄下来了")
	}
	if got := len(m.SnapshotNpcs()); got != 1 {
		t.Fatalf("✘ 旧地图上的 NPC 数量变成 %d", got)
	}
	if errs := n.Validate(); len(errs) != 0 {
		t.Errorf("✘ 合法 NPC 校验报错: %v", errs)
	}
}

// TestNpcSetMapIdConcurrent 同一 NPC 并发改挂：整段必须互斥，
// 分开加锁时两个 goroutine 会各自 append，一张地图上留下两份实例。
func TestNpcSetMapIdConcurrent(t *testing.T) {
	m := loadTestMap(t, 3301)
	n := &Npc{}
	n.SetId(330)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			n.SetMapId(3301)
		}()
	}
	wg.Wait()

	if got := len(m.SnapshotNpcs()); got != 1 {
		t.Fatalf("✘ 并发 SetMapId 在地图上留了 %d 份实例", got)
	}
}

// TestMapLoadParsesFields 地图 JSON 反序列化必须真的落到 this 上。
func TestMapLoadParsesFields(t *testing.T) {
	m := loadTestMap(t, 4001)
	if m.MapId != 4001 {
		t.Fatalf("✘ MapId 未解析: %d", m.MapId)
	}
	if len(m.Node) != 1 || len(m.Node[0]) != 1 || m.Node[0][0].ViewID != 1 {
		t.Fatalf("✘ Node 未解析: %+v", m.Node)
	}
	if _, ok := GetMap(4001); !ok {
		t.Error("✘ 地图未登记进 _maplist")
	}
}

// TestMapLoadRejectsDuplicateId 重复 MapId 要说明保留的是哪一份。
func TestMapLoadRejectsDuplicateId(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.json")
	if err := os.WriteFile(path, []byte(`{"mapid":5001,"node":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Map{}).Load(path); err != nil {
		t.Fatalf("首次加载应成功: %v", err)
	}
	t.Cleanup(func() { _maplist.Delete(5001) })

	err := (&Map{}).Load(path)
	if err == nil {
		t.Fatal("✘ 重复 MapId 应报错")
	}
	if !strings.Contains(err.Error(), "dup.json") {
		t.Fatalf("✘ 冲突信息应指明保留方: %v", err)
	}
}

// TestMapJsonCannotInjectNpcs NPC 列表非导出 + json:"-"：外部 JSON 造不出
// 绕过 Init 的 NPC（这类对象的 Shop/Dialog 是 nil，一用就崩）。
func TestMapJsonCannotInjectNpcs(t *testing.T) {
	raw := []byte(`{"mapid":6001,"node":[],"Npc":[{"id":1,"name":"注入"}],"npcs":[{"id":2}]}`)
	m := &Map{}
	if err := json.Unmarshal(raw, m); err != nil {
		t.Fatal(err)
	}
	if got := len(m.SnapshotNpcs()); got != 0 {
		t.Fatalf("✘ JSON 仍能注入 %d 个 NPC", got)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Npc") || strings.Contains(string(out), "npcs") {
		t.Fatalf("✘ NPC 列表仍被序列化: %s", out)
	}
}

// TestRunMapThenLuaMountsNpcs 端到端验证启动顺序：RunMap 先注册 Npc 类并装载地图，
// 之后 Lua 脚本里的 Npc()/SetMapId 才可能真的把 NPC 挂到地图上。
func TestRunMapThenLuaMountsNpcs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "1001.json"), []byte(`{"mapid":1001,"node":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := corectx.WithCfg(context.Background(), &config.Cfg{MapPath: dir})
	if err := RunMap(ctx); err != nil {
		t.Fatalf("RunMap 失败: %v", err)
	}
	t.Cleanup(func() { StopMap(context.Background()) })

	if _, ok := GetMap(1001); !ok {
		t.Fatal("✘ 地图目录已配置却没能装载")
	}

	ls, err := runLuaFile(t, "npc.lua")
	if err != nil {
		t.Fatalf("✘ npc.lua 执行失败: %v", err)
	}
	t.Cleanup(ls.Close)

	m, _ := GetMap(1001)
	if got := len(m.SnapshotNpcs()); got != 1 {
		t.Fatalf("✘ Lua 创建的 NPC 没挂上地图: map=%d 个", got)
	}
}

func TestValidateNpcsSummarizesProblems(t *testing.T) {
	m := loadTestMap(t, 8001)
	good := &Npc{}
	good.SetId(801)
	good.SetName("正常")
	good.SetShape("s1")
	good.SetMapId(8001)

	bad := &Npc{}
	bad.SetMapId(8001)

	problems := ValidateNpcs()
	if len(problems) == 0 {
		t.Fatal("✘ 无名字/无外观的 NPC 应被汇总出来")
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "NPC名称不能为空") || !strings.Contains(joined, "NPC外观不能为空") {
		t.Fatalf("✘ 汇总缺少具体项: %s", joined)
	}
	if strings.Contains(joined, "npc=Npc[id=801") {
		t.Fatalf("✘ 合法 NPC 也被报错了: %s", joined)
	}
	_ = m
}

// TestStopMapReleasesNpcs 停服要把 NPC 挂载一起断开。
func TestStopMapReleasesNpcs(t *testing.T) {
	m := loadTestMap(t, 7001)
	n := &Npc{}
	n.SetId(700)
	n.SetMapId(7001)
	if len(m.SnapshotNpcs()) != 1 {
		t.Fatal("✘ 前置条件：NPC 未挂上地图")
	}

	StopMap(context.Background())

	if len(m.SnapshotNpcs()) != 0 {
		t.Error("✘ StopMap 后地图仍持有 NPC")
	}
	if got := _maplist.Length(); got != 0 {
		t.Errorf("✘ StopMap 后 _maplist 还有 %d 项", got)
	}
}
