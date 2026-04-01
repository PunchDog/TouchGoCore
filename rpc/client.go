package rpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
}

func (c *RpcClient) Tick() {
	// 断线重连，链接上了就从计时器里移除
	conn, err := newClient(c.fullAddr, c.useTLS)
	if err != nil {
		vars.Error("RPC客户端连接失败[%s]: %v", c.fullAddr, err)
		return
	}
	c.conn.Store(conn)
	c.connStatus.Store(true)
	// 重置流状态
	c.streamValid.Store(false)
	c.Remove()
}

// markDisconnected 标记连接断开，并启动重连定时器
func (c *RpcClient) markDisconnected() {
	c.connStatus.Store(false)
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
		return
	}

	recv, err := stream.Recv()
	if err != nil {
		vars.Error("RPC客户端接收失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		// 流失效，需要重建
		c.streamValid.Store(false)
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
	}
}

// loadTLSConfig 加载 TLS 配置
func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // 生产环境应为 false
	}, nil
}

// loadSystemCertPool 加载系统根证书
func loadSystemCertPool() (*x509.CertPool, error) {
	certPool := x509.NewCertPool()
	// 加载系统根证书（Windows 下需要单独处理）
	// 这里可以添加更多证书加载逻辑
	return certPool, nil
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
