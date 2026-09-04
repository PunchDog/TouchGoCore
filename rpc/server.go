package rpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/config"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var (
	service_ *syncmap.Map[string, *RpcServer]
)

type msginfo struct {
	req           proto.Message
	clientNameKey string
	protol1       int32
	protol2       int32
	requestID     uint64
}

type RpcServer struct {
	message.UnimplementedGrpcServer
	nametoclientstream *syncmap.Map[string, message.Grpc_MsgServer]
	name               string
	service            *grpc.Server
	readchannel        chan *msginfo
	handlechannel      chan *msginfo
	done               chan struct{}
	stopped            atomic.Bool
	closeOnce          sync.Once
	handlerSem         chan struct{}
	// 使用独立的 CallFunction 实例，避免全局单例并发问题
	callFunc *util.CallFunction
	// 回调接口
	callbacks *ServerCallbacks
	// 正在执行的 handler 数量（超时后仍可能未退出，需业务接 ctx）
	inFlight atomic.Int64
}

func (s *RpcServer) Msg(stream message.Grpc_MsgServer) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok {
		vars.Error("gRPC连接错误,没有元数据")
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	clientName := md.Get("client-name")
	if len(clientName) == 0 {
		vars.Error("gRPC连接错误,没有客户端名称")
		return status.Error(codes.Unauthenticated, "missing client-name")
	}
	if clientName[0] == "" {
		vars.Error("gRPC连接错误,没有客户端名称")
		return status.Error(codes.Unauthenticated, "empty client-name")
	}
	// 客户端名称作为key
	clientNameKey := clientName[0]
	// 存储客户端stream
	s.nametoclientstream.Store(clientNameKey, stream)

	// 触发客户端连接回调
	s.triggerOnClientConnected(clientNameKey)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			vars.Info("gRPC连接关闭,客户端主动断开连接")
			// 移除客户端stream
			s.nametoclientstream.Delete(clientNameKey)
			// 触发客户端断开回调
			s.triggerOnClientDisconnected(clientNameKey)
			break
		}

		if err != nil {
			vars.Error("接收gRPC消息错误: %v", err)
			// 移除客户端stream
			s.nametoclientstream.Delete(clientNameKey)
			// 触发客户端断开回调
			s.triggerOnClientDisconnected(clientNameKey)
			return err
		}

		// 避免在服务器停止后继续发送消息
		select {
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
		default:
		}

		var reqID uint64
		if msg.GetHead() != nil {
			reqID = msg.GetHead().GetRequestId()
		}
		select {
		case s.readchannel <- &msginfo{
			req:           msg,
			clientNameKey: clientNameKey,
			protol1:       msg.GetHead().GetProtocol1(),
			protol2:       msg.GetHead().GetProtocol2(),
			requestID:     reqID,
		}:
			// 发送成功
		case <-s.done:
			vars.Info("RPC服务器已停止，丢弃接收到的消息[%s]", clientNameKey)
			return nil
		}

		// 触发消息接收回调（在放入 readchannel 后）
		s.triggerOnMessageReceived(clientNameKey, msg.GetHead().GetProtocol1(), msg.GetHead().GetProtocol2(), msg)
	}
	return nil
}

// 发送消息
func (s *RpcServer) Send(name string, pb1, pb2 int32, pb proto.Message) error {
	return s.SendWithRequestID(name, pb1, pb2, 0, pb)
}

// SendWithRequestID 发送响应并回填 request_id（0 表示旧客户端兼容串行模式）
func (s *RpcServer) SendWithRequestID(name string, pb1, pb2 int32, requestID uint64, pb proto.Message) error {
	if !s.triggerOnSendResponse(name, pb1, pb2, pb) {
		return fmt.Errorf("gRPC Send: 回调阻止了发送 [name=%s]", name)
	}

	rsp := util.NewFSMessageWithID(pb1, pb2, requestID, pb)
	st1, h := s.nametoclientstream.Load(name)
	if !h {
		return fmt.Errorf("gRPC Send: 未找到客户端流[name=%s]", name)
	}
	st := st1.(message.Grpc_MsgServer)
	if err := st.Send(rsp); err != nil {
		return fmt.Errorf("发送gRPC响应错误: %v", err)
	}
	return nil
}

// 解析数据
func (s *RpcServer) readChannel() {
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
					requestID:     msg.requestID,
				}
			}
		}
	}
}

// 操作数据
func (s *RpcServer) handleChannel() {
	// 消息处理超时，默认5秒
	const timeout = 5 * time.Second

	for {
		select {
		case <-s.done:
			return
		case <-rpcRunCtx.Done():
			return
		case msg := <-s.handlechannel:
			parent := rpcRunCtx
			if parent == nil {
				parent = context.Background()
			}
			ctx, cancel := context.WithTimeout(parent, timeout)

			resultCh := make(chan struct {
				bret bool
				res  []reflect.Value
			}, 1)

			select {
			case s.handlerSem <- struct{}{}:
			case <-s.done:
				cancel()
				return
			}

			// handler 必须接受 context.Context 为首参，超时才能取消阻塞 IO
			s.inFlight.Add(1)
			go func() {
				defer s.inFlight.Add(-1)
				defer func() { <-s.handlerSem }()
				key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.protol1, msg.protol2)
				select {
				case <-ctx.Done():
					select {
					case resultCh <- struct {
						bret bool
						res  []reflect.Value
					}{bret: false, res: nil}:
					default:
					}
					return
				default:
				}
				results, ok := s.callFunc.DoWithRetCtx(ctx, key, msg)
				if ok && len(results) > 0 {
					resultCh <- struct {
						bret bool
						res  []reflect.Value
					}{bret: true, res: results}
				} else {
					resultCh <- struct {
						bret bool
						res  []reflect.Value
					}{bret: false, res: nil}
				}
			}()

			// 等待处理结果或超时
			select {
			case <-ctx.Done():
				cancel()
				vars.Error("处理gRPC请求超时,协议号:%d:%d, 客户端:%s, in-flight=%d（回调须接受 context.Context）",
					msg.protol1, msg.protol2, msg.clientNameKey, s.inFlight.Load())

			case result := <-resultCh:
				cancel()
				if result.bret {
					if len(result.res) > 0 {
						rsp := result.res[0].Interface().(proto.Message)
						s.SendWithRequestID(msg.clientNameKey, msg.protol1, msg.protol2, msg.requestID, rsp)

						// 触发消息处理成功回调
						s.triggerOnMessageProcessed(msg.clientNameKey, msg.protol1, msg.protol2, rsp, true)
					} else {
						vars.Error("处理gRPC请求错误,没有返回值,协议号:%d:%d, 客户端:%s",
							msg.protol1, msg.protol2, msg.clientNameKey)

						// 触发消息处理失败回调（无返回值）
						s.triggerOnMessageProcessed(msg.clientNameKey, msg.protol1, msg.protol2, nil, false)
					}
				} else {
					vars.Error("处理gRPC请求错误,协议号:%d:%d, 客户端:%s",
						msg.protol1, msg.protol2, msg.clientNameKey)

					// 触发消息处理失败回调
					s.triggerOnMessageProcessed(msg.clientNameKey, msg.protol1, msg.protol2, nil, false)
				}
			}
		}
	}
}

// 关闭服务
func (s *RpcServer) closeDone() {
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

func (s *RpcServer) Stop(ctx context.Context) {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	s.closeDone()
	if s.service != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		done := make(chan struct{})
		go func() {
			s.service.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			s.service.Stop()
		case <-time.After(10 * time.Second):
			s.service.Stop()
		}
	}

	s.triggerOnServerStopped()

	vars.Info("RPC服务器停止[%s]", s.name)
}

func StartGrpcServer(name string, port int, useTLS bool) error {
	addr := "[::]:" + strconv.Itoa(port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("gRPC监听失败[%s]: %v", addr, err)
		return err
	}

	// 检查 TLS 配置
	var serverOptions []grpc.ServerOption

	if useTLS && config.Cfg_ != nil {
		rpc := config.Cfg_.Rpc
		if rpc == nil {
			rpc = config.Cfg_.RpcPort
		}
		if rpc != nil && rpc.TLS != nil {
			cert, err := tls.LoadX509KeyPair(rpc.TLS.CertFile, rpc.TLS.KeyFile)
			if err != nil {
				vars.Error("gRPC加载TLS证书失败[%s]: %v", addr, err)
				return err
			}
			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
			if authMode() == "mtls" {
				tlsConfig, err = mtlsServerTLS(tlsConfig)
				if err != nil {
					vars.Error("gRPC mTLS 配置失败[%s]: %v", addr, err)
					return err
				}
			}
			serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
		}
	}

	warnInsecureRPC(name, useTLS)

	serverOptions = append(serverOptions,
		grpc.ChainUnaryInterceptor(authUnaryInterceptor),
		grpc.ChainStreamInterceptor(authStreamInterceptor),
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

	vars.Info("gRPC监听已启动[%s]，服务器名称:%s, TLS: %v", addr, name, useTLS)

	s := grpc.NewServer(serverOptions...)
	service := &RpcServer{
		name:               name,
		service:            s,
		readchannel:        make(chan *msginfo, MAX_CHANNEL_SIZE),
		handlechannel:      make(chan *msginfo, MAX_CHANNEL_SIZE),
		done:               make(chan struct{}),
		callFunc:           &util.CallFunction{},
		nametoclientstream: syncmap.NewMap[string, message.Grpc_MsgServer](),
		callbacks:          NewServerCallbacks(),
		handlerSem:         make(chan struct{}, 256),
	}

	message.RegisterGrpcServer(service.service, service)

	go func(s *RpcServer) {
		//启动监听
		if err := s.service.Serve(lis); err != nil {
			vars.Error("gRPC服务启动失败[%s]: %v", s.name, err)
			service_.Delete(s.name)
			s.closeDone()
			return
		}
	}(service)

	go service.readChannel()
	go service.handleChannel()

	service_.Store(name, service)
	vars.Info("gRPC服务启动成功,端口:%d", port)

	// 触发服务启动回调
	service.triggerOnServerStarted()
	return nil
}

// ==================== 回调触发方法（内部使用）====================

// triggerOnServerStarted 触发服务启动回调
func (s *RpcServer) triggerOnServerStarted() {
	if s.callbacks != nil && s.callbacks.OnServerStarted != nil {
		s.callbacks.OnServerStarted(s.name)
	}
}

// triggerOnServerStopped 触发服务停止回调
func (s *RpcServer) triggerOnServerStopped() {
	if s.callbacks != nil && s.callbacks.OnServerStopped != nil {
		s.callbacks.OnServerStopped(s.name)
	}
}

// triggerOnClientConnected 触发客户端连接回调
func (s *RpcServer) triggerOnClientConnected(clientName string) {
	if s.callbacks != nil && s.callbacks.OnClientConnected != nil {
		s.callbacks.OnClientConnected(s.name, clientName)
	}
}

// triggerOnClientDisconnected 触发客户端断开回调
func (s *RpcServer) triggerOnClientDisconnected(clientName string) {
	if s.callbacks != nil && s.callbacks.OnClientDisconnected != nil {
		s.callbacks.OnClientDisconnected(s.name, clientName)
	}
}

// triggerOnMessageReceived 触发消息接收回调（返回是否继续处理）
func (s *RpcServer) triggerOnMessageReceived(clientName string, protocol1, protocol2 int32, msg proto.Message) bool {
	if s.callbacks != nil && s.callbacks.OnMessageReceived != nil {
		return s.callbacks.OnMessageReceived(s.name, clientName, protocol1, protocol2, msg)
	}
	return true // 默认继续处理
}

// triggerOnMessageProcessed 触发消息处理完成回调
func (s *RpcServer) triggerOnMessageProcessed(clientName string, protocol1, protocol2 int32, result proto.Message, success bool) {
	if s.callbacks != nil && s.callbacks.OnMessageProcessed != nil {
		s.callbacks.OnMessageProcessed(s.name, clientName, protocol1, protocol2, result, success)
	}
}

// triggerOnSendResponse 触发发送响应前回调（返回是否继续发送）
func (s *RpcServer) triggerOnSendResponse(clientName string, protocol1, protocol2 int32, resp proto.Message) bool {
	if s.callbacks != nil && s.callbacks.OnSendResponse != nil {
		return s.callbacks.OnSendResponse(s.name, clientName, protocol1, protocol2, resp)
	}
	return true // 默认继续发送
}

// ==================== 公共方法：回调接口管理 ====================

// SetCallbacks 设置服务端回调接口
func (s *RpcServer) SetCallbacks(callbacks *ServerCallbacks) {
	s.callbacks = callbacks
}

// GetCallbacks 获取服务端回调接口
func (s *RpcServer) GetCallbacks() *ServerCallbacks {
	return s.callbacks
}
