package lua

import "testing"

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
