package rpc

import (
	"touchgocore/config"
	"touchgocore/vars"

	"google.golang.org/grpc"
)

const (
	MAX_MSG_SIZE = 1024 * 1024 * 10
)

func Run() {
	service_ = make([]*grpc.Server, 0)
	if config.Cfg_.RpcPort != nil {
		//启动服务器监听
		if len(config.Cfg_.RpcPort.Port) > 0 {
			// 启动gRPC服务
			for _, port := range config.Cfg_.RpcPort.Port {
				StartGrpcServer(port)
			}
		}

		//启动客户端连接
		if len(config.Cfg_.RpcPort.ClientAddr) > 0 {
			// 初始化客户端连接
			for _, v := range config.Cfg_.RpcPort.ClientAddr {
				NewRpcClient(v.ServerName, v.Addr)
			}
		}
		vars.Info("gRPC服务启动成功")
	}
}

func Stop() {
	for _, v := range service_ {
		v.Stop()
	}

	for _, v := range rpcClient_ {
		v.conn.Close()
	}
}
