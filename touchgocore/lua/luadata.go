package lua

import (
	"strconv"

	"github.com/PunchDog/TouchGoCore/touchgocore/util"

	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	lua "github.com/yuin/gopher-lua"
)

//默认的lua指针
var defaultScript *LuaScript = newScript()

//lua产生的类数据
var defaultLuaDataUid int64 = 0
var defaultLuaData *syncmap.Map = &syncmap.Map{}

func newScript() *LuaScript {
	return &LuaScript{
		exports: &map[string]lua.LGFunction{
			"info":           info,           //打印
			"debug":          debug,          //打印
			"error":          error1,         //打印
			"dofile":         dofile,         //加载lua文件
			"getpathluafile": getpathluafile, //获取文件夹下所有文件名
		},
		exportsClass:      nil,
		closeLuaClearTick: make(chan byte, 1),
	}
}

//两个默认执行的函数
func info(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Info(retstr)
	return 0
}

func debug(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Debug(retstr)
	return 0
}

func error1(L *lua.LState) int {
	retstr := L.ToString(1)
	vars.Error(retstr)
	return 0
}

func dofile(L *lua.LState) int {
	// defer func() {
	// 	if err := recover(); err != nil {
	// 		log.Println("捕获错误:", err)
	// 	}
	// }()
	retstr := L.ToString(1)
	if err := L.DoFile(retstr); err != nil {
		L.Push(lua.LNumber(-1))
		L.Push(lua.LString(err.Error()))
	} else {
		L.Push(lua.LNumber(0))
		L.Push(lua.LString("ok"))
	}
	return 2
}

//获取路径下所有文件
func getpathluafile(L *lua.LState) int {
	path := L.ToString(1)
	pathlist := util.GetPathFile(path, []string{".lua"})

	//加载所有地图
	tbl := L.NewTable()
	for idx, filepath := range pathlist {
		L.SetField(tbl, strconv.FormatInt(int64(idx), 10), lua.LString(filepath))
	}
	L.Push(tbl)
	return 1
}

//lua加载的数据存储结构(设计缺陷....)
type LuaDataObj struct {
	map_ syncmap.Map //lua初始化存储的数据
}

//设置一条数据
func (f *LuaDataObj) SetValue(k string, v int64) {
	f.map_.Store(k, v)
}

//查询一条数据
func (f *LuaDataObj) GetValue(k string) int64 {
	if d, ok := f.map_.Load(k); ok {
		return d.(int64)
	}
	return 0
}

//设置一条数据
func (f *LuaDataObj) SetString(k string, v string) {
	f.map_.Store(k, v)
}

//查询一条数据
func (f *LuaDataObj) GetString(k string) string {
	if d, ok := f.map_.Load(k); ok {
		return d.(string)
	}
	return ""
}

//设置一条数据
func (f *LuaDataObj) SetTable(k string, v *syncmap.Map) {
	vl := &syncmap.Map{}
	if v != nil {
		*vl = *v
		f.map_.Store(k, vl)
	}
}

//查询一条数据
func (f *LuaDataObj) GetTable(k string) *syncmap.Map {
	if d, ok := f.map_.Load(k); ok {
		tbl := d.(*syncmap.Map)
		return tbl
	}
	return nil
}
