package rpc

import (
	"net"
	"strconv"
	"touchgocore/network/message"
	"touchgocore/vars"

	grpc "google.golang.org/grpc"
)

type RpcServer struct {
	message.UnimplementedGrpcServer
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			vars.Error("接收gRPC消息错误: %v", err)
			return err
		}

		// 处理请求逻辑
		res := &message.FSMessage{}
		if err := stream.Send(res); err != nil {
			vars.Error("发送gRPC响应错误: %v", err)
			return err
		}
	}
}

func StartGrpcServer(port int) {
	lis, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		vars.Error("gRPC监听失败: %v", err)
		return
	}

	s := grpc.NewServer()
	message.RegisterGrpcServer(s, &RpcServer{})

	vars.Info("gRPC服务启动成功,端口:%d", port)
	if err := s.Serve(lis); err != nil {
		vars.Error("gRPC服务启动失败: %v", err)
	}
}
