package lua

import (
	"sync"
	"touchgocore/syncmap"
)

var (
	luaTablePool = &sync.Pool{
		New: func() interface{} {
			return &LuaTable{
				tbl: &syncmap.Map{},
			}
		},
	}
)

// GetLuaTable 从池中获取 LuaTable
func GetLuaTable() *LuaTable {
	return luaTablePool.Get().(*LuaTable)
}

// PutLuaTable 将 LuaTable 返回到池中
func PutLuaTable(tbl *LuaTable) {
	if tbl != nil && tbl.tbl != nil {
		// 清空 table 数据
		tbl.tbl.ClearAll(nil)
		// 重置其他可能的状态
		luaTablePool.Put(tbl)
	}
}

// GetLuaTableWithCapacity 从池中获取指定容量的 LuaTable
func GetLuaTableWithCapacity(capacity int) *LuaTable {
	tbl := GetLuaTable()
	// 这里可以根据需要预分配容量
	return tbl
}

// NewLuaTablePooled 使用池化方式创建 LuaTable
func NewLuaTablePooled(data interface{}) *LuaTable {
	tbl := GetLuaTable()

	if data == nil {
		return tbl
	}

	// 填充数据
	switch v := data.(type) {
	case []string:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []int64:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []float64:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case []interface{}:
		for _, item := range v {
			tbl.AddListData(item)
		}
	case map[interface{}]interface{}:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[string]string:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[string]int64:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[int64]int64:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	case map[int64]string:
		for key, value := range v {
			tbl.SetTableData(key, value)
		}
	}

	return tbl
}
