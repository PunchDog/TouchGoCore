package main

import (
	"touchgocore/vars"
	"touchgocore/websocket"

	"google.golang.org/protobuf/proto"
)

// 这里实现消息分发机制需要的接口函数
type GateMsg struct {
}

func (this *GateMsg) OnConnect(client *websocket.Client) bool {
	vars.Info("GateMsg OnConnect")
	return true
}

func (this *GateMsg) OnMessage(client *websocket.Client, msg proto.Message) {
	vars.Info("GateMsg OnMessage")
}

func (this *GateMsg) OnClose(client *websocket.Client) {
	vars.Info("GateMsg OnClose")
}
