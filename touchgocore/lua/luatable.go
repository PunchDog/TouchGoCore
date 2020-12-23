package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"github.com/aarzilli/golua/lua"
)

//添加table用的临时函数,目前只支持几种类型,data只接受string和int64类型的列表或map
func newTable(data interface{}) *LuaTable {
	tbl := &LuaTable{}
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
	return tbl
}

//解析table数据
func getTable(L *lua.State, idx int) *LuaTable {
	if L.IsTable(idx) {
		tbl := newTable(nil)
		L.PushNil()
		for L.Next(idx) != 0 {
			key := pop(L, -2)
			val := pop(L, -1)
			tbl.SetTableData(key, val)
			L.Pop(1)
		}
		return tbl
	}
	return nil
}

type LuaTable struct {
	tbl *syncmap.Map
}

//是否有数据
func (this *LuaTable) HaveData() bool {
	return this.tbl != nil && this.tbl.Length() > 0
}

//给列表压数据
func (this *LuaTable) AddListData(val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.SetTableData(this.tbl.Length()+1, val)
}

//给map压数据
func (this *LuaTable) SetTableData(key, val interface{}) {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	this.tbl.Store(key, val)
}

//给lua压数据
func (this *LuaTable) PushTable(L *lua.State) bool {
	//没数据，不能压表
	if !this.HaveData() {
		return false
	}

	//压表函数
	push1 := func(L *lua.State, i, i2 interface{}) {
		push(L, i)
		push(L, i2)
		L.SetTable(-3)
	}

	//压map
	if this.tbl != nil {
		L.NewTable()
		this.tbl.Range(func(key, value interface{}) bool {
			push1(L, key, value)
			return true
		})
		return true
	}
	return false
}

//新增一个luatable数据块
func (this *LuaTable) AddTableData(key interface{}) *LuaTable {
	if this.tbl == nil {
		this.tbl = &syncmap.Map{}
	}
	data, _ := this.tbl.LoadOrStore(key, newTable(nil))
	return data.(*LuaTable)
}
