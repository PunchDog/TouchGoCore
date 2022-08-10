package main

import "touchgocore"

const (
	Name    = "GateWayServer"
	Version = "1.0"
)

//初始化一些数据
func init() {

}

func main() {
	//启动插件
	touchgocore.Run(Name, Version)
}
