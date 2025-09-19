package main

import (
	"touchgocore"
	"touchgocore/websocket"
)

const (
	Name = "GateWayServer"
)

// 初始化一些数据
func init() {
}

func main() {
	websocket.RegisterCall(&Msg{})
	//启动插件
	touchgocore.Run(Name)
}
