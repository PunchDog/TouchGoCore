package rpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
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
	// 连接 (原子指针：未连接时 Load 返回 nil，避免 atomic.Value.Store(nil) panic)
	conn atomic.Pointer[grpc.ClientConn]
	// 流复用: 客户端流
	stream   atomic.Value // message.Grpc_MsgClient
	streamMu sync.Mutex
	// streamCancel 当前流专属 context 的取消函数，随流重建/销毁。
	// 不能用某一次调用的 timeout ctx 建流：那次调用结束即取消，缓存流会立刻失效。
	streamCancel context.CancelFunc
	sendMu       sync.Mutex
	streamValid  atomic.Bool
	nextReqID    atomic.Uint64
	pending      sync.Map // uint64 -> chan *message.FSMessage
	// TLS 配置
	useTLS bool
	// 超时配置
	timeout time.Duration
	// closed 客户端已被主动关闭：关闭后不得再被重连定时器复活
	closed atomic.Bool
	// 回调接口（原子指针：SetCallbacks 可与 recvLoop/发送并发）
	callbacks atomic.Pointer[ClientCallbacks]
}

// Close 主动关闭客户端：停重连定时器、作废流、唤醒等待中的请求、关连接并从注册表摘除。
// 幂等，重复调用只清理一次。
func (c *RpcClient) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	// 先摘注册表：Close 之后注册表不该再暴露一个已废弃的客户端
	if rpcClient_ != nil {
		if cur, ok := rpcClient_.Load(c.serverName); ok && cur == c {
			rpcClient_.Delete(c.serverName)
		}
	}
	c.connStatus.Store(false)
	c.invalidateStream(nil)
	c.failAllPending()
	if conn := c.conn.Swap(nil); conn != nil {
		if err := conn.Close(); err != nil {
			vars.Error("RPC客户端关闭连接失败[%s]: %v", c.fullAddr, err)
		}
	}
	// 回调必须在交还对象池之前触发：Remove 之后这个实例可能立刻被下一个客户端复用，
	// 回调里读到的 UID/地址/状态就成了别人的数据
	c.triggerOnDisconnected(nil)
	// 所有权交还对象池：注册表里已无引用，Pause 会让这个实例永久悬挂
	c.Remove()
	return nil
}

// IsClosed 返回客户端是否已被主动关闭。
func (c *RpcClient) IsClosed() bool {
	return c.closed.Load()
}

func (c *RpcClient) Tick() {
	if c.closed.Load() {
		return
	}
	// 触发重连回调
	c.triggerOnReconnecting()

	// 断线重连，链接上了就从计时器里移除
	conn, err := newClient(c.fullAddr, c.useTLS)
	if err != nil {
		vars.Error("RPC客户端连接失败[%s]: %v", c.fullAddr, err)
		// 触发错误回调
		c.triggerOnError(err)
		return
	}
	// 拨号期间可能正好被 Close：这个实例已交还对象池，继续往下会把别人的
	// 新客户端连上服务器并触发一套连接回调
	if c.closed.Load() {
		_ = conn.Close()
		return
	}
	// 换连接必须关掉旧连接，否则每次重连都会泄漏一套传输层与 goroutine
	if old := c.conn.Swap(conn); old != nil && old != conn {
		_ = old.Close()
	}
	c.connStatus.Store(true)
	// 旧连接上的流已无意义，连同其 context 一起作废
	c.invalidateStream(nil)
	// 只停调度、不回对象池：本客户端指针会长期留在注册表里，断线后 markDisconnected
	// 还要拿同一个内嵌 Timer 重新 AddTimer 复活，用 Remove 会把实例交还池再被人取走。
	c.Pause()

	// 触发连接成功回调
	c.triggerOnConnected()
}

// markDisconnected 标记连接断开，并启动重连定时器
func (c *RpcClient) markDisconnected() {
	if c.closed.Load() {
		return
	}
	c.connStatus.Store(false)
	c.invalidateStream(nil)
	c.failAllPending()
	c.triggerOnDisconnected(nil)
	// 丢掉这个定时器意味着该客户端永远不会再自动重连，必须留下痕迹
	if err := localtimer.AddTimer(c); err != nil {
		vars.Error("RPC客户端重连定时器注册失败[%s]: %v", c.fullAddr, err)
	}
}

func (c *RpcClient) SendMsg(protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	if c.closed.Load() {
		vars.Error("RPC客户端已关闭[%s]，协议:%d:%d", c.fullAddr, protocol1, protocol2)
		return
	}
	conn := c.conn.Load()
	if conn == nil {
		vars.Error("RPC客户端连接未就绪[%s]，协议:%d:%d", c.fullAddr, protocol1, protocol2)
		c.markDisconnected()
		return
	}

	// 本次调用的超时 ctx 只用于等待响应；流使用独立 context，
	// 否则 defer cancel() 会在调用返回的瞬间作废整条缓存流。
	waitCtx, cancelWait := context.WithTimeout(context.Background(), c.timeout)
	defer cancelWait()

	stream, err := c.ensureStream(conn)
	if err != nil {
		vars.Error("RPC客户端创建流失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		c.markDisconnected()
		return
	}

	reqID := c.nextReqID.Add(1)
	waitCh := make(chan *message.FSMessage, 1)
	c.pending.Store(reqID, waitCh)
	defer c.pending.Delete(reqID)

	req := util.NewFSMessageWithID(protocol1, protocol2, reqID, pb)
	c.sendMu.Lock()
	err = stream.Send(req)
	c.sendMu.Unlock()
	if err != nil {
		vars.Error("RPC客户端发送失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		// 只能作废发送失败的那条流，不能连带杀掉别人刚建好的新流
		if c.invalidateStream(stream) {
			c.failAllPending()
		}
		c.triggerOnError(err)
		c.markDisconnected()
		return
	}
	c.triggerOnMessageSent(protocol1, protocol2, pb)

	var recv *message.FSMessage
	select {
	case recv = <-waitCh:
		if recv == nil {
			vars.Error("RPC客户端连接断开[%s] 协议:%d:%d request_id=%d", c.fullAddr, protocol1, protocol2, reqID)
			return
		}
	case <-waitCtx.Done():
		vars.Error("RPC客户端等待响应超时[%s] 协议:%d:%d request_id=%d", c.fullAddr, protocol1, protocol2, reqID)
		return
	}
	if isErrorPacket(recv) {
		// 服务端已明确失败（超时/handler 出错），不必再走一次业务回调
		err := fmt.Errorf("RPC服务端处理失败[%s] 协议:%d:%d request_id=%d", c.fullAddr, protocol1, protocol2, reqID)
		vars.Error("%v", err)
		c.triggerOnError(err)
		return
	}
	c.dispatchRecv(protocol1, protocol2, recv, callfunc)
}

// isErrorPacket 判断响应帧是否为服务端错误包（Cmd=ErrorCmd，Body 为空）。
func isErrorPacket(msg *message.FSMessage) bool {
	return msg != nil && msg.GetHead().GetCmd() == ErrorCmd
}

// ensureStream 复用或重建长连接流（流 context 与单次调用超时解耦）
func (c *RpcClient) ensureStream(conn *grpc.ClientConn) (message.Grpc_MsgClient, error) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	return c.ensureStreamLocked(conn)
}

// ensureStreamLocked 调用者必须持有 streamMu
func (c *RpcClient) ensureStreamLocked(conn *grpc.ClientConn) (message.Grpc_MsgClient, error) {
	if c.streamValid.Load() {
		if streamVal := c.stream.Load(); streamVal != nil {
			return streamVal.(message.Grpc_MsgClient), nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = metadata.NewOutgoingContext(ctx, clientAuthMetadata(c.serverName))
	client := message.NewGrpcClient(conn)
	stream, err := client.Msg(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	// 重建前收掉旧流：取消旧 context，让旧 recvLoop 尽快退出
	c.closeStreamLocked()
	c.stream.Store(stream)
	c.streamCancel = cancel
	c.streamValid.Store(true)
	go c.recvLoop(stream)
	return stream, nil
}

// currentStream 返回当前缓存流，无流时返回 nil
func (c *RpcClient) currentStream() message.Grpc_MsgClient {
	if v := c.stream.Load(); v != nil {
		if s, ok := v.(message.Grpc_MsgClient); ok {
			return s
		}
	}
	return nil
}

// invalidateStream 作废流：stream 为 nil 时无条件作废当前流，
// 非 nil 时仅当它仍是当前流才作废（避免旧 recvLoop 误杀重连后的新流）。
// 返回是否真的作废了一条流。
func (c *RpcClient) invalidateStream(stream message.Grpc_MsgClient) bool {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()
	if stream != nil && c.currentStream() != stream {
		return false
	}
	c.closeStreamLocked()
	return true
}

// closeStreamLocked 调用者必须持有 streamMu。
// 注意不要把 c.stream 置为 nil 值：atomic.Value.Store(nil) 会直接 panic。
func (c *RpcClient) closeStreamLocked() {
	c.streamValid.Store(false)
	if c.streamCancel != nil {
		c.streamCancel()
		c.streamCancel = nil
	}
}

func (c *RpcClient) recvLoop(stream message.Grpc_MsgClient) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("RPC客户端recvLoop发生panic[%s]: %v", c.fullAddr, r)
		}
	}()
	for {
		recv, err := stream.Recv()
		if err != nil {
			// context 被主动取消（重连/关闭）属于正常退出，不做断线处理
			if !c.invalidateStream(stream) {
				return
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			c.triggerOnError(err)
			c.failAllPending()
			return
		}
		rid := uint64(0)
		if recv.GetHead() != nil {
			rid = recv.GetHead().GetRequestId()
		}
		if rid != 0 {
			v, ok := c.pending.Load(rid)
			if !ok {
				// request_id 有值但无人等待：等待方已超时退出的残留响应。
				// 修复前这里会继续走「投给任意等待者」的兜底，把 A 的响应交给 B，
				// 业务拿到一条看着正常、内容完全无关的消息。
				vars.Error("RPC客户端收到无法关联的响应[%s] request_id=%d", c.fullAddr, rid)
				continue
			}
			select {
			case v.(chan *message.FSMessage) <- recv:
			default:
			}
			continue
		}
		// request_id=0：旧服务端不回填 head.request_id，只能投递给任意一个等待中的请求
		delivered := false
		c.pending.Range(func(_, value any) bool {
			select {
			case value.(chan *message.FSMessage) <- recv:
				delivered = true
				return false
			default:
				return true
			}
		})
		if !delivered {
			vars.Error("RPC客户端收到无法关联的响应[%s] request_id=%d", c.fullAddr, rid)
		}
	}
}

func (c *RpcClient) failAllPending() {
	c.pending.Range(func(key, value any) bool {
		c.pending.Delete(key)
		if ch, ok := value.(chan *message.FSMessage); ok {
			select {
			case ch <- nil:
			default:
			}
		}
		return true
	})
}

func (c *RpcClient) dispatchRecv(protocol1, protocol2 int32, recv *message.FSMessage, callfunc func(pb1 proto.Message)) {
	res := util.ParseFSMessage(recv)
	if res == nil {
		vars.Error("RPC客户端响应解析失败[%s] 协议:%d:%d", c.fullAddr, protocol1, protocol2)
		return
	}
	if callfunc != nil {
		reflectType := reflect.TypeOf(callfunc)
		resType := reflect.TypeOf(res)
		// 签名可能是 func() 或多参，直接 In(0) 会 panic
		if reflectType.NumIn() != 1 {
			vars.Error("RPC客户端回调签名不合法[%s] 协议:%d:%d: 需要 func(proto.Message)，实际 %v",
				c.fullAddr, protocol1, protocol2, reflectType)
		} else if pb1Type := reflectType.In(0); !resType.AssignableTo(pb1Type) {
			// 用可赋值判定而不是相等判定：形参常写成 proto.Message 接口，
			// 与具体响应类型永远不相等，旧写法下业务回调根本不会被调用。
			vars.Error("RPC客户端回调类型不匹配[%s] 期望:%v 实际:%v", c.fullAddr, pb1Type, resType)
		} else {
			callCallback(fmt.Sprintf("client[%s].callfunc %d:%d", c.serverName, protocol1, protocol2), func() {
				callfunc(res)
			})
		}
	}
	c.triggerOnMessageReceived(protocol1, protocol2, res)
}

func newClient(addr string, useTLS bool) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption

	if useTLS {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
			MinVersion:         tls.VersionTLS12,
		}
		if authMode() == "mtls" {
			var err error
			tlsConfig, err = mtlsClientTLS()
			if err != nil {
				return nil, err
			}
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
	if rpc := activeRpcCfg(); rpc != nil && rpc.TLS != nil {
		useTLS = rpc.TLS.Enable
		skipForIntranet = rpc.TLS.SkipForIntranet
	}

	// 检查是否需要跳过 TLS（内网且配置允许）
	if useTLS && skipForIntranet && util.IsIntranetIP(addr) {
		useTLS = false
		vars.Info("RPC客户端[%s]检测到内网地址[%s]，跳过TLS", servername, addr)
	}

	// 创建一个带计时器的客户端指针（initcallback 内完成连接参数初始化）
	client, err := localtimer.NewTimer[*RpcClient](1000, -1, func(c *RpcClient) {
		c.addr = addr
		c.port = port
		c.fullAddr = addr + ":" + strconv.Itoa(port)
		c.serverName = servername
		c.useTLS = useTLS
		// 池化实例可能带着上一次的 closed 标记回来
		c.closed.Store(false)
		c.timeout = 30 * time.Second // 默认超时 30 秒
		c.SetCallbacks(NewClientCallbacks())
	})
	if err != nil {
		vars.Error("创建RPC客户端失败[%s:%d]: %v", addr, port, err)
		return nil
	}

	conn, err := newClient(client.fullAddr, useTLS)
	if err == nil {
		client.conn.Store(conn)
		client.connStatus.Store(true)
		vars.Info("RPC客户端连接成功[%s], TLS: %v", client.fullAddr, useTLS)
	} else { // 一直保持监听保证连接
		vars.Error("RPC客户端初始连接失败[%s]: %v", client.fullAddr, err)
		// 不写入 nil：atomic.Pointer 零值即未连接，置 false 由重连定时器接管
		client.connStatus.Store(false)
		if err := localtimer.AddTimer(client); err != nil {
			vars.Error("RPC客户端重连定时器注册失败[%s]: %v", client.fullAddr, err)
		}
	}

	rpcClient_.Store(servername, client)
	return client
}

// ==================== 回调触发方法（内部使用）====================

// triggerOnReconnecting 触发重连回调
func (c *RpcClient) triggerOnReconnecting() {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnReconnecting != nil {
		callCallback(fmt.Sprintf("client[%s].OnReconnecting", c.serverName), func() {
			cbs.OnReconnecting(c.serverName, 0)
		})
	}
}

// triggerOnConnected 触发连接成功回调
func (c *RpcClient) triggerOnConnected() {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnConnected != nil {
		callCallback(fmt.Sprintf("client[%s].OnConnected", c.serverName), func() {
			cbs.OnConnected(c.serverName)
		})
	}
}

// triggerOnDisconnected 触发断开连接回调
func (c *RpcClient) triggerOnDisconnected(err error) {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnDisconnected != nil {
		callCallback(fmt.Sprintf("client[%s].OnDisconnected", c.serverName), func() {
			cbs.OnDisconnected(c.serverName, err)
		})
	}
}

// triggerOnError 触发错误回调
func (c *RpcClient) triggerOnError(err error) {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnError != nil {
		callCallback(fmt.Sprintf("client[%s].OnError", c.serverName), func() {
			cbs.OnError(c.serverName, err)
		})
	}
}

// triggerOnMessageSent 触发消息发送成功回调
func (c *RpcClient) triggerOnMessageSent(protocol1, protocol2 int32, req proto.Message) {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnMessageSent != nil {
		callCallback(fmt.Sprintf("client[%s].OnMessageSent", c.serverName), func() {
			cbs.OnMessageSent(c.serverName, protocol1, protocol2, req)
		})
	}
}

// triggerOnMessageReceived 触发消息接收回调
func (c *RpcClient) triggerOnMessageReceived(protocol1, protocol2 int32, resp proto.Message) {
	cbs := c.callbacks.Load()
	if cbs != nil && cbs.OnMessageReceived != nil {
		callCallback(fmt.Sprintf("client[%s].OnMessageReceived", c.serverName), func() {
			cbs.OnMessageReceived(c.serverName, protocol1, protocol2, resp)
		})
	}
}

// ==================== 公共方法：回调接口管理 ====================

// SetCallbacks 设置客户端回调接口
func (c *RpcClient) SetCallbacks(callbacks *ClientCallbacks) {
	if callbacks == nil {
		callbacks = NewClientCallbacks()
	}
	c.callbacks.Store(callbacks)
}

// GetCallbacks 获取客户端回调接口
func (c *RpcClient) GetCallbacks() *ClientCallbacks {
	return c.callbacks.Load()
}
