package syncmap

import "testing"

func TestNewMap_Empty(t *testing.T) {
	m := NewMap[string, int]()
	if m.Length() != 0 {
		t.Fatalf("新建 map 长度应为 0，实际: %d", m.Length())
	}
}

func TestMap_StoreLoad(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	if v, ok := m.Load("a"); !ok || v != 1 {
		t.Fatalf("Load(a) 失败: %v, %v", v, ok)
	}
	if m.Length() != 2 {
		t.Fatalf("Length 应为 2，实际: %d", m.Length())
	}
}

func TestMap_Store_UpdateNoInc(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("a", 99)
	if m.Length() != 1 {
		t.Fatalf("覆盖不应增加计数，实际: %d", m.Length())
	}
	if v, _ := m.Load("a"); v != 99 {
		t.Fatalf("应读到新值，实际: %d", v)
	}
}

func TestMap_Delete(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Delete("a")
	if _, ok := m.Load("a"); ok {
		t.Fatal("删除后不应命中")
	}
	if m.Length() != 0 {
		t.Fatalf("Length 应为 0，实际: %d", m.Length())
	}
	m.Delete("nonexistent") // 不应 panic
}

func TestMap_Clear(t *testing.T) {
	m := NewMap[int, string]()
	m.Store(1, "a")
	m.Store(2, "b")
	m.Clear()
	if m.Length() != 0 {
		t.Fatalf("Clear 后 Length 应为 0，实际: %d", m.Length())
	}
}

func TestMapAny_Basic(t *testing.T) {
	m := NewAny()
	m.Store("a", 1)
	m.Store("b", "two")
	if v, ok := m.Load("a"); !ok || v.(int) != 1 {
		t.Fatalf("Load 失败: %v, %v", v, ok)
	}
}

func TestMap_Range(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 5; i++ {
		m.Store(i, i*10)
	}
	count := 0
	m.Range(func(k, v int) bool {
		count++
		if v != k*10 {
			t.Fatalf("值异常: k=%d v=%d", k, v)
		}
		return true
	})
	if count != 5 {
		t.Fatalf("Range 应遍历 5 次，实际: %d", count)
	}
}

func TestMap_Range_Stop(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 10; i++ {
		m.Store(i, i)
	}
	count := 0
	m.Range(func(k, v int) bool {
		count++
		return count < 3 // 第三次停止
	})
	if count != 3 {
		t.Fatalf("Range 应在 3 次后停止，实际: %d", count)
	}
}
