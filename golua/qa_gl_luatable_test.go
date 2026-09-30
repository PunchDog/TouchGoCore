package lua

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// QA-GL: LuaTable Append/GetArray 键类型一致性 + pathCache 清理竞态回归。

func TestQaGL_AppendThenGetArray(t *testing.T) {
	lt := newTable(nil)
	lt.Append("a")
	lt.Append("b")
	lt.Append("c")

	// 旧实现：Append 用 Go int 键、GetArray 只认 int64 → 恒返回空数组（确定性红）
	arr := lt.GetArray()
	if len(arr) != 3 {
		t.Fatalf("GetArray 长度 = %d, 期望 3", len(arr))
	}
	for i, want := range []string{"a", "b", "c"} {
		if arr[i] != want {
			t.Errorf("arr[%d] = %v, 期望 %q（顺序错乱）", i, arr[i], want)
		}
	}
	if lt.Length() != 3 {
		t.Errorf("Length = %d, 期望 3", lt.Length())
	}
}

func TestQaGL_GetArrayAcceptsLegacyIntKeys(t *testing.T) {
	lt := newTable(nil)
	lt.Set(1, "x") // 历史调用方可能传 Go int 键
	lt.Set(2, "y")

	arr := lt.GetArray()
	if len(arr) != 2 || arr[0] != "x" || arr[1] != "y" {
		t.Fatalf("GetArray = %v, 期望 [x y]", arr)
	}
}

func TestQaGL_PopulateSliceOfMapsGetArray(t *testing.T) {
	data := []map[string]interface{}{
		{"k": "v1"},
		{"k": "v2"},
	}
	lt := newTable(data)

	arr := lt.GetArray()
	if len(arr) != 2 {
		t.Fatalf("GetArray 长度 = %d, 期望 2", len(arr))
	}
	for i, want := range []string{"v1", "v2"} {
		nested, ok := arr[i].(*LuaTable)
		if !ok {
			t.Fatalf("arr[%d] 类型 %T, 期望 *LuaTable", i, arr[i])
		}
		if got, _ := nested.GetString("k"); got != want {
			t.Errorf("arr[%d].k = %q, 期望 %q", i, got, want)
		}
	}
}

func TestQaGL_SetByPathExistingPathNoCrash(t *testing.T) {
	// 基线 bug 回归：SetByPath 的 else 分支用 `next, isTable := val.(*LuaTable)`
	// 遮蔽了外层 next，路径已存在时 current 被置 nil —— 第二次写同一路径必
	// 空指针 panic（确定性红）。
	lt := newTable(nil)
	if err := lt.SetByPath("a.b.c", int64(1)); err != nil {
		t.Fatalf("首次 SetByPath: %v", err)
	}
	if err := lt.SetByPath("a.b.c", int64(2)); err != nil {
		t.Fatalf("第二次 SetByPath（旧实现在此 panic）: %v", err)
	}
	v, ok := lt.GetByPath("a.b.c")
	if !ok || v != int64(2) {
		t.Fatalf("GetByPath = %v,%v, 期望 2,true", v, ok)
	}
}

func TestQaGL_PathCacheConcurrentCleanup(t *testing.T) {
	// 旧实现 cleanupCache 里 `pathCache = sync.Map{}` 裸重新赋值包级变量，
	// 与并发 Load/Store/Range 竞争；修复后保持同一实例、Range+Delete 清空。
	// 竞态窗口无 -race 不能确定性复现，用压测 + 清理后契约断言佐证。
	lt := newTable(nil)
	if err := lt.SetByPath("qa.b.c", int64(7)); err != nil {
		t.Fatalf("SetByPath: %v", err)
	}

	var panics atomic.Int64
	var wg sync.WaitGroup
	dur := 300 * time.Millisecond
	if testing.Short() {
		dur = 150 * time.Millisecond
	}
	deadline := time.Now().Add(dur)

	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panics.Add(1)
					buf := make([]byte, 4096)
					n := runtime.Stack(buf, false)
					t.Errorf("pathCache 压测协程 panic: %v\n%s", rec, buf[:n])
				}
			}()
			// 超过 cacheMaxSize 个不同路径，持续触发 cleanupCache
			for i := 0; time.Now().Before(deadline); i++ {
				p := fmt.Sprintf("p%d_%d.x.y", g, i%(cacheMaxSize+200))
				getPathFromCache(p)
				_ = lt.SetByPath(p, i)
				_, _ = lt.GetByPath(p)
			}
		}(g)
	}
	wg.Wait()

	if n := panics.Load(); n != 0 {
		t.Fatalf("pathCache 并发清理期间发生 %d 次 panic", n)
	}

	// 契约：清理后缓存仍可用，解析结果正确
	entry := getPathFromCache("qa.b.c")
	if entry == nil || len(entry.keys) != 3 {
		t.Fatalf("清理后 getPathFromCache 解析异常: %+v", entry)
	}
	if v, ok := lt.GetByPath("qa.b.c"); !ok || v != int64(7) {
		t.Fatalf("GetByPath(qa.b.c) = %v,%v, 期望 7,true", v, ok)
	}
}
