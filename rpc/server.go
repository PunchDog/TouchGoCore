package rpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"
	"touchgocore/config"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
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
	// 使用独立的 CallFunction 实例，避免全局单例并发问题
	callFunc *util.CallFunction
	// 回调接口
	callbacks *ServerCallbacks
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

		// 触发消息接收回调（在放入 readchannel 后）
		s.triggerOnMessageReceived(clientNameKey, msg.GetHead().GetProtocol1(), msg.GetHead().GetProtocol2(), msg)
	}
	return nil
}

// 发送消息
func (s *RpcServer) Send(name string, pb1, pb2 int32, pb proto.Message) error {
	// 触发发送响应前回调（可阻止发送）
	if !s.triggerOnSendResponse(name, pb1, pb2, pb) {
		return fmt.Errorf("gRPC Send: 回调阻止了发送 [name=%s]", name)
	}

	rsp := util.NewFSMessage(pb1, pb2, pb)
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
		case msg := <-s.handlechannel:
			// 为每个消息处理创建超时上下文
			ctx, cancel := context.WithTimeout(context.Background(), timeout)

			// 使用通道来接收处理结果
			resultCh := make(chan struct {
				bret bool
				res  []reflect.Value
			}, 1)

			// 在新的 goroutine 中处理消息，以便可以超时控制
			go func() {
				key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, msg.protol1, msg.protol2)
				// 检查上下文是否已超时，避免无意义计算
				select {
				case <-ctx.Done():
					select {
					case resultCh <- struct {
						bret bool
						res  []reflect.Value
					}{bret: false, res: nil}:
					default:
						// resultCh 可能已满（超时分支已写入），安全丢弃
					}
					return
				default:
				}
				results, ok := s.callFunc.DoWithRet(key, msg)
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
				// 超时或取消
				cancel()
				vars.Error("处理gRPC请求超时,协议号:%d:%d, 客户端:%s",
					msg.protol1, msg.protol2, msg.clientNameKey)
				// 清理 goroutine（它会在完成后退出）

			case result := <-resultCh:
				cancel()
				if result.bret {
					if len(result.res) > 0 {
						rsp := result.res[0].Interface().(proto.Message)
						s.Send(msg.clientNameKey, msg.protol1, msg.protol2, rsp)

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
func (s *RpcServer) Stop() {
	if s.stopped.Load() {
		return
	}
	s.stopped.Store(true)
	close(s.done)
	s.service.Stop()

	// 触发服务停止回调
	s.triggerOnServerStopped()

	vars.Info("RPC服务器停止[%s]", s.name)
}

func StartGrpcServer(name string, port int, useTLS bool) {
	addr := "[::]:" + strconv.Itoa(port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		vars.Error("gRPC监听失败[%s]: %v", addr, err)
		return
	}

	// 检查 TLS 配置
	var serverOptions []grpc.ServerOption

	if useTLS && config.Cfg_ != nil && config.Cfg_.Rpc != nil && config.Cfg_.Rpc.TLS != nil {
		cert, err := tls.LoadX509KeyPair(config.Cfg_.Rpc.TLS.CertFile, config.Cfg_.Rpc.TLS.KeyFile)
		if err != nil {
			vars.Error("gRPC加载TLS证书失败[%s]: %v", addr, err)
			return
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}

	serverOptions = append(serverOptions,
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

	if !useTLS {
		vars.Warning("gRPC服务器[%s]未启用TLS，请确保是内网环境", name)
	}

	s := grpc.NewServer(serverOptions...)
	service := &RpcServer{
		name:               name,
		service:            s,
		readchannel:        make(chan *msginfo, MAX_CHANNEL_SIZE),
		handlechannel:      make(chan *msginfo, MAX_CHANNEL_SIZE),
		done:               make(chan struct{}),
		callFunc:           &util.CallFunction{}, // 创建独立的 CallFunction 实例
		nametoclientstream: syncmap.NewMap[string, message.Grpc_MsgServer](),
		callbacks:          NewServerCallbacks(), // 初始化回调接口
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

	go service.readChannel()
	go service.handleChannel()

	service_.Store(name, service)
	vars.Info("gRPC服务启动成功,端口:%d", port)

	// 触发服务启动回调
	service.triggerOnServerStarted()
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
