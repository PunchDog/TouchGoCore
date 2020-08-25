package lua

import (
	"time"

	"github.com/PunchDog/TouchGoCore/touchgocore/vars"

	lua "github.com/yuin/gopher-lua"
)

type LuaScript struct {
	gScript           *lua.LState
	liRetList         *RetList
	path              string                       //脚本地址
	exports           *map[string]lua.LGFunction   //全局函数缓存
	exportsClass      *map[ILuaClassInterface]bool //导出类缓存
	closeLuaClearTick chan byte
}

//返回值结果
type RetList struct {
	liRetList []*lua.LValue
}

//删除start:end之间数据
func (this *RetList) Remove(indexstart int, indexend int) {
	len := len(this.liRetList)
	copy(this.liRetList[indexstart:], this.liRetList[indexend:])
	for k, n := len-indexend+indexstart, len; k < n; k++ {
		//or the zero value of T
		this.liRetList[k] = nil
	}
	this.liRetList = this.liRetList[:len-indexend+indexstart]
}

//清空
func (this *RetList) Clear() {
	this.liRetList = []*lua.LValue{}
}

//插入一个数
func (this *RetList) Push(data *lua.LValue) {
	this.liRetList = append(this.liRetList, data)
}

//获取一个数据
func (this *RetList) GetData(index int) *lua.LValue {
	if index >= len(this.liRetList) {
		return nil
	}
	return this.liRetList[index]
}

// func Loader(L *lua.LState) int {
// 	// register functions to the table
// 	mod := L.SetFuncs(L.NewTable(), exports)
// 	// register other stuff
// 	L.SetField(mod, "name", lua.LString("value"))
// 	// returns the module
// 	L.Push(mod)
// 	return 1
// }

//初始化lua文件
func (this *LuaScript) InitLua() {
	if this.gScript != nil {
		this.CloseLua()
	}

	this.gScript = lua.NewState()
	this.liRetList = new(RetList)

	//注册全局函数
	for funcname, function := range *this.exports {
		this.gScript.SetGlobal(funcname, this.gScript.NewFunction(function)) /* Original lua_setglobal uses stack... */
	}

	//注册类
	if this.exportsClass != nil {
		for class, _ := range *this.exportsClass {
			newLuaClass(class, this)
		}
	}
}

func (this *LuaScript) CloseLua() {
	this.closeLuaClearTick <- 1 //关闭定时器
	this.gScript.Close()
	this.gScript = nil
}

//读lua文件
func (this *LuaScript) DoFile(path string) error {
	if err := this.gScript.DoFile(path); err != nil {
		vars.Error(err)
		return err
	}

	vars.Info("lua load ok:", path)
	return nil
}

//获取的返回数据
func (this *LuaScript) GetRet(index int) *lua.LValue {
	return this.liRetList.GetData(index)
}

//call lua
func (this *LuaScript) Call(funcname string, list ...interface{}) bool {
	listlen := len(list)
	fn := lua.P{
		Fn:      this.gScript.GetGlobal(funcname),
		NRet:    lua.MultRet,
		Protect: true,
	}

	stackPos := this.gScript.GetTop()
	var err error = nil

	this.gScript.Push(fn.Fn)
	for _, val := range list {
		var arg lua.LValue = nil
		switch v := val.(type) {
		case int:
			arg = lua.LNumber(v)
		case int8:
			arg = lua.LNumber(v)
		case int16:
			arg = lua.LNumber(v)
		case int32:
			arg = lua.LNumber(v)
		case int64:
			arg = lua.LNumber(v)
		case string:
			arg = lua.LString(v)
		case bool:
			arg = lua.LBool(v)
		case []int:
			tbl := this.gScript.NewTable()
			for idx, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(idx), lua.LNumber(val))
			}
			arg = tbl
		case []string:
			tbl := this.gScript.NewTable()
			for idx, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(idx), lua.LString(val))
			}
			arg = tbl
		case map[int]string:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(key), lua.LString(val))
			}
			arg = tbl
		case map[string]string:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LString(key), lua.LString(val))
			}
			arg = tbl
		case map[string]int:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LString(key), lua.LNumber(val))
			}
			arg = tbl
		case map[int]int:
			tbl := this.gScript.NewTable()
			for key, val := range v {
				this.gScript.SetTable(tbl, lua.LNumber(key), lua.LNumber(val))
			}
			arg = tbl
		}
		this.gScript.Push(arg)
	}

	if fn.Protect {
		err = this.gScript.PCall(listlen, fn.NRet, fn.Handler)
	}
	if err != nil {
		vars.Error("lua调用出错:", err)
		return false
	}

	nNum := this.gScript.GetTop() - stackPos
	//获取返回数据
	this.liRetList.Clear()
	for i := 0; i < nNum; i++ {
		var data lua.LValue = this.gScript.Get(-1) // returned value
		this.gScript.Pop(1)                        // remove received value
		this.liRetList.Push(&data)
	}
	return true
}

//注册lua功能
func (this *LuaScript) LoadLua(path string) {
	//初始化指针
	this.path = path
	this.InitLua()

	//读取脚本文件
	if err := this.DoFile(path); err != nil {
		panic(err)
	}

	//创建定时器,定时垃圾回收
	go func() {
		for {
			select {
			case <-time.After(time.Minute * 30):
				this.Call("collectgarbage", lua.LString("collect"))
			case <-this.closeLuaClearTick:
				break
			}
		}
	}()
}

//重载脚本
func (this *LuaScript) ReLoadLua() {
	this.CloseLua()
	this.LoadLua(this.path)
}
