package rpc

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"touchgocore/network/message"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var (
	service_           []*grpc.Server = nil
	nametoclientstream map[string]message.Grpc_MsgServer
)

type RpcServer struct {
	message.UnimplementedGrpcServer
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			vars.Info("gRPC连接关闭,客户端主动断开连接")
			break
		}

		if err != nil {
			vars.Error(fmt.Sprintf("接收gRPC消息错误: %v", err))
			return err
		}

		// 处理请求逻辑
		req := util.PasreFSMessage(msg)
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
	}
	return nil
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
