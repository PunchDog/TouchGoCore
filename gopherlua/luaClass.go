package gopherlua

import (
	"reflect"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	lua "github.com/yuin/gopher-lua"
)

// lua产生的类数据
var defaultLuaDataUid int64 = 0
var defaultLuaData *syncmap.Map = &syncmap.Map{}

// 注册类接口
type ILuaClassInterface interface {
	AddField(id int64, lua *LuaScript) interface{}
	Delete()
	Update()
}

type ILuaClassObject struct {
	lua *LuaScript
}

func (self *ILuaClassObject) Delete() {

}

func (self *ILuaClassObject) Update() {

}

func (self *ILuaClassObject) SetLuaScript(lua *LuaScript) {
	self.lua = lua
}

// 创建一个多线程调用函数，处理lua脚本创建的多线程
func (this *ILuaClassObject) MultithreadingCall(funcname string, list ...interface{}) *LuaTable {
	ch := make(chan *LuaTable, 0)
	go func() {
		if this.lua.Call(funcname, list...) {
			ret := this.lua.Ret()
			if len(ret) == 1 {
				ch <- GetTable(*ret[0], nil)
			}
		}
	}()
	return <-ch
}

// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////
// ////////////////////////////////////////////////////////////////////////////////////////////////////
// 注册时存放查询数据的
type metaUserData struct {
	uid    int64
	script *LuaScript
}

// 创建回调函数
type metaOperate struct {
	methodname string
}

// 函数默认interface{}类型的number识别为int64,返回值是table的话，目前只支持*syncmap.Map,并且key目前只能是string
func (this *metaOperate) callBack(L *lua.LState) int {
	//获取数据
	self := L.CheckTable(1)
	datauid := L.GetField(self, "datauid")
	var data interface{} = nil
	if d, ok := defaultLuaData.Load(int64(datauid.(lua.LNumber))); ok {
		data = d
	} else {
		return 0
	}

	//获取类数据
	rcvr := reflect.ValueOf(data)
	//获取函数反射
	method := rcvr.MethodByName(this.methodname)
	//lua输入参数
	args := []reflect.Value{} //获取的形参
	NumIn := method.Type().NumIn()
	getTop := L.GetTop() //这个参数索引从2开始
	if getTop >= 2 {
		z := 0
		for i := 2; i <= getTop; i++ {
			luaval := L.Get(i)
			kind := reflect.Invalid
			if luaval.Type() == lua.LTNumber {
				arg := method.Type().In(z)
				kind = arg.Kind()
			}
			args = append(args, reflect.ValueOf(pop(luaval, kind)))
			z++
			if z >= NumIn {
				break
			}
		}
	}

	//调用原始函数
	resultValues := method.Call(args)

	//填写返回值
	rescnt := len(resultValues)
	if rescnt > 0 {
		//调用函数后返回参数
		for _, iresData := range resultValues {
			L.Push(push(iresData.Interface(), L))
		}
	}
	return rescnt
}

// 创建一个类注册
func newLuaClass(class ILuaClassInterface, script *LuaScript) {
	//创建函数类(暂时不支持interface{}类型参数和动态参数)
	//获取类和函数名
	sname, mnames := util.GetClassName(class)

	//创建lua内的类table
	meta := script.l.NewTable()
	script.l.SetGlobal(sname, meta)

	//创建一个new函数
	script.l.SetField(meta, "new", script.l.NewFunction(func(L *lua.LState) int {
		uid := L.ToInt64(2)
		defaultLuaDataUid++
		defaultLuaData.LoadOrStore(defaultLuaDataUid, class.AddField(uid, script)) //尝试插入一份数据
		c := L.NewTable()
		self := L.CheckTable(1)
		L.SetMetatable(c, self)
		L.SetField(self, "__index", self)
		L.SetField(self, "datauid", lua.LNumber(defaultLuaDataUid))
		L.Push(c)
		return 1
	}))
	//获取一个数据函数
	script.l.SetField(meta, "get", script.l.NewFunction(func(L *lua.LState) int {
		//这个参数索引从2开始
		if L.GetTop() == 2 {
			uid := L.ToInt64(2)
			if _, ok := defaultLuaData.Load(uid); !ok {
				return 0
			}
			c := L.NewTable()
			self := L.CheckTable(1)
			L.SetMetatable(c, self)
			L.SetField(self, "__index", self)
			L.SetField(self, "datauid", lua.LNumber(uid))
			L.Push(c)
			return 1
		}
		return 0
	}))
	//删除数据函数
	script.l.SetField(meta, "destory", script.l.NewFunction(func(L *lua.LState) int {
		self := L.CheckTable(1)
		datauid := L.GetField(self, "datauid")
		uid := int64(datauid.(lua.LNumber))
		var classfn ILuaClassInterface = nil
		if d, ok := defaultLuaData.Load(uid); ok {
			classfn = d.(ILuaClassInterface)
			classfn.Delete()
			defaultLuaData.Delete(uid)
			return 0
		}
		return 0
	}))

	//循环创建每个函数对应的实现
	for _, methodname := range mnames {
		//创建一个new函数
		fnclass := &metaOperate{
			methodname: methodname,
		}
		script.l.SetField(meta, methodname, script.l.NewFunction(fnclass.callBack))
	}

	vars.Info("注册lua类:%s成功!", sname)
}
