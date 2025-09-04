package rpc

import (
	"strings"
	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
)

const (
	MAX_MSG_SIZE = 1024 * 1024 * 10
)

func Run() {
	service_ = make([]*grpc.Server, 0)
	if config.Cfg_.RpcPort != nil {
		if config.Cfg_.RpcPort.Port != nil {
			portlist := util.String2NumberArray[int](*config.Cfg_.RpcPort.Port, "|")
			// 启动gRPC服务
			for _, port := range portlist {
				StartGrpcServer(port)
			}
		}

		//启动客户端连接
		if config.Cfg_.RpcPort.ClientAddr != nil {
			// 初始化客户端存根
			addrs := strings.Split(*config.Cfg_.RpcPort.ClientAddr, "||")
			for _, addr := range addrs {
				d := strings.Split(addr, "|")
				if len(d) == 2 {
					servername := d[0]
					addr1 := d[1]
					NewRpcClient(servername, addr1)
				}
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
