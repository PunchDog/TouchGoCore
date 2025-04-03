package rpc

import (
	"fmt"
	"net"
	"strconv"
	"touchgocore/config"
	"touchgocore/network/message"
	"touchgocore/vars"

	grpc "google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type RpcServer struct {
	message.UnimplementedGrpcServer
	//注册进入的协议处理
	prtoToHandler map[string](func(proto.Message) proto.Message)
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			vars.Error("接收gRPC消息错误: %v", err)
			return err
		}

		err = <-func(r *message.FSMessage, st message.Grpc_MsgServer) chan error {
			errch := make(chan error, 1)
			go func() {
				//处理协议
				if fn, ok := s.prtoToHandler[r.GetHead().GetCmd()]; ok {
					res := &message.FSMessage{}
					//通过“message.fsmessage”这个字符串创建proto.Message类型的消息
					msgType, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(r.GetHead().GetCmd()))
					if err == nil {
						reqMsg := msgType.New().Interface()
						ret := fn(reqMsg)
						res.GetHead().Cmd = proto.String(string(proto.MessageName(ret)))
						res.Body, _ = proto.Marshal(ret)
					}

					// 处理请求逻辑
					if err := st.Send(res); err != nil {
						vars.Error(fmt.Sprintf("发送gRPC响应错误: %v", err))
						errch <- err
					}
				}
				errch <- nil
			}()
			return errch
		}(req, stream)
		if err != nil {
			return err
		}
	}
}

var rpcServer_ *RpcServer = nil

func init() {
	rpcServer_ = &RpcServer{
		prtoToHandler: make(map[string](func(proto.Message) proto.Message)),
	}
}

// 注册协议处理函数，函数参数为proto.Message，返回值为proto.Message
func RegisterCmd(cmd string, handler func(proto.Message) proto.Message) {
	rpcServer_.prtoToHandler[cmd] = handler
}

func StartGrpcServer() {
	// 设置最大接收和发送消息大小
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(100 * 1024 * 1024), // 100MB
		grpc.MaxSendMsgSize(100 * 1024 * 1024), // 100MB
	}

	lis, err := net.Listen("tcp", "[::]:"+strconv.Itoa(config.Cfg_.RpcPort))
	if err != nil {
		vars.Error(fmt.Sprintf("gRPC监听失败: %v", err))
		return
	}

	s := grpc.NewServer(opts...)
	message.RegisterGrpcServer(s, rpcServer_)
	vars.Info(fmt.Sprintf("gRPC服务启动成功,端口:%d", config.Cfg_.RpcPort))

	go func() {
		if err := s.Serve(lis); err != nil {
			vars.Error(fmt.Sprintf("gRPC服务启动失败: %v", err))
		}
	}()
}
