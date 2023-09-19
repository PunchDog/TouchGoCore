package gopherlua

import (
	"touchgocore/syncmap"

	lua "github.com/yuin/gopher-lua"
)

// 添加table用的临时函数,目前只支持几种类型,data只接受string和int64类型的列表或map
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
	if data != nil {
		//如果有数据传入，就给table写入数据
		switch data.(type) {
		case []string:
			l := data.([]string)
			for _, i2 := range l {
				tbl.AddListData(i2)
			}
		case []int64:
			l := data.([]int64)
			for _, i2 := range l {
				tbl.AddListData(i2)
			}
		case []float64:
			l := data.([]float64)
			for _, i2 := range l {
				tbl.AddListData(i2)
			}
		case []interface{}:
			l := data.([]interface{})
			for _, i2 := range l {
				tbl.AddListData(i2)
			}
		case map[interface{}]interface{}:
			l := data.(map[interface{}]interface{})
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[string]string:
			l := data.(map[string]string)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[string]map[string][]string:
			l := data.(map[string]map[string][]string)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[string][]string:
			l := data.(map[string][]string)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[string]int64:
			l := data.(map[string]int64)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[int64]int64:
			l := data.(map[int64]int64)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case map[int64]string:
			l := data.(map[int64]string)
			for i, i2 := range l {
				tbl.SetTableData(i, i2)
			}
		case *syncmap.Map:
			t := data.(*syncmap.Map)
			*tbl.tbl = *t
		case syncmap.Map:
			t := data.(syncmap.Map)
			*tbl.tbl = t
		}
	}

	return tbl
}

// 解析table数据
func GetTable(luaval lua.LValue, tbl *LuaTable) *LuaTable {
	if ltbl, ok := luaval.(*lua.LTable); ok {
		if tbl == nil {
			tbl = new(LuaTable)
		}
		ltbl.ForEach(func(l1, l2 lua.LValue) {
			if l2.Type() != lua.LTTable {
				tbl.SetTableData(pop(l1, 0), pop(l2, 0))
			} else {
				childtbl := tbl.AddTableData(pop(l1, 0))
				GetTable(l2, childtbl)
			}
		})
	}
	return tbl
}

type LuaTable struct {
	tbl *syncmap.Map
}

// 获取内部数据
func (this *LuaTable) Get() *syncmap.Map {
	return this.tbl
}

// 是否有数据
func (this *LuaTable) HaveData() bool {
	return this.tbl != nil && this.tbl.Length() > 0
}

// 给列表压数据
func (this *LuaTable) AddListData(val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.SetTableData(this.tbl.Length()+1, val)
}

// 给map压数据
func (this *LuaTable) SetTableData(key, val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.tbl.Store(key, val)
}

// 给lua压数据
func (this *LuaTable) PushTable(L *lua.LState) (tbl lua.LValue) {
	tbl = L.NewTable()
	//没数据，不能压表
	if !this.HaveData() {
		return
	}

	//压map
	if this.tbl != nil {
		this.tbl.Range(func(key, value interface{}) bool {
			L.SetTable(tbl, push(key, L), push(value, L))
			return true
		})
		return
	}
	return
}

// 新增一个luatable数据块
func (this *LuaTable) AddTableData(key interface{}) *LuaTable {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	data, _ := this.tbl.LoadOrStore(key, newTable(nil))
	return data.(*LuaTable)
}
