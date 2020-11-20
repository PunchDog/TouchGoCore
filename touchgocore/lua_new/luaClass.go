package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	"github.com/aarzilli/golua/lua"
	"reflect"
)

var _defaultLuaData *syncmap.Map = &syncmap.Map{}
var _defaultLuaDataUid int64

//注册类接口
type ILuaClassInterface interface {
	AddField(id int64) interface{}
	Delete()
}

//删除数据函数
func delete(L *lua.State) int {
	vp := L.ToUserdata(1)
	id := (*int64)(vp)
	if d, ok := _defaultLuaData.Load(int64(*id)); ok {
		fn := d.(ILuaClassInterface)
		fn.Delete()
		_defaultLuaData.Delete(int64(*id))
	}
	*id = 0
	return 0
}

//获取UID函数
func index(L *lua.State) int {
	vp := L.ToUserdata(1)
	id := (*int64)(vp)
	L.PushInteger(int64(*id))
	return 1
}

//创建回调函数
type metaOperate struct {
	methodname string
}

//函数默认interface{}类型的number识别为int64,返回值是table的话，目前只支持*syncmap.Map,并且key目前只能是string
func (this *metaOperate) callBack(L *lua.State) int {
	L.CheckType(1, lua.LUA_TUSERDATA)
	//获取数据
	vp := L.ToUserdata(1)
	datauid := (*int64)(vp)
	var data interface{} = nil
	if d, ok := _defaultLuaData.Load(int64(*datauid)); ok {
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
			args = append(args, reflect.ValueOf(pop(L, i)))
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
			if !push(L, iresData.Interface()) {
				vars.Error("处理lua返回数据出错")
				return 0
			}
		}
	}
	return rescnt
}

//创建一个类注册
func newLuaClass(class ILuaClassInterface, script *LuaScript) {
	//创建函数类(暂时不支持interface{}类型参数和动态参数)
	//获取类和函数名
	sname, mnames := util.GetClassName(class)
	//创建lua内的类table
	if !script.l.NewMetaTable(sname) {
		return
	}

	//设置清理回调
	script.l.SetMetaMethod("__gc", delete)
	//设置索引回调
	script.l.SetMetaMethod("__index", index)

	//循环创建每个函数对应的实现
	script.l.NewTable()
	for _, methodname := range mnames {
		//创建一个new函数
		fnclass := &metaOperate{
			methodname: methodname,
		}
		script.l.PushGoFunction(fnclass.callBack)
		script.l.SetField(-2, methodname)
	}
	script.l.SetField(-2, "__index")
	script.l.Pop(1)

	//注册创建函数
	script.l.Register(sname+":new", func(L *lua.State) int {
		_defaultLuaDataUid++
		new := class.AddField(_defaultLuaDataUid)
		_defaultLuaData.Store(_defaultLuaDataUid, new)
		p := L.NewUserdata(8)
		p1 := (*int64)(p)
		*p1 = _defaultLuaDataUid
		return 1
	})
}
