package main

import (
	"os"

	"touchgocore"
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
	// 业务 protobuf 须在启动前注册：util.RegisterProtocolType(p1, p2, &YourMsg{})
	// 未注册的 (protocol1, protocol2) 会被拒绝解析。
	touchgocore.Run(Name)
}
