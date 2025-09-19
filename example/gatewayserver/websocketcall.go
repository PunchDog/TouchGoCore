package main

import "touchgocore/websocket"

//这里实现消息分发机制需要的接口函数
type Msg struct {
	websocket.ICall
}
