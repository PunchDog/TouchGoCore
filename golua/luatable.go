package lua

import (
	"reflect"
	"touchgocore/syncmap"

	"github.com/aarzilli/golua/lua"
)

// newTable 从 Go 数据创建 LuaTable 对象
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
	if data == nil {
		return tbl
	}

	// 根据数据类型转换
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
	case map[string]interface{}:
		// 支持嵌套 map[string]interface{}
		for key, value := range v {
			// 递归处理嵌套结构
			if isNestedStructure(value) {
				tbl.SetTableData(key, newTable(value))
			} else {
				tbl.SetTableData(key, value)
			}
		}
	case []map[string]interface{}:
		// 支持嵌套数组
		for i, item := range v {
			tbl.SetTableData(i+1, newTable(item))
		}
		// 不再支持 *syncmap.Map 和 syncmap.Map 的直接复制
		// 使用 newTable 进行统一转换
	case interface{}:
		// 尝试通过 newTable 处理接口类型
		tbl2 := newTable(v)
		if tbl2.HaveData() {
			tbl.tbl = tbl2.tbl
		}
	default:
		// 尝试反射处理其他类型
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			length := rv.Len()
			for i := 0; i < length; i++ {
				elem := rv.Index(i).Interface()
				if isNestedStructure(elem) {
					tbl.AddListData(newTable(elem))
				} else {
					tbl.AddListData(elem)
				}
			}
		} else if rv.Kind() == reflect.Map {
			// 通用 map 处理
			iter := rv.MapRange()
			for iter.Next() {
				key := iter.Key().Interface()
				value := iter.Value().Interface()
				if isNestedStructure(value) {
					tbl.SetTableData(key, newTable(value))
				} else {
					tbl.SetTableData(key, value)
				}
			}
		}
	}
	return tbl
}

// isNestedStructure 检查是否是嵌套结构（map 或 slice）
func isNestedStructure(v interface{}) bool {
	if v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array
}

// getTable 从 Lua 栈读取 table 并转换为 LuaTable（支持嵌套）
func getTable(L *lua.State, idx int) *LuaTable {
	if L.IsTable(idx) {
		tbl := newTable(nil)
		L.PushNil()
		for L.Next(idx) != 0 {
			key := LuaToGoValue(L, -2)
			// 检查值是否是嵌套 table
			if L.IsTable(-1) {
				val := getTable(L, -1) // 递归处理嵌套 table
				tbl.SetTableData(key, val)
			} else {
				val := LuaToGoValue(L, -1)
				tbl.SetTableData(key, val)
			}
			L.Pop(1)
		}
		return tbl
	}
	return nil
}

type LuaTable struct {
	tbl *syncmap.Map
}

// HasData 检查 table 是否有数据
func (this *LuaTable) HasData() bool {
	return this.tbl != nil && this.tbl.Length() > 0
}

// HaveData 检查 table 是否有数据（兼容性别名）
func (this *LuaTable) HaveData() bool {
	return this.HasData()
}

// Append 添加列表元素
func (this *LuaTable) Append(val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.Set(this.tbl.Length()+1, val)
}

// AddListData 添加列表元素（兼容性别名）
func (this *LuaTable) AddListData(val interface{}) {
	this.Append(val)
}

// Set 设置键值对
func (this *LuaTable) Set(key, val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.tbl.Store(key, val)
}

// SetTableData 设置键值对（兼容性别名）
func (this *LuaTable) SetTableData(key, val interface{}) {
	this.Set(key, val)
}

// Push 将 table 压入 Lua 栈（支持嵌套）
func (this *LuaTable) PushTable(L *lua.State) bool {
	if !this.HasData() {
		return false
	}

	// 创建空表
	L.NewTable()

	// 遍历 map 填充表（自动处理嵌套 LuaTable）
	this.tbl.Range(func(key, value interface{}) bool {
		PushValue(L, key)

		// 检查值是否是嵌套 LuaTable
		if nestedTbl, ok := value.(*LuaTable); ok && nestedTbl != nil {
			nestedTbl.PushTable(L) // 递归压入嵌套 table
		} else {
			PushValue(L, value)
		}

		L.SetTable(-3)
		return true
	})

	return true
}

// SubTable 获取或创建嵌套 table
func (this *LuaTable) SubTable(key interface{}) *LuaTable {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	data, _ := this.tbl.LoadOrStore(key, newTable(nil))
	return data.(*LuaTable)
}

// AddTableData 获取或创建嵌套 table（兼容性别名）
func (this *LuaTable) AddTableData(key interface{}) *LuaTable {
	return this.SubTable(key)
}

// Length 返回 table 长度
func (this *LuaTable) Length() int {
	if this.tbl == nil {
		return 0
	}
	return this.tbl.Length()
}
