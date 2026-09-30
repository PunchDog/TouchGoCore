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
	// errClientClosed 客户端已关闭时拒绝新建/复用流。
	errClientClosed = errors.New("rpc client closed")
)

// closeDrainTimeout Close 排空后台协程的兜底上限：正常路径 recvLoop 随流取消即时退出、
// sendGinRegistration 随 failAllPending 立即返回，仅 pathological 的业务代理 handler 卡住
// 才会触顶。超时后**不交还对象池**（见 drainBackground / Close 的取舍说明）。
const closeDrainTimeout = 5 * time.Second

// defaultProxyConcurrency 下行代理请求的客户端并发上限（A-F6）。
// 修复前每条代理请求起一个无界 goroutine：网关侧一次流量尖峰或恶意刷量
// 就能让后端进程 goroutine 爆量。超限的请求直接回 503 语义的代理响应，
// 让网关按「后端过载」处理，而不是把客户端拖死。
const defaultProxyConcurrency = 64

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
	// bgWG 追踪与流绑定的后台协程（recvLoop / sendGinRegistration / handleProxyRequest）。
	// Close 交还对象池（Remove）前必须排空，否则旧主人的协程会与复用同一块内存的新主人的
	// initcallback 并发读写裸字段（serverName/fullAddr 等），构成对象池 use-after-free 数据竞争。
	bgWG sync.WaitGroup
	// proxySem 下行代理 handler 的并发信号量（容量 defaultProxyConcurrency）。
	// 惰性初始化（proxySemaphore）：RpcClient 经 localtimer 对象池复用，零值实例
	// 与测试直接构造的实例都拿不到 make 过的 channel；Once 保证只建一次。
	// 池复用时上一位主人的令牌必已归还——未归还（drain 超时）的实例不会被 Remove 交还池。
	proxySem     chan struct{}
	proxySemOnce sync.Once
}

// proxySemaphore 返回（并按需创建）代理并发信号量。
func (c *RpcClient) proxySemaphore() chan struct{} {
	c.proxySemOnce.Do(func() {
		c.proxySem = make(chan struct{}, defaultProxyConcurrency)
	})
	return c.proxySem
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
	// 交还对象池前必须排空与流绑定的后台协程：否则旧主人的 recvLoop / sendGinRegistration /
	// handleProxyRequest 仍可能读写裸字段，而 Remove 后这块内存随时被新 NewRpcClient 取走、
	// 其 initcallback 并写 serverName/fullAddr——这正是对象池 use-after-free 数据竞争的根因。
	if !c.drainBackground() {
		// A-F6：排空超时（业务代理 handler 卡死）则**不交还对象池**。
		// 取舍说明：卡死的协程仍持有本实例的字段引用，此时 Remove 交池等于把
		// 同一块内存交给新主人并发读写（use-after-free 数据竞争）；宁可泄漏这一个
		// 实例（连同卡死的 goroutine，它本就无法被强杀），也不制造静默的数据串台。
		// 泄漏有上界：只有触发 closeDrainTimeout 的异常关闭才会走到这里。
		vars.Error("RPC客户端[%s]后台协程未排空，实例不交还对象池（宁可泄漏，避免池复用 use-after-free）", c.fullAddr)
		return nil
	}
	// 所有权交还对象池：注册表里已无引用，Pause 会让这个实例永久悬挂
	c.Remove()
	return nil
}

// drainBackground 等待本客户端所有与流绑定的后台协程退出，带超时兜底。
// 目的：让 NewTimer initcallback 的「独占期」假设真正成立——交还对象池后不再有旧协程读写裸字段。
// 返回是否在预算内完成排空；false 表示仍有协程在跑，调用方不得交还对象池。
func (c *RpcClient) drainBackground() bool {
	done := make(chan struct{})
	go func() {
		c.bgWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(closeDrainTimeout):
		vars.Error("RPC客户端关闭时后台协程未在[%v]内退出[%s]，可能存在卡死的业务代理回调", closeDrainTimeout, c.fullAddr)
		return false
	}
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
	// 已关闭则拒绝建流：确保 Close 置 closed 后不会再 Add 新的后台协程，
	// 使 Close 的 bgWG.Wait 与所有 bgWG.Add 之间形成 happens-before（杜绝 Add-after-Wait）。
	if c.closed.Load() {
		return nil, errClientClosed
	}
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
	c.bgWG.Add(1)
	go func() {
		defer c.bgWG.Done()
		c.recvLoop(stream)
	}()
	// 流新建（含重连重建）后按 ginpath 配置向网关注册反向路由。
	// 异步发送：sendGinRegistration 会再次 ensureStream（需 streamMu），
	// 同步调用会与当前持锁的 ensureStreamLocked 自死锁。
	c.bgWG.Add(1)
	go func() {
		defer c.bgWG.Done()
		c.sendGinRegistration()
	}()
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
		// 反向代理请求（服务端发起，子码=3）：不当作自身 pending 响应，
		// 起协程异步处理避免拖慢客户端自身 RPC 回包。注册回执（子码=2）仍走下方 rid 匹配。
		if recv.GetHead().GetProtocol1() == ProtocolGinProxy && recv.GetHead().GetProtocol2() == ginSubProxyReq {
			// A-F6：并发信号量限流。占不到令牌说明已有 defaultProxyConcurrency 个
			// 代理 handler 在跑，直接回 503 语义的代理响应，不再起无界 goroutine。
			select {
			case c.proxySemaphore() <- struct{}{}:
			default:
				vars.Error("RPC客户端代理并发超限(>%d)[%s]，request_id=%d 直接回 503",
					defaultProxyConcurrency, c.fullAddr, recv.GetHead().GetRequestId())
				c.respondProxyOverloaded(recv)
				continue
			}
			// 本协程仍在 bgWG 计数内（Done 未执行），此处 Add(1) 发生在计数>0 时，
			// 与并发 Wait 合法；handleProxyRequest 会读裸字段，故纳入 Close 排空范围。
			c.bgWG.Add(1)
			go func() {
				defer c.bgWG.Done()
				defer func() { <-c.proxySemaphore() }()
				c.handleProxyRequest(recv)
			}()
			continue
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

// resetForReuse 池复用兜底：显式清空上一位主人可能残留的脏状态。
//
// 正常路径下 Close 已清干净这些字段，此处清理是幂等的；但若 Close 未走完
// 或被跳过，复用者不会继承在途请求表（回调错投）、流状态（在死流上收发）
// 与代际计数。调用点必须选在复用者独占期（NewTimer 的 initcallback 内：
// timerPool.Get 已完成所有权认领，注册表尚未 Store，定时器尚未调度），
// 不影响正在关闭的旧实例。
func (c *RpcClient) resetForReuse() {
	// 在途请求表：清空前先投递 nil 唤醒可能残留的等待者，语义与 failAllPending
	// 一致且不重复遍历；正常路径下表已为空，Range 不产生任何动作。
	c.failAllPending()
	// 请求代际计数归零：与清空后的 pending 保持一致，新实例从 1 重新编号。
	c.nextReqID.Store(0)
	// 流相关字段：独占期内直接重建零值 atomic.Value（Store(nil) 会 panic，
	// 而整体赋值在无并发的独占期安全），彻底摸除上一条脏流的引用。
	// streamMu 仍要持有：防御独占期约定被未来改动破坏后的数据竞争。
	c.streamMu.Lock()
	c.closeStreamLocked()
	c.stream = atomic.Value{}
	c.streamMu.Unlock()
	// 连接状态兜底：NewRpcClient 随后会按拨号结果重写，这里先归零，
	// 避免拨号前的窗口内脏实例对外谎报「已连接」。
	c.connStatus.Store(false)
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
		// 池复用兜底：显式清理上一位主人可能残留的脏状态（在途请求表 / 流 / 代际计数）。
		// 此刻正处于新主人独占期（timerPool.Get 已认领、注册表尚未 Store），
		// 与旧实例的关闭流程无并发，清理幂等安全。
		c.resetForReuse()
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
