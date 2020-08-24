package lua

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	lua "github.com/yuin/gopher-lua"
)

//注册函数列表
func RegisterLuaFunc(funcname string, function lua.LGFunction) bool {
	if (*defaultScript.exports)[funcname] != nil {
		return false
	}
	(*defaultScript.exports)[funcname] = function
	return true
}

//注册一个类到默认lua
func RegisterLuaClass(class interface{}) bool {
	//初始化一个类初始化
	if defaultScript.exportsClass == nil {
		defaultScript.exportsClass = &map[interface{}]bool{}
	}
	if (*defaultScript.exportsClass)[class] {
		return false
	}
	(*defaultScript.exportsClass)[class] = true
	return true
}

//创建一个DIY类型的脚本
func NewLuaScript(exports *map[string]lua.LGFunction, class *map[interface{}]bool) *LuaScript {
	script := newScript()
	if class != nil {
		script.exportsClass = class
	}
	if exports != nil {
		if exports != nil {
			for k, v := range *script.exports {
				(*exports)[k] = v
			}
			script.exports = exports
		}
	}
	// script.LoadLua(config.Cfg_.Lua)
	script.LoadLua("./test.lua")
	return script
}

//启动lua
func Run() {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动lua服务")
		return
	}

	defaultScript.LoadLua(config.Cfg_.Lua)
	vars.Info("启动lua服务成功")
}
