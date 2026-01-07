package rpc

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var (
	service_ map[string]*RpcServer = nil
)

type msginfo struct {
	req           proto.Message
	clientNameKey string
	protol1       int32
	protol2       int32
}

type RpcServer struct {
	message.UnimplementedGrpcServer
	nametoclientstream syncmap.Map
	name               string
	service            *grpc.Server
	readchannel        chan *msginfo
	handlechannel      chan *msginfo
	done               chan struct{}
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		vars.Error("gRPC连接错误,没有元数据")
		return nil
	}
	//获取元数据
	clientName := md.Get("client-name")
	if len(clientName) == 0 {
		vars.Error("gRPC连接错误,没有客户端名称")
		return fmt.Errorf("gRPC连接错误: 没有客户端名称")
	}
	if clientName[0] == "" {
		vars.Error("gRPC连接错误,没有客户端名称")
		return fmt.Errorf("gRPC连接错误: 没有客户端名称")
	}
	// 客户端名称作为key
	clientNameKey := clientName[0]
	// 存储客户端stream
	s.nametoclientstream.Store(clientNameKey, stream)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			vars.Info("gRPC连接关闭,客户端主动断开连接")
			// 移除客户端stream
			s.nametoclientstream.Delete(clientNameKey)
			break
		}

		if err != nil {
			vars.Error(fmt.Sprintf("接收gRPC消息错误: %v", err))
			// 移除客户端stream
			s.nametoclientstream.Delete(clientNameKey)
			return err
		}

		s.readchannel <- &msginfo{
			req:           msg,
			clientNameKey: clientNameKey,
			protol1:       msg.GetHead().GetProtocol1(),
			protol2:       msg.GetHead().GetProtocol2(),
		}
	}
	return nil
}

// 发送消息
func (s *RpcServer) Send(name string, pb1, pb2 int32, pb proto.Message) error {
	rsp := util.NewFSMessage(pb1, pb2, pb)
	if st1, h := s.nametoclientstream.Load(name); h {
		st := st1.(message.Grpc_MsgServer)
		if err := st.Send(rsp); err != nil {
			return fmt.Errorf("发送gRPC响应错误: %v", err)
		}
	}
	return nil
}

// 解析数据
func (s *RpcServer) readChanel() {
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.readchannel:
			req := util.PasreFSMessage(msg.req)
			if req != nil {
				s.handlechannel <- &msginfo{
					req:           req,
					clientNameKey: msg.clientNameKey,
					protol1:       msg.protol1,
					protol2:       msg.protol2,
				}
			}
		}
	}
}

// 操作数据
func (s *RpcServer) handleChanel() {
	for {
		select {
		case <-s.done:
			return
		case msg := <-s.handlechannel:
			util.DefaultCallFunc.SetDoRet()
			key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.protol1, msg.protol2)
			bret := util.DefaultCallFunc.Do(key, msg)
			res := util.DefaultCallFunc.GetRet()
			if bret {
				rsp := res[0].Interface().(proto.Message)
				s.Send(msg.clientNameKey, msg.protol1, msg.protol2, rsp)
			} else {
				vars.Error("处理gRPC请求错误,协议号:%d:%d", msg.protol1, msg.protol2)
			}
		}
	}
}

// 关闭服务
func (s *RpcServer) Stop() {
	close(s.done)
	s.service.Stop()
}

func StartGrpcServer(name, ip string, port int) {
	lis, err := net.Listen("tcp", "[::]:"+strconv.Itoa(port))
	if err != nil {
		vars.Error("gRPC监听失败: %v", err)
		return
	}

	// opt := []grpc.ServerOption{
	// 	grpc.MaxRecvMsgSize(MAX_MSG_SIZE),
	// 	grpc.MaxSendMsgSize(MAX_MSG_SIZE),
	// }

	s := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// MaxConnectionIdle 和 MaxConnectionAge 设为 0 表示无限制，永不主动断开
			MaxConnectionIdle:     0,                // 不因空闲断开
			MaxConnectionAge:      0,                // 不因存活时间断开
			MaxConnectionAgeGrace: 30 * time.Second, // 优雅关闭宽限期
			Time:                  2 * time.Hour,    // 服务端 ping 间隔（基本不主动 ping）
			Timeout:               20 * time.Second, // ping 超时
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second, // 允许客户端最小 5 秒 ping 一次（宽松策略）
			PermitWithoutStream: true,            // 允许无 stream 时 ping
		}),
		grpc.MaxRecvMsgSize(MAX_MSG_SIZE),
		grpc.MaxSendMsgSize(MAX_MSG_SIZE),
	)
	service := &RpcServer{
		name:          name,
		service:       s,
		readchannel:   make(chan *msginfo, MAX_CHANNEL_SIZE),
		handlechannel: make(chan *msginfo, MAX_CHANNEL_SIZE),
		done:          make(chan struct{}),
	}

	message.RegisterGrpcServer(service.service, service)

	go func(s *RpcServer) {
		//启动监听
		if err := s.service.Serve(lis); err != nil {
			vars.Error("gRPC服务启动失败: %v", err)
			delete(service_, s.name)
			return
		}
	}(service)

	go service.readChanel()
	go service.handleChanel()

	if service_ == nil {
		service_ = make(map[string]*RpcServer)
	}
	service_[name] = service
	vars.Info("gRPC服务启动成功,端口:%d", port)
}
