package main

import (
	"touchgocore/example/gatewayserver/gatepb"
	"touchgocore/vars"
	"touchgocore/websocket"

	"google.golang.org/protobuf/proto"
)

type GateMsg struct {
}

func (this *GateMsg) OnConnect(client *websocket.Client) bool {
	vars.Info("GateMsg OnConnect")
	return true
}

func (this *GateMsg) OnMessage(client *websocket.Client, msg proto.Message) {
	switch m := msg.(type) {
	case *gatepb.GatePing:
		vars.Info("GatePing payload=%s", m.GetPayload())
		client.SendMsg(gatepb.ProtocolPong1, gatepb.ProtocolPong2, &gatepb.GatePong{Payload: "pong:" + m.GetPayload()})
	case *gatepb.GatePong:
		vars.Info("GatePong payload=%s", m.GetPayload())
	default:
		vars.Info("GateMsg OnMessage type=%T", msg)
	}
}

func (this *GateMsg) OnClose(client *websocket.Client) {
	vars.Info("GateMsg OnClose")
}
