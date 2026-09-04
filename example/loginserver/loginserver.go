package main

import (
	"os"

	"touchgocore"
	"touchgocore/example/gatewayserver/gatepb"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"

	"google.golang.org/protobuf/proto"
)

func main() {
	util.RegisterProtocolTypes(
		util.ProtocolBinding{Protocol1: gatepb.ProtocolPing1, Protocol2: gatepb.ProtocolPing2, Message: &gatepb.GatePing{}},
		util.ProtocolBinding{Protocol1: gatepb.ProtocolPong1, Protocol2: gatepb.ProtocolPong2, Message: &gatepb.GatePong{}},
	)
	websocket.RegisterCall("GateMsg", &loginGateMsg{})

	token := os.Getenv("TOUCHGO_WS_TOKEN")
	if token == "" {
		token = "dev-token"
	}
	client, err := websocket.NewClient("ws://127.0.0.1:8000/ws?token="+token, "loginserver", "GateMsg")
	if err != nil {
		vars.Error("创建client失败: %v", err)
		return
	}
	client.SendMsg(gatepb.ProtocolPing1, gatepb.ProtocolPing2, &gatepb.GatePing{Payload: "hello"})
	touchgocore.Run("loginserver")
}

type loginGateMsg struct{}

func (loginGateMsg) OnConnect(client *websocket.Client) bool {
	vars.Info("loginserver connected")
	return true
}

func (loginGateMsg) OnMessage(client *websocket.Client, msg proto.Message) {
	if pong, ok := msg.(*gatepb.GatePong); ok {
		vars.Info("loginserver got GatePong payload=%s", pong.GetPayload())
		return
	}
	vars.Info("loginserver OnMessage type=%T", msg)
}

func (loginGateMsg) OnClose(client *websocket.Client) {
	vars.Info("loginserver disconnected")
}
