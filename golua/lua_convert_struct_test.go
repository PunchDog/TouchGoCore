package lua

import (
	"context"
	"testing"
	"time"

	rt "github.com/arnodel/golua/runtime"
)

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
