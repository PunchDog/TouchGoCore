package rpc

import (
	"context"
	"crypto/tls"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/config"
	"touchgocore/localtimer"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var (
	// RpcClient rpc客户端
	rpcClient_ *syncmap.Map[string, *RpcClient]
)

// RpcClient rpc客户端
type RpcClient struct {
	localtimer.Timer
	// 连接地址（不含端口）
	addr string
	// 端口
	port int
	// 完整地址 addr:port
	fullAddr string
	// 对应的服务器名
	serverName string
	// 连接状态 (原子操作)
	connStatus atomic.Bool
	// 连接 (原子操作)
	conn atomic.Value // *grpc.ClientConn
	// 流复用: 客户端流
	stream      atomic.Value // *grpc.ClientStream
	streamMu    sync.Mutex
	streamValid atomic.Bool
	// TLS 配置
	useTLS bool
	// 超时配置
	timeout time.Duration
	// 回调接口
	callbacks *ClientCallbacks
}

func (c *RpcClient) Tick() {
	// 触发重连回调
	if c.callbacks != nil && c.callbacks.OnReconnecting != nil {
		c.callbacks.OnReconnecting(c.serverName, 0)
	}

	// 断线重连，链接上了就从计时器里移除
	conn, err := newClient(c.fullAddr, c.useTLS)
	if err != nil {
		vars.Error("RPC客户端连接失败[%s]: %v", c.fullAddr, err)
		// 触发错误回调
		c.triggerOnError(err)
		return
	}
	c.conn.Store(conn)
	c.connStatus.Store(true)
	// 重置流状态
	c.streamValid.Store(false)
	c.Remove()

	// 触发连接成功回调
	c.triggerOnConnected()
}

// markDisconnected 标记连接断开，并启动重连定时器
func (c *RpcClient) markDisconnected() {
	c.connStatus.Store(false)

	// 触发断开连接回调
	c.triggerOnDisconnected(nil)

	localtimer.AddTimer(c)
}

func (c *RpcClient) SendMsg(protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	// 从原子值获取连接
	connVal := c.conn.Load()
	if connVal == nil {
		vars.Error("RPC客户端连接未就绪[%s]，协议:%d:%d", c.fullAddr, protocol1, protocol2)
		c.markDisconnected()
		return
	}
	conn := connVal.(*grpc.ClientConn)

	// 客户端 context 创建（带超时）
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	md := metadata.Pairs("client-name", c.serverName)
	ctx = metadata.NewOutgoingContext(ctx, md)

	// 尝试复用流
	c.streamMu.Lock()
	if c.streamValid.Load() {
		if streamVal := c.stream.Load(); streamVal != nil {
			stream := streamVal.(message.Grpc_MsgClient)
			c.streamMu.Unlock()
			c.sendWithStream(ctx, stream, protocol1, protocol2, pb, callfunc)
			return
		}
	}
	c.streamMu.Unlock()

	// 创建新流
	client := message.NewGrpcClient(conn)
	stream, err := client.Msg(ctx)
	if err != nil {
		vars.Error("RPC客户端创建流失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		c.markDisconnected()
		return
	}

	// 保存流以供复用
	c.streamMu.Lock()
	c.stream.Store(stream)
	c.streamValid.Store(true)
	c.streamMu.Unlock()

	c.sendWithStream(ctx, stream, protocol1, protocol2, pb, callfunc)
}

// sendWithStream 使用流发送消息
func (c *RpcClient) sendWithStream(ctx context.Context, stream message.Grpc_MsgClient, protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	req := util.NewFSMessage(protocol1, protocol2, pb)
	err := stream.Send(req)
	if err != nil {
		vars.Error("RPC客户端发送失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		// 流失效，需要重建
		c.streamValid.Store(false)
		c.markDisconnected()
		// 触发错误回调
		c.triggerOnError(err)
		return
	}

	// 触发消息发送成功回调
	c.triggerOnMessageSent(protocol1, protocol2, pb)

	recv, err := stream.Recv()
	if err != nil {
		vars.Error("RPC客户端接收失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		// 流失效，需要重建
		c.streamValid.Store(false)
		// 触发错误回调
		c.triggerOnError(err)
		return
	}
	if callfunc != nil {
		res := util.PasreFSMessage(recv)
		if res != nil && callfunc != nil {
			// 判断 res 和 pb1 是否相同类型
			reflectType := reflect.TypeOf(callfunc)
			pb1Type := reflectType.In(0)
			if reflect.TypeOf(res) == pb1Type {
				callfunc(res)
			} else {
				vars.Error("RPC客户端回调类型不匹配[%s] 期望:%v 实际:%v", c.fullAddr, pb1Type, reflect.TypeOf(res))
			}
		}

		// 触发消息接收回调（传递解析后的响应）
		c.triggerOnMessageReceived(protocol1, protocol2, res)
	} else {
		// 无回调函数时，也触发消息接收回调（传递原始响应）
		res := util.PasreFSMessage(recv)
		c.triggerOnMessageReceived(protocol1, protocol2, res)
	}
}

func newClient(addr string, useTLS bool) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if useTLS {
		// 加载 TLS 配置
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	opts = append(opts,
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MAX_MSG_SIZE)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(MAX_MSG_SIZE)),
		grpc.WithBackoffMaxDelay(5*time.Second),
	)

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func NewRpcClient(servername, addr string, port int) *RpcClient {
	// 检查 TLS 配置
	useTLS := false
	skipForIntranet := false
	if config.Cfg_ != nil && config.Cfg_.Rpc != nil && config.Cfg_.Rpc.TLS != nil {
		useTLS = config.Cfg_.Rpc.TLS.Enable
		skipForIntranet = config.Cfg_.Rpc.TLS.SkipForIntranet
	}

	// 检查是否需要跳过 TLS（内网且配置允许）
	if useTLS && skipForIntranet && util.IsIntranetIP(addr) {
		useTLS = false
		vars.Info("RPC客户端[%s]检测到内网地址[%s]，跳过TLS", servername, addr)
	}

	// 创建一个带计时器的客户端指针
	c, err := localtimer.NewTimer(1000, -1, &RpcClient{})
	if err != nil {
		vars.Error("创建RPC客户端失败[%s:%d]: %v", addr, port, err)
		return nil
	}
	client := c.(*RpcClient)
	client.addr = addr
	client.port = port
	client.fullAddr = addr + ":" + strconv.Itoa(port)
	client.serverName = servername
	client.useTLS = useTLS
	client.timeout = 30 * time.Second // 默认超时 30 秒
	client.callbacks = NewClientCallbacks() // 初始化回调接口

	conn, err := newClient(client.fullAddr, useTLS)
	if err == nil {
		client.conn.Store(conn)
		client.connStatus.Store(true)
		vars.Info("RPC客户端连接成功[%s], TLS: %v", client.fullAddr, useTLS)
	} else { // 一直保持监听保证连接
		vars.Error("RPC客户端初始连接失败[%s]: %v", client.fullAddr, err)
		client.conn.Store(nil)
		client.connStatus.Store(false)
		localtimer.AddTimer(client)
	}

	rpcClient_.Store(servername, client)
	return client
}

// ==================== 回调触发方法（内部使用）====================

// triggerOnConnected 触发连接成功回调
func (c *RpcClient) triggerOnConnected() {
	if c.callbacks != nil && c.callbacks.OnConnected != nil {
		c.callbacks.OnConnected(c.serverName)
	}
}

// triggerOnDisconnected 触发断开连接回调
func (c *RpcClient) triggerOnDisconnected(err error) {
	if c.callbacks != nil && c.callbacks.OnDisconnected != nil {
		c.callbacks.OnDisconnected(c.serverName, err)
	}
}

// triggerOnError 触发错误回调
func (c *RpcClient) triggerOnError(err error) {
	if c.callbacks != nil && c.callbacks.OnError != nil {
		c.callbacks.OnError(c.serverName, err)
	}
}

// triggerOnMessageSent 触发消息发送成功回调
func (c *RpcClient) triggerOnMessageSent(protocol1, protocol2 int32, req proto.Message) {
	if c.callbacks != nil && c.callbacks.OnMessageSent != nil {
		c.callbacks.OnMessageSent(c.serverName, protocol1, protocol2, req)
	}
}

// triggerOnMessageReceived 触发消息接收回调
func (c *RpcClient) triggerOnMessageReceived(protocol1, protocol2 int32, resp proto.Message) {
	if c.callbacks != nil && c.callbacks.OnMessageReceived != nil {
		c.callbacks.OnMessageReceived(c.serverName, protocol1, protocol2, resp)
	}
}

// ==================== 公共方法：回调接口管理 ====================

// SetCallbacks 设置客户端回调接口
func (c *RpcClient) SetCallbacks(callbacks *ClientCallbacks) {
	c.callbacks = callbacks
}

// GetCallbacks 获取客户端回调接口
func (c *RpcClient) GetCallbacks() *ClientCallbacks {
	return c.callbacks
}
