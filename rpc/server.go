package rpc

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"touchgocore/config"
	"touchgocore/network/message"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var (
	service_ []*grpc.Server = nil
)

type RpcServer struct {
	message.UnimplementedGrpcServer
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			vars.Error(fmt.Sprintf("接收gRPC消息错误: %v", err))
			return err
		}

		// 处理请求逻辑
		go func(msg *message.FSMessage, stream message.Grpc_MsgServer) {
			// 处理请求逻辑
			req := util.PasreFSMessage(req)
			util.DefaultCallFunc.SetDoRet()
			key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.GetHead().GetProtocol1(), msg.GetHead().GetProtocol2())
			bret := util.DefaultCallFunc.Do(key, req)
			res := util.DefaultCallFunc.GetRet()
			if bret {
				rsp := util.NewFSMessage(msg.GetHead().GetProtocol1(), msg.GetHead().GetProtocol2(), res[0].Interface().(proto.Message))
				if err := stream.Send(rsp); err != nil {
					vars.Error("发送gRPC响应错误: %v", err)
				}
			} else {
				vars.Error("处理gRPC请求错误,协议号:%d:%d", msg.GetHead().GetProtocol1(), msg.GetHead().GetProtocol2())
			}
		}(req, stream)
	}
}

func StartGrpcServer(port int) {
	lis, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		vars.Error("gRPC监听失败: %v", err)
		return
	}

	opt := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(MAX_MSG_SIZE),
		grpc.MaxSendMsgSize(MAX_MSG_SIZE),
	}

	s := grpc.NewServer(opt...)
	message.RegisterGrpcServer(s, &RpcServer{})

	vars.Info("gRPC服务启动成功,端口:%d", port)
	if err := s.Serve(lis); err != nil {
		vars.Error("gRPC服务启动失败: %v", err)
	}
	service_ = append(service_, s)
	vars.Info("gRPC服务启动成功,端口:%d", port)
}

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
