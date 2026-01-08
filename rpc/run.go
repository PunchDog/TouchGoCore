package rpc

import (
	"touchgocore/config"
	"touchgocore/vars"

	"google.golang.org/grpc"
)

const (
	MAX_MSG_SIZE     = 1024 * 1024 * 10
	MAX_CHANNEL_SIZE = 100000 // 减少通道容量以降低内存占用
)

func Run() {
	if config.Cfg_.RpcPort == nil {
		vars.Info("RPC配置为空，跳过RPC服务启动")
		return
	}
	cfg := config.Cfg_.RpcPort
	serverCount := len(cfg.Server)
	clientCount := len(cfg.Client)
	vars.Info("开始启动RPC服务: 服务器%d个, 客户端%d个", serverCount, clientCount)

	// 启动服务器监听
	for _, v := range cfg.Server {
		if v.Name == "" || v.Addr == "" || v.Port <= 0 {
			vars.Error("RPC服务器配置无效: Name=%s, Addr=%s, Port=%d", v.Name, v.Addr, v.Port)
			continue
		}
		StartGrpcServer(v.Name, v.Addr, v.Port)
	}

	// 启动客户端连接
	clientSuccess := 0
	for _, v := range cfg.Client {
		if v.Name == "" || v.Addr == "" || v.Port <= 0 {
			vars.Error("RPC客户端配置无效: Name=%s, Addr=%s, Port=%d", v.Name, v.Addr, v.Port)
			continue
		}
		if client := NewRpcClient(v.Name, v.Addr, v.Port); client != nil {
			clientSuccess++
		}
	}
	vars.Info("RPC服务启动完成: 服务器%d个, 客户端%d个 (成功连接%d个)", serverCount, clientCount, clientSuccess)
}

func Stop() {
	// 停止所有RPC服务器
	serverCount := 0
	service_.Range(func(key, value any) bool {
		v1 := value.(*RpcServer)
		v1.Stop()
		serverCount++
		return true
	})

	// 关闭所有RPC客户端连接
	clientCount := 0
	rpcClient_.Range(func(key, value any) bool {
		v1 := value.(*RpcClient)
		if connVal := v1.conn.Load(); connVal != nil {
			if conn, ok := connVal.(*grpc.ClientConn); ok && conn != nil {
				conn.Close()
				clientCount++
			}
		}
		return true
	})
	vars.Info("RPC服务停止: 服务器%d个, 客户端%d个", serverCount, clientCount)
}
