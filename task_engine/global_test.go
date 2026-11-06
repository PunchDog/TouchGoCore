package taskengine

import (
	"testing"

	"touchgocore/list"
)

// TestGlobalDefsIndex 验证全局定义索引的写入、整体替换与单条回填。
func TestGlobalDefsIndex(t *testing.T) {
	a := &TaskDef{ID: 9001, Title: "A", Enabled: 1}
	b := &TaskDef{ID: 9002, Title: "B", Enabled: 1}

	// 整体替换：写入两条。
	taskglobal.setDefs([]TaskDef{*a, *b})
	if got, ok := taskglobal.getDef(9001); !ok || got.Title != "A" {
		t.Fatalf("getDef(9001)=%+v ok=%v", got, ok)
	}

	// 再替换一次：不在新切片里的旧 id 必须消失。
	taskglobal.setDefs([]TaskDef{*b})
	if _, ok := taskglobal.getDef(9001); ok {
		t.Fatal("setDefs 必须整体替换，旧定义不应残留")
	}

	// 单条回填；nil 是空操作。
	taskglobal.storeDef(a)
	if got, ok := taskglobal.getDef(9001); !ok || got.ID != 9001 {
		t.Fatalf("storeDef 后应可读: %+v ok=%v", got, ok)
	}
	taskglobal.storeDef(nil)
}

// TestNewRegistersEngine 验证 New 把引擎挂进全局引擎链。
func TestNewRegistersEngine(t *testing.T) {
	before := taskglobal.engineList.Length()
	e := newTestEngine(t, newFakeStore(sampleDef(1)))

	if got := taskglobal.engineList.Length(); got != before+1 {
		t.Fatalf("引擎注册数 %d → %d，应 +1", before, got)
	}
	found := false
	taskglobal.engineList.Range(func(n list.INode) bool {
		if n.GetData() == e {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatal("全局引擎链中应能按数据找到该引擎")
	}
}
