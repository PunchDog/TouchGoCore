package lua

import (
	"touchgocore/config"
	"touchgocore/vars"
)

// 启动函数
func Run() {
	if config.Cfg_.Lua == "off" {
		vars.Info("不启动lua服务")
		return
	}

	RunLua()
}

// 关闭所有的定时器
func Stop() {
	StopLua()
}
