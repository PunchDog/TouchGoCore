package main

import "touchgocore"

const (
	Name = "GateWayServer"
)

// 初始化一些数据
func init() {

}

func main() {
	//启动插件
	touchgocore.Run(Name)
}
