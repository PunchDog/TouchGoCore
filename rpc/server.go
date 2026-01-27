package rpc

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
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
	service_ syncmap.Map
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
	stopped            atomic.Bool
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
			vars.Error("接收gRPC消息错误: %v", err)
			// 移除客户端stream
			s.nametoclientstream.Delete(clientNameKey)
			return err
		}

		// 避免在服务器停止后继续发送消息
		select {
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
		default:
		}

		select {
		case s.readchannel <- &msginfo{
			req:           msg,
			clientNameKey: clientNameKey,
			protol1:       msg.GetHead().GetProtocol1(),
			protol2:       msg.GetHead().GetProtocol2(),
		}:
			// 发送成功
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
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
			key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.protol1, msg.protol2)
			res, bret := util.DefaultCallFunc.Do(key, msg)
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
	if s.stopped.Load() {
		return
	}
	s.stopped.Store(true)
	close(s.done)
	s.service.Stop()
	vars.Info("RPC服务器停止[%s]", s.name)
}

func StartGrpcServer(name, ip string, port int) {
	addr := "[::]:" + strconv.Itoa(port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("gRPC监听失败[%s]: %v", addr, err)
		return
	}
	vars.Info("gRPC监听已启动[%s]，服务器名称:%s", addr, name)

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
			vars.Error("gRPC服务启动失败[%s]: %v", s.name, err)
			service_.Delete(s.name)
			// 通知处理goroutine退出
			close(s.done)
			return
		}
	}(service)

	go service.readChanel()
	go service.handleChanel()

	service_.Store(name, service)
	vars.Info("gRPC服务启动成功,端口:%d", port)
}
