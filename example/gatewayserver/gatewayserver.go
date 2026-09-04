package main

import (
	"os"

	"touchgocore"
	"touchgocore/example/gatewayserver/gatepb"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"
)

const (
	Name = "GateWayServer"
)

func init() {
	websocket.RegisterCall("GateMsg", &GateMsg{})
	websocket.SetAuthFunc(gatewayAuth)
}

func gatewayAuth(token, remoteAddr string) bool {
	expected := os.Getenv("TOUCHGO_WS_TOKEN")
	if expected == "" {
		if os.Getenv("TOUCHGO_ENV") == "production" {
			vars.Error("生产环境必须设置 TOUCHGO_WS_TOKEN")
			return false
		}
		expected = "dev-token"
		vars.Warning("TOUCHGO_WS_TOKEN 未设置，示例网关使用 dev-token（仅本地调试）")
	}
	if token == "" || token != expected {
		vars.Warning("WebSocket 认证失败, IP=%s", remoteAddr)
		return false
	}
	return true
}

func main() {
	util.RegisterProtocolTypes(
		util.ProtocolBinding{Protocol1: gatepb.ProtocolPing1, Protocol2: gatepb.ProtocolPing2, Message: &gatepb.GatePing{}},
		util.ProtocolBinding{Protocol1: gatepb.ProtocolPong1, Protocol2: gatepb.ProtocolPong2, Message: &gatepb.GatePong{}},
	)
	touchgocore.Run(Name)
}
