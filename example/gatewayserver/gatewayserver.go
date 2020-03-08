package main

import (
	"github.com/PunchDog/TouchGoCore/touchgocore"
)

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

	////测试发送
	//ret := &rpc.RetBuffer{}
	//if _, err := rpc.SendMsg(0, 1, 1, 1, ret); err == nil {
	//	str := ret.RetData.(string)
	//	vars.Info("通信消息成功！返回数据：%s", str)
	//} else {
	//	vars.Error("消息手法出错：", err)
	//}
	chsig := make(chan byte)
	<-chsig
}
