package lua

import (
	"reflect"
	"touchgocore/util"
	"touchgocore/vars"
	"unsafe"

	"github.com/aarzilli/golua/lua"
)

//注册类接口
type ILuaClassInterface interface {
	AddField(id int64) interface{}
	Delete()
	Update()
}

//注册类接口基类
type ILuaClassObject struct {
}

//创建一个NPC容器，放入到NPC数据里
func (this *ILuaClassObject) AddField(id int64) interface{} {
	return nil
}

func (this *ILuaClassObject) Delete() {
}

func (this *ILuaClassObject) Update() {
}

//////////////////////////////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////////////////////////
//注册时存放查询数据的
type metaUserData struct {
	uid    int64
	script *LuaScript
}

//创建回调函数
type metaOperate struct {
	methodname string
}

//函数默认interface{}类型的number识别为int64,返回值是table的话，目前只支持*syncmap.Map,并且key目前只能是string
func (this *metaOperate) callBack(L *lua.State) int {
	//数据函数
	var pData (**metaUserData) = (**metaUserData)(L.ToUserdata(1))
	meta := *pData
	var data interface{} = nil
	if d, ok := meta.script.defaultLuaData.Load(meta.uid); ok {
		data = d.(ILuaClassInterface)
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
			//转换函数形参类型
			if L.Type(i) == lua.LUA_TNUMBER {
				arg := method.Type().In(z)
				val := L.ToNumber(i)
				switch arg.Kind() {
				case reflect.Int:
					args = append(args, reflect.ValueOf(int(val)))
				case reflect.Int8:
					args = append(args, reflect.ValueOf(int8(val)))
				case reflect.Int16:
					args = append(args, reflect.ValueOf(int16(val)))
				case reflect.Int32:
					args = append(args, reflect.ValueOf(int32(val)))
				case reflect.Uint:
					args = append(args, reflect.ValueOf(uint(val)))
				case reflect.Uint8:
					args = append(args, reflect.ValueOf(uint8(val)))
				case reflect.Uint16:
					args = append(args, reflect.ValueOf(uint16(val)))
				case reflect.Uint32:
					args = append(args, reflect.ValueOf(uint32(val)))
				case reflect.Int64:
					args = append(args, reflect.ValueOf(int64(val)))
				case reflect.Uint64:
					args = append(args, reflect.ValueOf(uint64(val)))
				case reflect.Float32:
					args = append(args, reflect.ValueOf(float32(val)))
				case reflect.Float64:
					args = append(args, reflect.ValueOf(float64(val)))
				case reflect.Interface:
					args = append(args, reflect.ValueOf(float64(val)))
				}
			} else {
				args = append(args, reflect.ValueOf(pop(L, i)))
			}
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
	script.defaultLuaDataUid++
	//创建函数类(暂时不支持interface{}类型参数和动态参数)
	//获取类和函数名
	sname, mnamelist := util.GetClassName(class)
	//创建类函数
	createclass := func(l *lua.State) int {
		script.defaultLuaData.Store(script.defaultLuaDataUid, class.AddField(script.defaultLuaDataUid))
		meta := &metaUserData{
			uid:    script.defaultLuaDataUid,
			script: script,
		}
		var pData **metaUserData = (**metaUserData)(script.l.NewUserdata(unsafe.Sizeof(meta)))
		*pData = meta
		script.l.LGetMetaTable(sname)
		script.l.SetMetaTable(-2)
		return 1
	}
	//删除函数
	destoryclass := func(l *lua.State) int {
		var p **metaUserData = (**metaUserData)(script.l.ToUserdata(1))
		meta := *p
		if d, ok := meta.script.defaultLuaData.Load(meta.uid); ok {
			data := d.(ILuaClassInterface)
			data.Delete()
			meta.script.defaultLuaData.Delete(meta.uid)
		}
		*p = nil
		return 0
	}

	//开始写创建
	script.l.PushGoFunction(createclass)
	script.l.SetGlobal(sname)

	script.l.NewMetaTable(sname)

	script.l.PushString("__gc")
	script.l.PushGoFunction(destoryclass)
	script.l.SetTable(-3)

	script.l.PushString("__index")
	script.l.PushValue(-2)
	script.l.SetTable(-3)

	//循环注册函数
	for _, mname := range mnamelist {
		script.l.PushString(mname)
		meta := &metaOperate{methodname: mname}
		script.l.PushGoFunction(meta.callBack)
		script.l.SetTable(-3)
	}

	script.l.Pop(1)
}
