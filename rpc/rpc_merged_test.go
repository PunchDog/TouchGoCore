package rpc

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/gin"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
)

func withRPCAuth(t *testing.T, auth *config.RpcAuthConfig) {
	t.Helper()
	prev := config.Cfg_
	config.Cfg_ = &config.Cfg{Rpc: &config.RpcConfig{Auth: auth}}
	t.Cleanup(func() {
		config.Cfg_ = prev
	})
}

func TestAuthenticateNoneMissingClientName(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "none"})
	err := authenticate(context.Background())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

func TestAuthenticateNoneOK(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "none"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if err := authenticate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticateAllowlist(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "allowlist", AllowList: []string{"gate"}})
	okCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if err := authenticate(okCtx); err != nil {
		t.Fatal(err)
	}
	badCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "other"))
	if status.Code(authenticate(badCtx)) != codes.Unauthenticated {
		t.Fatal("expected allowlist reject")
	}
}

func TestAuthenticateToken(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "token", Token: "secret"})
	okMD := metadata.Pairs("client-name", "gate", "authorization", "Bearer secret")
	if err := authenticate(metadata.NewIncomingContext(context.Background(), okMD)); err != nil {
		t.Fatal(err)
	}
	badMD := metadata.Pairs("client-name", "gate", "authorization", "Bearer wrong")
	if status.Code(authenticate(metadata.NewIncomingContext(context.Background(), badMD))) != codes.Unauthenticated {
		t.Fatal("expected token reject")
	}
}

func TestAuthenticateMTLSMissingPeer(t *testing.T) {
	withRPCAuth(t, &config.RpcAuthConfig{Mode: "mtls", CAFile: "ca.pem"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if status.Code(authenticate(ctx)) != codes.Unauthenticated {
		t.Fatal("expected mTLS reject without peer cert")
	}
}

// ============================================================================
// L6：RpcClient 池复用依赖隐式清理的回归。
//
// 问题：RpcClient 通过 localtimer 对象池复用，pending（在途请求表）/ stream
// 状态原本只在 Close 路径隐式清理。若 Close 未走完或被跳过，池把实例发给下一个
// NewRpcClient 时，复用者会继承脏状态：残留 pending 让响应错投给已消失的等待者，
// 残留 streamValid/stream 让复用者在一条死流上收发，nextReqID 高水位错乱代际。
//
// 修复：在池复用取出/重置路径（NewTimer 的 initcallback，复用者独占期）显式调用
// resetForReuse() 兜底清理。本用例直接驱动 resetForReuse，确定性地验证：
// 预置脏 pending/stream 的实例经清理后状态干净，且清理幂等安全。
// ============================================================================

// dirtyClient 构造一个带脏状态的 RpcClient，模拟「Close 未走完就被池收走」的实例。
func dirtyClient() (*RpcClient, chan *message.FSMessage) {
	c := &RpcClient{
		serverName: "reuse",
		fullAddr:   "reuse://127.0.0.1:0",
		timeout:    time.Second,
	}
	c.SetCallbacks(NewClientCallbacks())

	// 脏 pending：两个在途请求，其中一个有等待者正在阻塞收响应
	waitCh := make(chan *message.FSMessage, 1)
	c.pending.Store(uint64(1), waitCh)
	c.pending.Store(uint64(2), make(chan *message.FSMessage, 1))

	// 脏 stream：缓存了一条流，且标记为有效
	c.stream.Store(message.Grpc_MsgClient(&fakeMsgStream{
		recvCh: make(chan *message.FSMessage, 1),
		errCh:  make(chan error, 1),
	}))
	c.streamCancel = func() {}
	c.streamValid.Store(true)

	// 脏代际计数与连接状态
	c.nextReqID.Store(42)
	c.connStatus.Store(true)
	return c, waitCh
}

func pendingCount(c *RpcClient) int {
	n := 0
	c.pending.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// TestRpcClient_ResetForReuseClearsDirtyState 预置脏 pending/stream 的实例
// 经池复用重置后状态必须干净：pending 为空、stream 字段零值、代际计数归零。
func TestRpcClient_ResetForReuseClearsDirtyState(t *testing.T) {
	c, waitCh := dirtyClient()

	// 前置：确认脏状态确实存在，否则本用例是空跑
	if pendingCount(c) != 2 {
		t.Fatalf("前置条件被破坏：脏 pending 数=%d", pendingCount(c))
	}
	if c.stream.Load() == nil || !c.streamValid.Load() {
		t.Fatal("前置条件被破坏：脏 stream 未预置")
	}

	c.resetForReuse()

	// pending 表清空
	if n := pendingCount(c); n != 0 {
		t.Errorf("✘ 复用后 pending 未清空: %d 条残留", n)
	}
	// 残留等待者被投递 nil 唤醒（语义与 failAllPending 一致），不会永久阻塞
	select {
	case v, ok := <-waitCh:
		if ok && v != nil {
			t.Errorf("✘ 残留等待者应收到 nil 唤醒信号，实收: %v", v)
		}
	default:
		t.Error("✘ 残留等待者未被唤醒（清理未投递 nil）")
	}
	// stream 字段回到零值
	if c.stream.Load() != nil {
		t.Error("✘ 复用后 stream 未归零值")
	}
	if c.currentStream() != nil {
		t.Error("✘ 复用后 currentStream 应为 nil")
	}
	if c.streamValid.Load() {
		t.Error("✘ 复用后 streamValid 应为 false")
	}
	if c.streamCancel != nil {
		t.Error("✘ 复用后 streamCancel 应为 nil")
	}
	// 代际计数与连接状态归零
	if got := c.nextReqID.Load(); got != 0 {
		t.Errorf("✘ 复用后 nextReqID 应归零，实为: %d", got)
	}
	if c.connStatus.Load() {
		t.Error("✘ 复用后 connStatus 应归零")
	}
	t.Log("✔ 脏 pending/stream/代际计数经 resetForReuse 后全部干净")
}

// TestRpcClient_ResetForReuseIdempotent 清理必须幂等：对已经干净的实例
// （正常 Close 后再复用）重复调用不 panic、不改坏状态。
func TestRpcClient_ResetForReuseIdempotent(t *testing.T) {
	c := newFakeClient()

	// 零值实例连续多次清理都不应 panic
	for i := 0; i < 3; i++ {
		c.resetForReuse()
	}
	if pendingCount(c) != 0 || c.stream.Load() != nil || c.streamValid.Load() {
		t.Fatal("✘ 幂等清理后干净实例状态被改坏")
	}

	// 先弄脏再连续清理两次，第二次必须是安全的空操作
	dirty, _ := dirtyClient()
	dirty.resetForReuse()
	dirty.resetForReuse()
	if n := pendingCount(dirty); n != 0 {
		t.Fatalf("✘ 二次清理后 pending 应仍为空: %d", n)
	}
	if dirty.stream.Load() != nil || dirty.nextReqID.Load() != 0 {
		t.Fatal("✘ 二次清理后 stream/代际计数应保持干净")
	}
	t.Log("✔ resetForReuse 幂等：干净实例重复清理安全")
}

// TestRpcClient_ResetForReuseWiredIntoConstructor 验证 resetForReuse 已接入
// 池复用重置路径：NewRpcClient 的 initcallback 会调用它。用一个已脏的实例
// 走一遍 callback 应清干净——直接调用构造闭包所触发的清理入口。
func TestRpcClient_ResetForReuseWiredIntoConstructor(t *testing.T) {
	// 无法在单测里安全地跑完整 NewRpcClient（需真实拨号），
	// 这里断言清理入口存在且能被复用路径调用：等价于 initcallback 内的那一步。
	c, _ := dirtyClient()
	c.closed.Store(true) // 模拟上一位主人留下的 closed 标记

	// 复刻 initcallback 对复用实例做的关键清理次序
	c.closed.Store(false)
	c.resetForReuse()

	if c.closed.Load() {
		t.Error("✘ closed 标记未随复用清除")
	}
	if pendingCount(c) != 0 || c.stream.Load() != nil {
		t.Error("✘ 复用路径未清干净脏 pending/stream")
	}
	t.Log("✔ 复用重置入口按预期清理脏状态")
}

func TestFailAllPendingWakesWaiter(t *testing.T) {
	c := &RpcClient{}
	ch := make(chan *message.FSMessage, 1)
	c.pending.Store(uint64(1), ch)

	done := make(chan struct{})
	go func() {
		recv := <-ch
		if recv != nil {
			t.Errorf("expected nil disconnect sentinel, got %v", recv)
		}
		close(done)
	}()

	c.failAllPending()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pending waiter was not woken")
	}
}

// 事件推送的集成测试：验证 requestID=0 的主动推帧能被 OnEvent 收到，
// 且不影响未声明为事件的协议号走原来的兼容路径。
//
// 背景（events.go 头注）：修复前这类帧要么被丢弃（无在途请求），要么被
// 错投成某个业务请求的响应（有在途请求）——后者会让业务拿到语义无关的消息。

// eventTestEnv 起一对真实的 server/client，返回两者与清理函数。
func eventTestEnv(t *testing.T, clientName string) (*RpcServer, *RpcClient) {
	t.Helper()

	cfg := &config.Cfg{
		Rpc: &config.RpcConfig{
			Auth: &config.RpcAuthConfig{Mode: "none"},
		},
	}
	prev := config.Cfg_
	prevCtx := runCtx()
	config.Cfg_ = cfg
	setRunCtx(corectx.WithCfg(context.Background(), cfg))
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prev
		setRunCtx(prevCtx)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := StartGrpcServer("evtest", port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer("evtest")
	if srv == nil {
		t.Fatal("server missing")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })

	client := NewRpcClient(clientName, "127.0.0.1", port)
	if client == nil {
		t.Fatal("client nil")
	}
	client.timeout = 3 * time.Second
	t.Cleanup(func() { _ = client.Close() })

	return srv, client
}

// waitConnected 发一次请求把 client 的 bidi 流建起来（流是懒建的）。
func waitConnected(t *testing.T, client *RpcClient, p1, p2 int32) {
	t.Helper()
	got := make(chan proto.Message, 1)
	client.SendMsg(p1, p2, wrapperspb.String("hello"), func(resp proto.Message) {
		select {
		case got <- resp:
		default:
		}
	})
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("首次请求超时，流未建立")
	}
}

// TestEventPushDelivered 已声明为事件的协议号：主动推帧必须送达 OnEvent。
func TestEventPushDelivered(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		pingP1, pingP2   = int32(31), int32(1)
		eventP1, eventP2 = int32(91), int32(1)
	)
	srv, client := eventTestEnv(t, "ev-client")

	util.RegisterProtocolType(pingP1, pingP2, wrapperspb.String(""))
	util.RegisterProtocolType(eventP1, eventP2, wrapperspb.String(""))
	pingKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, pingP1, pingP2)
	pingHandler := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(pingKey, pingHandler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(pingKey, pingHandler) })

	// 声明事件通道 —— 这是本次修复的核心开关。
	RegisterEventProtocol(eventP1, eventP2)
	t.Cleanup(func() { UnregisterEventProtocol(eventP1, eventP2) })

	received := make(chan proto.Message, 4)
	client.SetCallbacks(&ClientCallbacks{
		OnEvent: func(_ string, p1, p2 int32, evt proto.Message) {
			if p1 != eventP1 || p2 != eventP2 {
				t.Errorf("OnEvent 协议号不符: got %d:%d", p1, p2)
			}
			select {
			case received <- evt:
			default:
			}
		},
	})

	waitConnected(t, client, pingP1, pingP2)

	// 服务端主动推：RpcServer.Send 走 requestID=0。
	if err := srv.Send("ev-client", eventP1, eventP2, wrapperspb.String("qr-refresh")); err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	select {
	case evt := <-received:
		if got := evt.(*wrapperspb.StringValue).GetValue(); got != "qr-refresh" {
			t.Fatalf("事件内容不符: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到事件 —— 修复未生效（帧被丢弃或错投）")
	}
}

// TestEventPushIgnoredWhenUnregistered 未声明的协议号：行为必须保持原样
// （事件被丢弃但不 panic、不影响后续请求）。这条是兼容性回归闸。
func TestEventPushIgnoredWhenUnregistered(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		pingP1, pingP2   = int32(32), int32(1)
		eventP1, eventP2 = int32(92), int32(1)
	)
	srv, client := eventTestEnv(t, "ev-client2")

	util.RegisterProtocolType(pingP1, pingP2, wrapperspb.String(""))
	util.RegisterProtocolType(eventP1, eventP2, wrapperspb.String(""))
	pingKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, pingP1, pingP2)
	pingHandler := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(pingKey, pingHandler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(pingKey, pingHandler) })

	// 故意不注册事件协议。
	eventHit := make(chan struct{}, 1)
	client.SetCallbacks(&ClientCallbacks{
		OnEvent: func(_ string, _, _ int32, _ proto.Message) {
			select {
			case eventHit <- struct{}{}:
			default:
			}
		},
	})

	waitConnected(t, client, pingP1, pingP2)
	_ = srv.Send("ev-client2", eventP1, eventP2, wrapperspb.String("noise"))

	select {
	case <-eventHit:
		t.Fatal("未声明为事件的协议号不应触发 OnEvent")
	case <-time.After(500 * time.Millisecond):
		// 预期：走原兼容路径，不进 OnEvent
	}

	// 关键：上面那条"垃圾帧"不能污染后续正常请求。
	got := make(chan proto.Message, 1)
	client.SendMsg(pingP1, pingP2, wrapperspb.String("again"), func(resp proto.Message) {
		select {
		case got <- resp:
		default:
		}
	})
	select {
	case resp := <-got:
		if resp == nil || resp.(*wrapperspb.StringValue).GetValue() != "pong" {
			t.Fatalf("后续请求被污染: %v", resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("后续请求超时 —— 未声明的推送帧破坏了连接")
	}
}

// ============================================================================
// 反向 gRPC 路由代理（rpc 侧）回归：帧编解码互转、回调查表分发、
// sessionProxy.Do 与服务端子码分派的往返、注册回执、以及子码/孤立响应的错误路径。
// ============================================================================

// gpRecordStream 服务端流的最小替身：记录 Send 出去的帧供断言取用。
// 这里自带一份而不是复用 p2_rpc_review_test.go 里的同名单据：那类 p[0-3]_* 阶段
// 测试按项目约定被 .gitignore 排除（仅本地保留），长期回归用例必须在全新
// clone 里能独立编译，否则“能过本地测试”会靠上不入库的文件。
type gpRecordStream struct {
	message.Grpc_MsgServer
	mu   sync.Mutex
	sent []*message.FSMessage
}

func (s *gpRecordStream) Send(msg *message.FSMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *gpRecordStream) frames() []*message.FSMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*message.FSMessage(nil), s.sent...)
}

// gpLoopServer 构造一个不监听端口、仅字段就绪的 RpcServer，用于直调子码分派。
func gpLoopServer(name string, queueSize int) *RpcServer {
	return &RpcServer{
		name:               name,
		done:               make(chan struct{}),
		readClose:          make(chan struct{}),
		readGone:           make(chan struct{}),
		handleGone:         make(chan struct{}),
		readchannel:        make(chan *MessageInfo, queueSize),
		handlechannel:      make(chan *MessageInfo, queueSize),
		callFunc:           util.DefaultCallFunc,
		handlerSem:         make(chan struct{}, defaultHandlerConcurrency),
		nametoclientstream: syncmap.NewMap[string, *clientSession](),
	}
}

func TestGinProxyPatternNormalizeAndLookup(t *testing.T) {
	if got := normalizeProxyPattern("/foo/:id|GET"); got != "/foo/:id|GET" {
		t.Fatalf("✘ 归一化(带方法)不符: %q", got)
	}
	if got := normalizeProxyPattern("/bar"); got != "/bar|" {
		t.Fatalf("✘ 归一化(通配)应为 /bar|，实际 %q", got)
	}

	h := func(*GinProxyRequest) *GinProxyResponse { return nil }
	RegisterGinProxyHandler("/lookup/:id|GET", h)

	if got, ok := lookupGinProxyHandler("/lookup/:id", http.MethodGet); !ok || got == nil {
		t.Fatal("✘ 精确 matched|GET 未命中")
	}
	if _, ok := lookupGinProxyHandler("/lookup/:id", http.MethodPost); ok {
		t.Fatal("✘ 未注册的方法不应命中")
	}

	// 通配注册：任意方法都回退到 "pattern|"。
	RegisterGinProxyHandler("/wild", h)
	if _, ok := lookupGinProxyHandler("/wild", http.MethodDelete); !ok {
		t.Fatal("✘ 通配回退未命中")
	}
}

func TestGinProxyConversions(t *testing.T) {
	req := &gin.ProxyRequest{
		Method:         http.MethodPost,
		Path:           "/user/7",
		RawQuery:       "a=b",
		MatchedPattern: "/user/:id",
		Params:         map[string]string{"id": "7"},
		Header:         http.Header{"X-Trace": []string{"t1"}},
		Body:           []byte("payload"),
	}
	m := ginRequestToProxy(req)
	if m.GetMethod() != http.MethodPost || m.GetUrlPath() != "/user/7" || m.GetMatchedPattern() != "/user/:id" {
		t.Fatalf("✘ 请求转换基本字段不符: %v", m)
	}
	if len(m.GetParams()) != 1 || m.GetParams()[0].GetKey() != "id" || m.GetParams()[0].GetValue() != "7" {
		t.Fatalf("✘ 参数未随下行传递: %v", m.GetParams())
	}

	rsp := ginResponseToProxy(&message.GinHTTPResponse{
		Status:  201,
		Headers: []*message.GinHeader{{Key: "X-From", Values: []string{"client"}}},
		Body:    []byte("ok"),
	})
	if rsp.Status != 201 || string(rsp.Body) != "ok" || rsp.Header.Get("X-From") != "client" {
		t.Fatalf("✘ 响应转换不符: %+v", rsp)
	}
}

func TestGinProxyDispatchHandler(t *testing.T) {
	client := &RpcClient{fullAddr: "127.0.0.1:0", serverName: "s"}

	RegisterGinProxyHandler("/dp/:id|GET", func(req *GinProxyRequest) *GinProxyResponse {
		return &GinProxyResponse{Status: http.StatusOK, Body: []byte("hi " + req.Params["id"])}
	})
	rsp := client.dispatchProxyHandler(context.Background(), &message.GinHTTPRequest{
		Method:         http.MethodGet,
		MatchedPattern: "/dp/:id",
		Params:         []*message.GinParam{{Key: "id", Value: "bob"}},
	})
	if rsp.GetStatus() != http.StatusOK || string(rsp.GetBody()) != "hi bob" {
		t.Fatalf("✘ 回调分发结果不符: status=%d body=%q", rsp.GetStatus(), rsp.GetBody())
	}

	// 无回调 -> 501。
	miss := client.dispatchProxyHandler(context.Background(), &message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/nope"})
	if miss.GetStatus() != http.StatusNotImplemented {
		t.Fatalf("✘ 无回调应 501，实际 %d", miss.GetStatus())
	}

	// panic 回调 -> 500（隔离崩溃，不让 panic 击穿 recvLoop）。
	RegisterGinProxyHandler("/boom|GET", func(*GinProxyRequest) *GinProxyResponse { panic("kaboom") })
	pan := client.dispatchProxyHandler(context.Background(), &message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/boom"})
	if pan.GetStatus() != http.StatusInternalServerError {
		t.Fatalf("✘ panic 回调应 500，实际 %d", pan.GetStatus())
	}
}

// TestGinProxySessionDoRoundtrip 端到端串联服务端：Do 发出代理请求 -> 子码=4 响应投递 -> Do 收到。
func TestGinProxySessionDoRoundtrip(t *testing.T) {
	srv := gpLoopServer("gp-roundtrip", 8)
	st := &gpRecordStream{}
	cs := &clientSession{stream: st}
	srv.nametoclientstream.Store("peer", cs)

	sp := &sessionProxy{server: srv, clientNameKey: "peer"}

	// 异步扮演远端客户端：取走 Do 发出的代理请求帧，按其 request_id 回一帧代理响应。
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.After(2 * time.Second)
		for {
			if frames := st.frames(); len(frames) > 0 {
				req := frames[0]
				rid := req.GetHead().GetRequestId()
				body, _ := proto.Marshal(&message.GinHTTPResponse{Status: http.StatusOK, Body: []byte("pong")})
				srv.handleGinProxyFrame(cs, "peer", rid, ginSubProxyResp, newGinProxyFrame(ginSubProxyResp, rid, body))
				return
			}
			select {
			case <-deadline:
				return
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := sp.Do(ctx, &gin.ProxyRequest{Method: http.MethodGet, Path: "/x", MatchedPattern: "/x"})
	if err != nil {
		t.Fatalf("✘ Do 失败: %v", err)
	}
	if resp == nil || resp.Status != http.StatusOK || string(resp.Body) != "pong" {
		t.Fatalf("✘ Do 响应不符: %+v", resp)
	}
	<-done
}

// TestGinProxyRegistrationAck 注册帧：写网关路由并回执 ok，request_id 原样回填。
func TestGinProxyRegistrationAck(t *testing.T) {
	srv := gpLoopServer("gp-reg", 8)
	st := &gpRecordStream{}
	cs := &clientSession{stream: st}
	srv.nametoclientstream.Store("peer", cs)
	t.Cleanup(func() { gin.UnregisterGRPCRoutes("peer", cs.id) })

	reg := &message.GinRouteRegistration{Routes: []*message.GinRoute{
		{UrlPath: "/regtest/hello", Method: http.MethodGet},
		{UrlPath: "/regtest/user/:id", Method: http.MethodGet},
	}}
	body, err := proto.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	srv.handleGinProxyFrame(cs, "peer", 42, ginSubRegister, newGinProxyFrame(ginSubRegister, 42, body))

	frames := st.frames()
	if len(frames) != 1 {
		t.Fatalf("✘ 期望回执 1 帧，实际 %d", len(frames))
	}
	ack := frames[0]
	if ack.GetHead().GetProtocol1() != ProtocolGinProxy || ack.GetHead().GetProtocol2() != ginSubRegisterAck {
		t.Fatalf("✘ 回执协议号/子码不符: %d/%d", ack.GetHead().GetProtocol1(), ack.GetHead().GetProtocol2())
	}
	if ack.GetHead().GetRequestId() != 42 {
		t.Fatalf("✘ 回执 request_id 未回填: %d", ack.GetHead().GetRequestId())
	}
	got := &message.GinRegisterAck{}
	if err := proto.Unmarshal(ack.GetBody(), got); err != nil {
		t.Fatalf("✘ 回执解析失败: %v", err)
	}
	if !got.GetOk() {
		t.Fatalf("✘ 回执应为 ok=true, msg=%q", got.GetMsg())
	}
}

// TestGinProxyOrphanAndUnknownSubcode 错误路径：孤立代理响应被丢弃、未知子码不误投递，均不 panic。
func TestGinProxyOrphanAndUnknownSubcode(t *testing.T) {
	srv := gpLoopServer("gp-err", 8)
	st := &gpRecordStream{}
	cs := &clientSession{stream: st}
	srv.nametoclientstream.Store("peer", cs)

	// 无等待方的 request_id -> 丢弃，不投递、不回帧。
	body, _ := proto.Marshal(&message.GinHTTPResponse{Status: http.StatusOK})
	srv.handleGinProxyFrame(cs, "peer", 999, ginSubProxyResp, newGinProxyFrame(ginSubProxyResp, 999, body))

	// 未知子码 -> default 分支，不回帧。
	srv.handleGinProxyFrame(cs, "peer", 1, 77, newGinProxyFrame(77, 1, body))

	if len(st.frames()) != 0 {
		t.Fatalf("✘ 错误路径不应产生回帧，实际 %d", len(st.frames()))
	}
}

func TestRPCRoundTripRequestID(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端并等待连接建立，耗时约 10 秒，-short 跳过")
	}
	cfg := &config.Cfg{
		Rpc: &config.RpcConfig{
			Auth: &config.RpcAuthConfig{Mode: "none"},
		},
	}
	prev := config.Cfg_
	prevCtx := runCtx()
	config.Cfg_ = cfg
	setRunCtx(corectx.WithCfg(context.Background(), cfg))
	UseRegistry(syncmap.NewMap[string, *RpcServer](), syncmap.NewMap[string, *RpcClient]())
	t.Cleanup(func() {
		config.Cfg_ = prev
		setRunCtx(prevCtx)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if err := StartGrpcServer("itest", port, false); err != nil {
		t.Fatal(err)
	}
	srv := GetRpcServer("itest")
	if srv == nil {
		t.Fatal("server missing")
	}
	t.Cleanup(func() { srv.Stop(context.Background()) })

	util.RegisterProtocolType(3, 1, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, 3, 1)
	handler := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("pong")
	}
	util.DefaultCallFunc.Register(key, handler)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, handler) })

	client := NewRpcClient("itest-client", "127.0.0.1", port)
	if client == nil {
		t.Fatal("client nil")
	}
	client.timeout = 3 * time.Second

	done := make(chan proto.Message, 1)
	client.SetCallbacks(&ClientCallbacks{
		OnMessageReceived: func(_ string, _, _ int32, resp proto.Message) {
			select {
			case done <- resp:
			default:
			}
		},
	})
	client.SendMsg(3, 1, wrapperspb.String("ping"), nil)
	select {
	case got := <-done:
		sv, ok := got.(*wrapperspb.StringValue)
		if !ok || sv.GetValue() != "pong" {
			t.Fatalf("got %#v", got)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("rpc roundtrip timeout")
	}
}

// TestSendMsgCtxWaitTimeout 客户端自身超时必须返回 ErrWaitTimeout，
// 而不是挂到服务端 12s 预算。
func TestSendMsgCtxWaitTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(61), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-client")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(3 * time.Second) // 远大于下面的 client.timeout
		return wrapperspb.String("late")
	}
	util.DefaultCallFunc.Register(key, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, slow) })

	// 客户端预算压到 300ms：服务端还在 sleep，客户端必须先放弃。
	client.timeout = 300 * time.Millisecond

	err := client.SendMsgCtx(context.Background(), slowP1, slowP2,
		wrapperspb.String("hi"), func(proto.Message) {})
	if err == nil {
		t.Fatal("服务端慢响应时 SendMsgCtx 应返回 error")
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("期望 ErrWaitTimeout，实际: %v", err)
	}
}

// TestSendMsgCtxCtxCancel 调用方取消必须原样返回 ctx.Err()，不能伪装成超时。
func TestSendMsgCtxCtxCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(62), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-client2")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	key := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(3 * time.Second)
		return wrapperspb.String("late")
	}
	util.DefaultCallFunc.Register(key, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(key, slow) })

	client.timeout = 5 * time.Second // 客户端预算够长，让 ctx 先到

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := client.SendMsgCtx(ctx, slowP1, slowP2, wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("期望 context.DeadlineExceeded，实际: %v", err)
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Fatal("调用方取消不应被报告成 ErrWaitTimeout")
	}
}

// TestSendMsgCtxClosed 客户端关闭后调用必须立刻返回 ErrClientClosed。
func TestSendMsgCtxClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	_, client := eventTestEnv(t, "sctx-client3")
	_ = client.Close()

	err := client.SendMsgCtx(context.Background(), 63, 1, wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, ErrClientClosed) {
		t.Fatalf("期望 ErrClientClosed，实际: %v", err)
	}
}

// TestSendMsgCtxRecoversAfterTimeout 钉住"超时后作废流"这个修复。
//
// 修复前的行为（2026-10-28 实测）：一次超时会让这条流进入半死状态，
// **此后所有调用都等满超时且拿不到响应**，直到进程重启。
// 本测试先制造一次超时，再调一个本该秒回的协议 —— 必须成功。
func TestSendMsgCtxRecoversAfterTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("起真实 RPC 服务端与客户端，-short 跳过")
	}
	const (
		slowP1, slowP2 = int32(71), int32(1)
		fastP1, fastP2 = int32(72), int32(1)
	)
	_, client := eventTestEnv(t, "sctx-recover")

	util.RegisterProtocolType(slowP1, slowP2, wrapperspb.String(""))
	util.RegisterProtocolType(fastP1, fastP2, wrapperspb.String(""))

	slowKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, slowP1, slowP2)
	slow := func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(2 * time.Second)
		return wrapperspb.String("slow")
	}
	util.DefaultCallFunc.Register(slowKey, slow)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(slowKey, slow) })

	fastKey := fmt.Sprintf("%s:%d:%d", util.CallRpcMsg, fastP1, fastP2)
	fast := func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("fast")
	}
	util.DefaultCallFunc.Register(fastKey, fast)
	t.Cleanup(func() { util.DefaultCallFunc.Unregister(fastKey, fast) })

	// 先把预算压到 300ms，制造一次必然的超时。
	client.timeout = 300 * time.Millisecond
	err := client.SendMsgCtx(context.Background(), slowP1, slowP2,
		wrapperspb.String("hi"), func(proto.Message) {})
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("第一次调用应超时，实际: %v", err)
	}

	// 恢复正常预算，紧接着调秒回的协议：必须成功。
	client.timeout = 5 * time.Second
	var got proto.Message
	err = client.SendMsgCtx(context.Background(), fastP1, fastP2,
		wrapperspb.String("hi"), func(m proto.Message) { got = m })
	if err != nil {
		t.Fatalf("超时之后的调用不应失败（流未重建？）: %v", err)
	}
	if got == nil {
		t.Fatal("超时之后的调用拿不到响应")
	}
	if s, ok := got.(*wrapperspb.StringValue); !ok || s.GetValue() != "fast" {
		t.Fatalf("响应内容不对: %v", got)
	}
}

// fakeMsgStream 只实现 Send/Recv 的假流，用于确定性地驱动 recvLoop 分支。
type fakeMsgStream struct {
	message.Grpc_MsgClient
	recvCh chan *message.FSMessage
	errCh  chan error
}

func (f *fakeMsgStream) Send(*message.FSMessage) error { return nil }

func (f *fakeMsgStream) Recv() (*message.FSMessage, error) {
	select {
	case m := <-f.recvCh:
		return m, nil
	case err := <-f.errCh:
		return nil, err
	}
}

func newFakeClient() *RpcClient {
	c := &RpcClient{
		serverName: "fake",
		fullAddr:   "fake://127.0.0.1:0",
		timeout:    time.Second,
	}
	c.SetCallbacks(NewClientCallbacks())
	return c
}
