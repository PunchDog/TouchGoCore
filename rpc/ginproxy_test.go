package rpc

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
	"touchgocore/gin"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"

	"google.golang.org/protobuf/proto"
)

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
	rsp := client.dispatchProxyHandler(&message.GinHTTPRequest{
		Method:         http.MethodGet,
		MatchedPattern: "/dp/:id",
		Params:         []*message.GinParam{{Key: "id", Value: "bob"}},
	})
	if rsp.GetStatus() != http.StatusOK || string(rsp.GetBody()) != "hi bob" {
		t.Fatalf("✘ 回调分发结果不符: status=%d body=%q", rsp.GetStatus(), rsp.GetBody())
	}

	// 无回调 -> 501。
	miss := client.dispatchProxyHandler(&message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/nope"})
	if miss.GetStatus() != http.StatusNotImplemented {
		t.Fatalf("✘ 无回调应 501，实际 %d", miss.GetStatus())
	}

	// panic 回调 -> 500（隔离崩溃，不让 panic 击穿 recvLoop）。
	RegisterGinProxyHandler("/boom|GET", func(*GinProxyRequest) *GinProxyResponse { panic("kaboom") })
	pan := client.dispatchProxyHandler(&message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/boom"})
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
	t.Cleanup(func() { gin.UnregisterGRPCRoutes("peer") })

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
