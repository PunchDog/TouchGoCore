package main

import (
	"touchgocore"
	"touchgocore/vars"
	"touchgocore/websocket"
)

func main() {
	client, err := websocket.NewClient("ws://127.0.0.1:8000/ws", "loginserver", "GateMsg")
	if err != nil {
		vars.Error("创建client失败", err)
		return
	}
	if !client.OnConnect(client) {
		vars.Error("连接失败", err)
		return
	}
	touchgocore.Run("loginserver")
}
