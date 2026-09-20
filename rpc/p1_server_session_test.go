package rpc

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// ============================================================================
// S45–S47：RPC 服务端流生命周期与停机预算、API 可用性、鉴权默认值回归。
// ============================================================================

// within 在 d 内轮询 cond，返回是否达成。
func within(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// rpcTestEnvCfg 用给定配置装配最小 RPC 测试环境。
func rpcTestEnvCfg(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	prevCfg := config.Cfg_
	prevRunCtx := runCtx()
	config.Cfg_ = cfg
	setRunCtx(corectx.WithCfg(context.Background(), cfg))
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prevCfg
		setRunCtx(prevRunCtx)
	})
}

func rpcNoneAuthEnv(t *testing.T) {
	t.Helper()
	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{Auth: &config.RpcAuthConfig{Mode: "none"}}})
}

// dialClient 连接指定端口并注册清理。
func dialClient(t *testing.T, name string, port int) *RpcClient {
	t.Helper()
	client := NewRpcClient(name, "127.0.0.1", port)
	if client == nil {
		t.Fatal("客户端未创建")
	}
	client.timeout = 3 * time.Second
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// startServer 启动服务端并注册停止清理。
func startServer(t *testing.T, name string, port int) *RpcServer {
	t.Helper()
	if err := StartGrpcServer(name, port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer(name)
	if srv == nil {
		t.Fatal("服务端未注册")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })
	return srv
}

// registerHandler 注册协议对与 handler，并在用例结束时撤销。
func registerHandler(t *testing.T, p1, p2 int32, handler func(ctx context.Context, msg *MessageInfo) proto.Message) {
	t.Helper()
	util.RegisterProtocolType(p1, p2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, p1, p2)
	id := util.DefaultCallFunc.Register(key, handler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, id) })
}

// useHandlerTimeout 临时调整服务端 handler 超时。
func useHandlerTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := handlerTimeout()
	SetHandlerTimeout(d)
	t.Cleanup(func() { SetHandlerTimeout(prev) })
}

// TestReadChannelReturnsWhenDoneClosed 停机时 handlechannel 已满，
// readChannel 必须靠 done 分支脱身，否则该 goroutine 永久阻塞。
func TestReadChannelReturnsWhenDoneClosed(t *testing.T) {
	rpcNoneAuthEnv(t)
	util.RegisterProtocolType(120, 120, wrapperspb.String(""))

	srv := &RpcServer{
		name:               "read-done",
		done:               make(chan struct{}),
		readchannel:        make(chan *MessageInfo, 1),
		handlechannel:      make(chan *MessageInfo, 1),
		nametoclientstream: syncmap.NewMap[string, *clientSession](),
	}
	// 预填满下游，令 readChannel 卡在发送上
	srv.handlechannel <- &MessageInfo{ClientNameKey: "x"}
	raw := util.NewFSMessageWithID(120, 120, 1, wrapperspb.String("ping"))
	if raw == nil {
		t.Fatal("构造请求失败")
	}
	srv.readchannel <- &MessageInfo{Req: raw, ClientNameKey: "x", Protol1: 120, Protol2: 120, RequestID: 1}

	returned := make(chan struct{})
	go func() {
		srv.readChannel()
		close(returned)
	}()
	if !within(500*time.Millisecond, func() bool { return len(srv.readchannel) == 0 }) {
		t.Fatal("readChannel 未消费 readchannel，前置条件不成立")
	}

	close(srv.done)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ 停机后 readChannel 永久卡在 handlechannel 发送上（goroutine 泄漏）")
	}
}

// TestHandleChannelRunsConcurrently 前一条消息的 handler 未完成时，后一条必须能被处理：
// 串行等待结果会让两条互等deadlock。
func TestHandleChannelRunsConcurrently(t *testing.T) {
	rpcNoneAuthEnv(t)
	useHandlerTimeout(t, 10*time.Second)

	release := make(chan struct{})
	// A：只有 B 能放行它——若串行处理，B 永远轮不到，A 只能等到自判超时
	registerHandler(t, 121, 121, func(_ context.Context, _ *MessageInfo) proto.Message {
		select {
		case <-release:
			return wrapperspb.String("concurrent")
		case <-time.After(6 * time.Second):
			return wrapperspb.String("serialized")
		}
	})
	registerHandler(t, 122, 122, func(_ context.Context, _ *MessageInfo) proto.Message {
		close(release)
		return wrapperspb.String("b")
	})

	port := freePort(t)
	startServer(t, "concurrent-srv", port)
	client := dialClient(t, "concurrent-client", port)
	client.timeout = 12 * time.Second

	gotA := make(chan proto.Message, 1)
	go client.SendMsg(121, 121, wrapperspb.String("a"), func(pb proto.Message) { gotA <- pb })
	client.SendMsg(122, 122, wrapperspb.String("b"), nil)

	select {
	case pb := <-gotA:
		if sv, ok := pb.(*wrapperspb.StringValue); !ok || sv.GetValue() != "concurrent" {
			t.Fatalf("✘ handler 被串行执行，A 只拿到 %v（修复前：等待结果写在派发循环里，B 无法并发运行）", pb)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("✘ A 没拿到响应，说明并发派发未生效")
	}
}

// TestHandlerTimeoutSendsErrorPacket handler 卡死时服务端必须回错误包，
// 而不是让客户端干等满自己的调用超时。
func TestHandlerTimeoutSendsErrorPacket(t *testing.T) {
	rpcNoneAuthEnv(t)
	SetShutdownBudget(time.Second, time.Second)
	t.Cleanup(func() { SetShutdownBudget(0, 0) })
	useHandlerTimeout(t, 200*time.Millisecond)

	registerHandler(t, 123, 123, func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(3 * time.Second) // 不尊重 ctx 的旧 handler
		return wrapperspb.String("late")
	})

	port := freePort(t)
	startServer(t, "timeout-srv", port)
	client := dialClient(t, "timeout-client", port)
	client.timeout = 10 * time.Second // 远超服务端超时，确保只有错误包能让它早退

	errs := make(chan error, 4)
	client.SetCallbacks(&ClientCallbacks{OnError: func(_ string, err error) {
		select {
		case errs <- err:
		default:
		}
	}})

	started := time.Now()
	blocked := make(chan struct{})
	go func() {
		client.SendMsg(123, 123, wrapperspb.String("ping"), nil)
		close(blocked)
	}()
	select {
	case <-blocked:
	case <-time.After(8 * time.Second):
		t.Fatal("✘ 客户端一直等到自己的调用超时（修复前：服务端超时不回任何包）")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("✘ 回错误包耗时 %v，未按 200ms 的服务端超时收敛", elapsed)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("✘ OnError 收到 nil")
		}
	default:
		t.Fatal("✘ 超时未触发 OnError 回调")
	}
}

// sendCounterStream 记录并发 Send 的峰值。
type sendCounterStream struct {
	message.Grpc_MsgServer
	mu      sync.Mutex
	active  int
	maxSeen int
	sent    int
}

func (s *sendCounterStream) Send(*message.FSMessage) error {
	s.mu.Lock()
	s.active++
	if s.active > s.maxSeen {
		s.maxSeen = s.active
	}
	s.mu.Unlock()

	time.Sleep(2 * time.Millisecond)

	s.mu.Lock()
	s.active--
	s.sent++
	s.mu.Unlock()
	return nil
}

// newBareServer 构造一个不监听端口、只够单测用的 RpcServer。
func newBareServer(name string) *RpcServer {
	return &RpcServer{
		name:               name,
		done:               make(chan struct{}),
		nametoclientstream: syncmap.NewMap[string, *clientSession](),
	}
}

// TestSendPerStreamMutex 同一条 gRPC 流并发 Send 会被传输层拒绝（要求单写），
// 必须按会话串行化。
func TestSendPerStreamMutex(t *testing.T) {
	rpcNoneAuthEnv(t)
	st := &sendCounterStream{}
	srv := newBareServer("send-mutex")
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.SendWithRequestID("peer", 1, 2, 1, wrapperspb.String("x")); err != nil {
				t.Errorf("SendWithRequestID: %v", err)
			}
		}()
	}
	wg.Wait()

	if st.maxSeen != 1 {
		t.Fatalf("✘ 同一条流上并发 Send 峰值=%d（必须为 1）", st.maxSeen)
	}
	if st.sent != 20 {
		t.Fatalf("✘ 发送计数 %d != 20", st.sent)
	}
}

// TestSessionCompareDelete 同名重连后，旧会话退出不得摘掉新会话，也不得补发断开回调。
func TestSessionCompareDelete(t *testing.T) {
	rpcNoneAuthEnv(t)
	var mu sync.Mutex
	connected, disconnected := 0, 0

	srv := newBareServer("session")
	srv.SetCallbacks(&ServerCallbacks{
		OnClientConnected:    func(string, string) { mu.Lock(); connected++; mu.Unlock() },
		OnClientDisconnected: func(string, string) { mu.Lock(); disconnected++; mu.Unlock() },
	})

	first := srv.openSession("peer", &sendCounterStream{})
	second := srv.openSession("peer", &sendCounterStream{})

	srv.closeSession("peer", first)
	if cur, ok := srv.nametoclientstream.Load("peer"); !ok || cur != second {
		t.Fatal("✘ 旧会话退出把新会话摘掉了，之后所有回包都会报「未找到客户端流」")
	}
	mu.Lock()
	gotConn, gotDisc := connected, disconnected
	mu.Unlock()
	if gotConn != 2 || gotDisc != 0 {
		t.Fatalf("✘ 连接/断开回调不成对: connected=%d disconnected=%d", gotConn, gotDisc)
	}

	srv.closeSession("peer", second)
	if _, ok := srv.nametoclientstream.Load("peer"); ok {
		t.Fatal("✘ 当前会话退出后仍残留")
	}
	mu.Lock()
	finalDisc := disconnected
	mu.Unlock()
	if finalDisc != 1 {
		t.Fatalf("✘ 断开回调计数 %d != 1", finalDisc)
	}
}

// TestTLSFailureClosesListener TLS 配置加载失败时必须释放已绑定的端口，
// 否则一次启动失败就把端口永久占住。
func TestTLSFailureClosesListener(t *testing.T) {
	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{
		Auth: &config.RpcAuthConfig{Mode: "none"},
		TLS:  &config.RpcTLSConfig{Enable: true, CertFile: "no-such-cert.crt", KeyFile: "no-such-key.key"},
	}})

	port := freePort(t)
	addr := "[::]:" + fmt.Sprint(port)
	if err := StartGrpcServer("tls-fail", port, true); err == nil {
		t.Fatal("✘ 证书不存在却启动成功")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("✘ TLS 失败后端口仍被占用: %v", err)
	}
	_ = ln.Close()
}

// TestStopUsesShutdownBudget 停机预算必须可配置：进程退出路径不能被写死的 5s 拖住。
func TestStopUsesShutdownBudget(t *testing.T) {
	rpcNoneAuthEnv(t)
	SetShutdownBudget(time.Second, 60*time.Millisecond)
	t.Cleanup(func() { SetShutdownBudget(0, 0) })

	srv := newBareServer("budget")
	srv.inFlight.Add(1) // 有一个不理会 ctx 的 handler 还在跑

	started := time.Now()
	srv.Stop(context.Background())
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("✘ 停机耗时 %v，预算未生效", elapsed)
	}
}

// TestRpcClientCloseIsIdempotent Close 之后不得被重连定时器复活，且要摘掉注册表。
func TestRpcClientCloseIsIdempotent(t *testing.T) {
	rpcNoneAuthEnv(t)
	client := NewRpcClient("closed-client", "127.0.0.1", freePort(t))
	if client == nil {
		t.Fatal("客户端未创建")
	}
	if GetRpcClient("closed-client") == nil {
		t.Fatal("✘ 客户端未登记进注册表")
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("✘ 重复 Close 返回错误: %v", err)
	}
	if GetRpcClient("closed-client") != nil {
		t.Fatal("✘ Close 后注册表仍持有该客户端，Stop 会对已回池实例二次清理")
	}
	if client.conn.Load() != nil {
		t.Fatal("✘ Close 未清空连接")
	}
	if !client.IsClosed() {
		t.Fatal("✘ IsClosed 应为 true")
	}

	client.Tick()
	if client.conn.Load() != nil {
		t.Fatal("✘ Close 之后 Tick 又建了新连接（定时器复活）")
	}
	client.markDisconnected()
	if client.connStatus.Load() {
		t.Fatal("✘ Close 之后 markDisconnected 改写了连接状态")
	}
}

// TestServerCallbacksPanicContained 「是否放行」型回调 panic 时按拒绝处理，
// 且不得把 panic 放出去带走服务端 goroutine。
func TestServerCallbacksPanicContained(t *testing.T) {
	rpcNoneAuthEnv(t)
	srv := newBareServer("panic")
	srv.SetCallbacks(&ServerCallbacks{
		OnMessageReceived: func(string, string, int32, int32, proto.Message) bool {
			panic("业务回调炸了")
		},
		OnServerStarted: func(string) { panic("启动回调炸了") },
	})

	if srv.triggerOnMessageReceived("peer", 1, 2, wrapperspb.String("x")) {
		t.Fatal("✘ panic 的放行回调默认放行了消息")
	}
	srv.triggerOnServerStarted() // 不 panic 即通过
}

// TestMessageReceivedCallbackRejects 回调返回 false 必须真正拦下消息（不再进 handler），
// 并且要把拒绝如实告知客户端。修复前拒绝分支只丢消息不回包，客户端只能空等满
// 自己的调用超时，业务侧看到的是「超时」而不是「被拒」。
func TestMessageReceivedCallbackRejects(t *testing.T) {
	rpcNoneAuthEnv(t)
	var handled atomic.Int32
	registerHandler(t, 124, 124, func(_ context.Context, _ *MessageInfo) proto.Message {
		handled.Add(1)
		return wrapperspb.String("ok")
	})

	port := freePort(t)
	srv := startServer(t, "reject-srv", port)
	srv.SetCallbacks(&ServerCallbacks{
		OnMessageReceived: func(string, string, int32, int32, proto.Message) bool { return false },
	})

	client := dialClient(t, "reject-client", port)
	client.timeout = 10 * time.Second // 远超本机调度耗时，只有错误包能让它早退

	errs := make(chan error, 4)
	client.SetCallbacks(&ClientCallbacks{OnError: func(_ string, err error) {
		select {
		case errs <- err:
		default:
		}
	}})

	bizCalled := make(chan struct{})
	blocked := make(chan struct{})
	started := time.Now()
	go func() {
		client.SendMsg(124, 124, wrapperspb.String("ping"), func(proto.Message) { close(bizCalled) })
		close(blocked)
	}()
	select {
	case <-blocked:
	case <-time.After(4 * time.Second):
		t.Fatal("✘ 被拒消息没有回错误包，客户端一直等自己的调用超时")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("✘ 拒绝回包耗时 %v，未走错误包路径", elapsed)
	}
	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("✘ OnError 收到 nil")
		}
	default:
		t.Fatal("✘ 被拒未触发 OnError，客户端分不清拒绝与超时")
	}
	select {
	case <-bizCalled:
		t.Fatal("✘ 错误包被当成响应交给了业务回调")
	default:
	}
	if n := handled.Load(); n != 0 {
		t.Fatalf("✘ 回调已拒绝，handler 仍执行了 %d 次", n)
	}
}

// TestDispatchRecvParseFailureNoPanic 响应体解析失败时不得把 nil 交给业务回调。
func TestDispatchRecvParseFailureNoPanic(t *testing.T) {
	rpcNoneAuthEnv(t)
	client := &RpcClient{serverName: "s", fullAddr: "127.0.0.1:0", timeout: time.Second}
	client.SetCallbacks(NewClientCallbacks())

	raw := util.NewFSMessageWithID(125, 125, 1, wrapperspb.String("x"))
	if raw == nil {
		t.Fatal("构造响应失败")
	}
	// 125:125 未注册协议，解析必失败
	called := false
	client.dispatchRecv(125, 125, raw, func(proto.Message) { called = true })
	if called {
		t.Fatal("✘ 解析失败仍调用了业务回调")
	}
}

// TestIsErrorPacketDetection 客户端要能认出服务端的错误包。
func TestIsErrorPacketDetection(t *testing.T) {
	good := util.NewFSMessageWithID(126, 126, 1, wrapperspb.String("x"))
	if good == nil {
		t.Fatal("构造响应失败")
	}
	if isErrorPacket(good) {
		t.Fatal("✘ 正常响应被判成错误包")
	}
	bad := &message.FSMessage{Head: &message.Head{
		Protocol1: proto.Int32(126),
		Protocol2: proto.Int32(126),
		RequestId: proto.Uint64(1),
		Cmd:       proto.String(ErrorCmd),
	}}
	if !isErrorPacket(bad) {
		t.Fatal("✘ 错误包未被识别，业务回调会收到一个零值响应")
	}
	if isErrorPacket(nil) {
		t.Fatal("✘ nil 帧不应判为错误包")
	}
}

// TestConcurrentCallsGetOwnResponses 同一客户端并发多条同协议请求，
// 每条必须拿回自己的响应：request_id 丢失时响应会被投给任意等待者，直接串号。
func TestConcurrentCallsGetOwnResponses(t *testing.T) {
	rpcNoneAuthEnv(t)
	registerHandler(t, 129, 129, func(_ context.Context, msg *MessageInfo) proto.Message {
		req, ok := msg.Req.(*wrapperspb.StringValue)
		if !ok {
			return wrapperspb.String("bad-req")
		}
		return wrapperspb.String("echo:" + req.GetValue())
	})

	port := freePort(t)
	startServer(t, "mux-srv", port)
	client := dialClient(t, "mux-client", port)
	client.timeout = 15 * time.Second

	const n = 20
	var wg sync.WaitGroup
	var mismatches, missing atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		id := fmt.Sprintf("req-%03d", i)
		go func() {
			defer wg.Done()
			got := make(chan string, 1)
			client.SendMsg(129, 129, wrapperspb.String(id), func(pb proto.Message) {
				if sv, ok := pb.(*wrapperspb.StringValue); ok {
					got <- sv.GetValue()
				}
			})
			select {
			case v := <-got:
				if v != "echo:"+id {
					// 拿到别人的响应：并发下静默错单，比报错更危险
					mismatches.Add(1)
					t.Logf("✘ %s 拿到 %s", id, v)
				}
			default:
				missing.Add(1)
			}
		}()
	}
	wg.Wait()
	if m := mismatches.Load(); m != 0 {
		t.Fatalf("✘ %d 条响应串到了别的请求上", m)
	}
	if m := missing.Load(); m != 0 {
		t.Fatalf("✘ %d 条请求没拿到响应", m)
	}
}

// TestDispatchRecvCallsInterfaceParamCallback 业务回调按 proto.Message 接口形参声明时
// 必须真的被调用：旧实现用「类型相等」判定，接口与具体类型永不相等，回调形同虚设。
func TestDispatchRecvCallsInterfaceParamCallback(t *testing.T) {
	rpcNoneAuthEnv(t)
	util.RegisterProtocolType(127, 127, wrapperspb.String(""))

	client := &RpcClient{serverName: "s", fullAddr: "127.0.0.1:0", timeout: time.Second}
	client.SetCallbacks(NewClientCallbacks())

	raw := util.NewFSMessageWithID(127, 127, 1, wrapperspb.String("pong"))
	if raw == nil {
		t.Fatal("构造响应失败")
	}
	var got proto.Message
	client.dispatchRecv(127, 127, raw, func(pb proto.Message) { got = pb })

	sv, ok := got.(*wrapperspb.StringValue)
	if !ok || sv.GetValue() != "pong" {
		t.Fatalf("✘ 接口形参回调没被调用，got=%v", got)
	}
}

// TestStaticDiscoveryResolvesClientNames 静态发现也要登记 client 配置里的服务名。
func TestStaticDiscoveryResolvesClientNames(t *testing.T) {
	sd := NewStaticDiscovery(
		[]*config.RpcAddr{{Name: "srv-a", Addr: "10.0.0.1", Port: 9001}},
		[]*config.RpcAddr{{Name: "cli-b", Addr: "10.0.0.2", Port: 9002}},
	)
	for _, name := range []string{"srv-a", "cli-b"} {
		eps, err := sd.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("✘ Resolve(%s): %v（修复前 client 侧服务名查不到）", name, err)
		}
		if len(eps) != 1 || eps[0].Name != name {
			t.Fatalf("Resolve(%s) = %+v", name, eps)
		}
	}
}

// TestAuthenticateDefaultsToToken 未配置鉴权时按 token 处理，而不是静默放行。
func TestAuthenticateDefaultsToToken(t *testing.T) {
	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{}})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if status.Code(authenticate(ctx)) != codes.Unauthenticated {
		t.Fatal("✘ 未配置 rpc.auth 却放行了连接（默认应为 token，且无口令时拒绝）")
	}

	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{Auth: &config.RpcAuthConfig{Mode: "token"}}})
	if status.Code(authenticate(ctx)) != codes.Unauthenticated {
		t.Fatal("✘ token 模式未配口令却放行")
	}
	if authMode() != defaultAuthMode {
		t.Fatalf("默认模式 %q != %q", authMode(), defaultAuthMode)
	}
}
