package rpc

import "google.golang.org/protobuf/proto"

// ==================== Client 回调接口 ====================

// ClientCallbacks RPC客户端回调接口集合
type ClientCallbacks struct {
	// OnConnected 连接成功时触发
	OnConnected func(clientName string)
	// OnDisconnected 连接断开时触发
	OnDisconnected func(clientName string, err error)
	// OnReconnecting 正在重连时触发
	OnReconnecting func(clientName string, attempt int)
	// OnMessageSent 消息发送成功后触发（protocol1, protocol2, 请求数据）
	OnMessageSent func(clientName string, protocol1, protocol2 int32, req proto.Message)
	// OnMessageReceived 收到响应消息时触发（protocol1, protocol2, 响应数据）
	OnMessageReceived func(clientName string, protocol1, protocol2 int32, resp proto.Message)
	// OnError 错误发生时触发
	OnError func(clientName string, err error)
}

// NewClientCallbacks 创建客户端回调接口实例
func NewClientCallbacks() *ClientCallbacks {
	return &ClientCallbacks{}
}

// ==================== Server 回调接口 ====================

// ServerCallbacks RPC服务端回调接口集合
type ServerCallbacks struct {
	// OnServerStarted 服务启动完成时触发
	OnServerStarted func(serverName string)
	// OnServerStopped 服务停止时触发
	OnServerStopped func(serverName string)
	// OnClientConnected 新客户端连接时触发
	OnClientConnected func(serverName string, clientName string)
	// OnClientDisconnected 客户端断开连接时触发
	OnClientDisconnected func(serverName string, clientName string)
	// OnMessageReceived 收到客户端消息时触发（返回 false 可拒绝处理）
	// 参数：serverName, clientName, protocol1, protocol2, 原始消息
	// 返回值：是否继续处理该消息
	OnMessageReceived func(serverName string, clientName string, protocol1, protocol2 int32, msg proto.Message) bool
	// OnMessageProcessed 消息处理完成后触发
	// 参数：serverName, clientName, protocol1, protocol2, 处理结果(可为nil), 是否成功
	OnMessageProcessed func(serverName string, clientName string, protocol1, protocol2 int32, result proto.Message, success bool)
	// OnError 错误发生时触发
	OnError func(serverName string, err error)
	// OnSendResponse 向客户端发送响应前触发（可修改或阻止发送）
	// 返回值：是否继续发送
	OnSendResponse func(serverName string, clientName string, protocol1, protocol2 int32, resp proto.Message) bool
}

// NewServerCallbacks 创建服务端回调接口实例
func NewServerCallbacks() *ServerCallbacks {
	return &ServerCallbacks{}
}
