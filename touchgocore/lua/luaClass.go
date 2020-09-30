package lua

import (
	"reflect"

	"github.com/PunchDog/TouchGoCore/touchgocore/vars"

	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	lua "github.com/yuin/gopher-lua"
)

type ILuaClassInterface interface {
	AddField(id int64) interface{}
}

type funcToName struct {
	name string
}

//函数默认interface{}类型的number识别为int64
func (this *funcToName) callBack(L *lua.LState) int {
	self := L.CheckTable(1)

	//获取数据
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
	method := rcvr.MethodByName(this.name)
	//lua输入参数
	args := []reflect.Value{} //获取的形参
	NumIn := method.Type().NumIn()
	getTop := L.GetTop() //这个参数索引从2开始
	if getTop >= 2 {
		z := 0
		for i := 2; i <= getTop; i++ {
			luaval := L.Get(i)
			switch luaval.Type() {
			case lua.LTString:
				arg := string(lua.LVAsString(luaval))
				args = append(args, reflect.ValueOf(arg))
			case lua.LTNumber:
				arg := method.Type().In(z)
				switch arg.Kind() {
				case reflect.Int:
					args = append(args, reflect.ValueOf(int(L.ToInt(i))))
				case reflect.Int8:
					args = append(args, reflect.ValueOf(int8(L.ToInt(i))))
				case reflect.Int16:
					args = append(args, reflect.ValueOf(int16(L.ToInt(i))))
				case reflect.Int32:
					args = append(args, reflect.ValueOf(int32(L.ToInt(i))))
				case reflect.Uint:
					args = append(args, reflect.ValueOf(uint(L.ToInt(i))))
				case reflect.Uint8:
					args = append(args, reflect.ValueOf(uint8(L.ToInt(i))))
				case reflect.Uint16:
					args = append(args, reflect.ValueOf(uint16(L.ToInt(i))))
				case reflect.Uint32:
					args = append(args, reflect.ValueOf(uint32(L.ToInt(i))))
				case reflect.Int64:
					args = append(args, reflect.ValueOf(int64(L.ToInt64(i))))
				case reflect.Uint64:
					args = append(args, reflect.ValueOf(uint64(L.ToInt64(i))))
				case reflect.Float32:
					args = append(args, reflect.ValueOf(float32(L.ToNumber(i))))
				case reflect.Float64:
					args = append(args, reflect.ValueOf(float64(L.ToNumber(i))))
				case reflect.Interface:
					args = append(args, reflect.ValueOf(float64(L.ToNumber(i))))
				}
			case lua.LTBool:
				arg := bool(lua.LVAsBool(luaval))
				args = append(args, reflect.ValueOf(arg))
			case lua.LTTable:
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
			switch iresData.Type().Kind() {
			case reflect.Int:
				L.Push(lua.LNumber(iresData.Interface().(int)))
			case reflect.Int8:
				L.Push(lua.LNumber(iresData.Interface().(int8)))
			case reflect.Int16:
				L.Push(lua.LNumber(iresData.Interface().(int16)))
			case reflect.Int32:
				L.Push(lua.LNumber(iresData.Interface().(int32)))
			case reflect.Uint:
				L.Push(lua.LNumber(iresData.Interface().(uint)))
			case reflect.Uint8:
				L.Push(lua.LNumber(iresData.Interface().(uint8)))
			case reflect.Uint16:
				L.Push(lua.LNumber(iresData.Interface().(uint16)))
			case reflect.Uint32:
				L.Push(lua.LNumber(iresData.Interface().(uint32)))
			case reflect.Int64:
				L.Push(lua.LNumber(iresData.Interface().(int64)))
			case reflect.Uint64:
				L.Push(lua.LNumber(iresData.Interface().(uint64)))
			case reflect.Float32:
				L.Push(lua.LNumber(iresData.Interface().(float32)))
			case reflect.Float64:
				L.Push(lua.LNumber(iresData.Interface().(float64)))
			case reflect.String:
				L.Push(lua.LString(iresData.Interface().(string)))
			case reflect.Bool:
				L.Push(lua.LBool(iresData.Interface().(bool)))
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
	meta := script.gScript.NewTable()
	script.gScript.SetGlobal(sname, meta)

	//创建一个new函数
	script.gScript.SetField(meta, "new", script.gScript.NewFunction(func(L *lua.LState) int {
		uid := L.ToInt64(2)
		defaultLuaDataUid++
		defaultLuaData.LoadOrStore(defaultLuaDataUid, class.AddField(uid)) //尝试插入一份数据
		c := L.NewTable()
		self := L.CheckTable(1)
		L.SetMetatable(c, self)
		L.SetField(self, "__index", self)
		L.SetField(self, "datauid", lua.LNumber(defaultLuaDataUid))
		L.Push(c)
		return 1
	}))
	//获取一个数据函数
	script.gScript.SetField(meta, "get", script.gScript.NewFunction(func(L *lua.LState) int {
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
	script.gScript.SetField(meta, "destory", script.gScript.NewFunction(func(L *lua.LState) int {
		self := L.CheckTable(1)
		datauid := L.GetField(self, "datauid")
		uid := int64(datauid.(lua.LNumber))
		if _, ok := defaultLuaData.Load(uid); !ok {
			return 0
		}
		defaultLuaData.Delete(uid)
		return 0
	}))

	//循环创建每个函数对应的实现
	for _, methodname := range mnames {
		//创建一个new函数
		fnclass := &funcToName{
			name: methodname,
		}
		script.gScript.SetField(meta, methodname, script.gScript.NewFunction(fnclass.callBack))
	}

	vars.Info("注册lua类:%s成功!", sname)
}
