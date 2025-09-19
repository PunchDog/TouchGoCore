package rpc

import (
	"touchgocore/config"
	"touchgocore/vars"
)

const (
	MAX_MSG_SIZE = 1024 * 1024 * 10
)

func Run() {
	if config.Cfg_.RpcPort != nil {
		//启动服务器监听
		// 启动gRPC服务
		for _, v := range config.Cfg_.RpcPort.Server {
			StartGrpcServer(v.Name, v.Addr, v.Port)
		}

		//启动客户端连接
		for _, v := range config.Cfg_.RpcPort.Client {
			NewRpcClient(v.Name, v.Addr, v.Port)
		}
		vars.Info("gRPC服务启动成功")
	}
}

func Stop() {
	for _, v := range service_ {
		v.service.Stop()
	}

	for _, v := range rpcClient_ {
		v.conn.Close()
	}
}

func SendMsg() {

}
