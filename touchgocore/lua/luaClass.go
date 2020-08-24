package lua

import (
	"reflect"

	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	lua "github.com/yuin/gopher-lua"
)

//注册用的
type luaClass struct {
}

//创建一个类注册
func newLuaClass(class interface{}, script *LuaScript) {
	var luaClass *luaClass = &luaClass{}
	luaClass.create(class, script)
}

//创建函数类(暂时不支持interface{}类型参数和动态参数)
func (this *luaClass) create(class interface{}, script *LuaScript) {
	//获取类数据
	rcvr := reflect.ValueOf(class)

	//获取类和函数名
	sname, mnames := util.GetClassName(class)

	//创建lua内的类table
	meta := script.gScript.NewTable()
	script.gScript.SetGlobal(sname, meta)

	//创建一个new函数
	script.gScript.SetField(meta, "new", script.gScript.NewFunction(func(L *lua.LState) int {
		c := L.NewTable()
		self := L.CheckTable(1)
		L.SetMetatable(c, self)
		L.SetField(self, "__index", self)
		L.Push(c)
		return 1
	}))

	//循环创建每个函数对应的实现
	for _, methodname := range mnames {
		//创建一个new函数
		script.gScript.SetField(meta, methodname, script.gScript.NewFunction(func(L *lua.LState) int {
			method := rcvr.MethodByName(methodname)
			//lua输入参数
			args := []reflect.Value{} //获取的形参
			NumIn := method.Type().NumIn()
			getTop := L.GetTop() //这个参数索引从2开始
			if NumIn > 0 && getTop >= 2 {
				z := 0
				for i := getTop; i < NumIn+getTop; i++ {
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
					case reflect.String:
						args = append(args, reflect.ValueOf(string(L.ToString(i))))
					case reflect.Bool:
						args = append(args, reflect.ValueOf(bool(L.ToBool(i))))
					}
					z++
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
		}))
	}
}
