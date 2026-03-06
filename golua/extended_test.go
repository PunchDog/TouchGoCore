package lua

import (
	"testing"
)

// TestParsePath 测试路径解析函数
func TestParsePath(t *testing.T) {
	testCases := []struct {
		name     string
		path     string
		expected []interface{}
	}{
		{
			name:     "点号分隔路径",
			path:     "player.stats.health",
			expected: []interface{}{"player", "stats", "health"},
		},
		{
			name:     "方括号语法",
			path:     "player['name']",
			expected: []interface{}{"player", "name"},
		},
		{
			name:     "混合语法",
			path:     "player.stats['level']",
			expected: []interface{}{"player", "stats", "level"},
		},
		{
			name:     "数字索引",
			path:     "players[1]",
			expected: []interface{}{"players", "1"},
		},
		{
			name:     "空路径",
			path:     "",
			expected: nil,
		},
		{
			name:     "单级路径",
			path:     "player",
			expected: []interface{}{"player"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := parsePath(tc.path)
			if len(result) != len(tc.expected) {
				t.Errorf("期望长度 %d, 实际长度 %d", len(tc.expected), len(result))
				return
			}

			for i, expected := range tc.expected {
				if result[i] != expected {
					t.Errorf("期望 %v, 实际 %v", expected, result[i])
				}
			}
		})
	}
}

// TestLuaTableBasic 测试 LuaTable 基本功能
func TestLuaTableBasic(t *testing.T) {
	tbl := NewLuaTablePooled(nil)
	defer PutLuaTable(tbl)

	// 测试 Set 和 Get
	tbl.Set("name", "Test")
	if val, ok := tbl.Get("name"); !ok || val != "Test" {
		t.Errorf("期望 'Test', 实际 %v", val)
	}

	// 测试 Append
	tbl.Append(1)
	tbl.Append(2)
	tbl.Append(3)
	if val, ok := tbl.Get(1); !ok || val != 1 {
		t.Errorf("期望 1, 实际 %v", val)
	}
	if val, ok := tbl.Get(2); !ok || val != 2 {
		t.Errorf("期望 2, 实际 %v", val)
	}
	if val, ok := tbl.Get(3); !ok || val != 3 {
		t.Errorf("期望 3, 实际 %v", val)
	}

	// 测试 HasData
	if !tbl.HasData() {
		t.Error("期望 HasData() 返回 true")
	}

	// 测试 Length
	if tbl.Length() != 4 { // 1 (name) + 3 (append)
		t.Errorf("期望长度 4, 实际 %d", tbl.Length())
	}

	// 测试 SubTable
	subTbl := tbl.SubTable("nested")
	subTbl.Set("value", 42)
	if val, ok := subTbl.Get("value"); !ok || val != 42 {
		t.Errorf("期望 42, 实际 %v", val)
	}

	// 测试 GetByPath
	if val, ok := tbl.GetByPath("nested.value"); !ok || val != 42 {
		t.Errorf("期望 42, 实际 %v", val)
	}

	// 测试 SetByPath
	if err := tbl.SetByPath("nested.deep.value", "test"); err != nil {
		t.Errorf("SetByPath 失败: %v", err)
	}
	if val, ok := tbl.GetByPath("nested.deep.value"); !ok || val != "test" {
		t.Errorf("期望 'test', 实际 %v", val)
	}
}

// TestLuaTablePool 测试 LuaTable 对象池
func TestLuaTablePool(t *testing.T) {
	// 从池中获取
	tbl1 := GetLuaTable()
	tbl1.Set("key", "value")

	// 放回池中
	PutLuaTable(tbl1)

	// 再次从池中获取
	tbl2 := GetLuaTable()
	// 检查是否被清空
	if val, ok := tbl2.Get("key"); ok {
		t.Errorf("期望池化对象被清空, 实际 %v", val)
	}

	PutLuaTable(tbl2)
}
