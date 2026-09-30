package rpc

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"touchgocore/gin"
	"touchgocore/network/message"

	"google.golang.org/protobuf/proto"
)

// ============================================================================
// 槽位 A 审查修复（A-F2/F3/F6）rpc 侧回归：
//   - A-F2：handleGinProxyFrame 注册前逐条复核格式，非法条目跳过不影响同帧合法条目；
//   - A-F3：过期会话（已被同名新会话顶替）的注册帧被拒；
//   - A-F6：dispatchProxyHandler 超时回 504；并发信号量超限直接回 503 代理响应；
//     drainBackground 正常路径返回 true（排空成功才可交还对象池）。
// ============================================================================

// qaARecvStream 客户端流替身：按脚本 Recv，Send 全记录。
// 自带一份而不复用 p[0-3]_* 阶段测试里的替身：那些文件被 .gitignore 排除，
// 长期回归用例必须在全新 clone 里能独立编译。
type qaARecvStream struct {
	message.Grpc_MsgClient
	mu   sync.Mutex
	recv []*message.FSMessage
	sent []*message.FSMessage
}

func (s *qaARecvStream) Recv() (*message.FSMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recv) == 0 {
		return nil, errors.New("qa stream exhausted")
	}
	m := s.recv[0]
	s.recv = s.recv[1:]
	return m, nil
}

func (s *qaARecvStream) Send(m *message.FSMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *qaARecvStream) sentFrames() []*message.FSMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*message.FSMessage(nil), s.sent...)
}

// qaAAck 取出记录帧里的注册回执。
func qaAAck(t *testing.T, frames []*message.FSMessage) *message.GinRegisterAck {
	t.Helper()
	if len(frames) != 1 {
		t.Fatalf("✘ 期望 1 帧回执，实际 %d", len(frames))
	}
	ack := &message.GinRegisterAck{}
	if err := proto.Unmarshal(frames[0].GetBody(), ack); err != nil {
		t.Fatalf("✘ 回执解析失败: %v", err)
	}
	return ack
}

// TestQaAServerRejectsInvalidRoutes A-F2：非法条目（无前导斜杠/小写方法/空路径）
// 被跳过并留痕，同帧合法条目照常注册，ack 只计合法条数。
func TestQaAServerRejectsInvalidRoutes(t *testing.T) {
	srv := gpLoopServer("qa-a-invalid", 8)
	st := &gpRecordStream{}
	cs := &clientSession{id: 7, stream: st}
	srv.nametoclientstream.Store("qa-peer-a", cs)
	t.Cleanup(func() { gin.UnregisterGRPCRoutes("qa-peer-a", cs.id) })

	reg := &message.GinRouteRegistration{Routes: []*message.GinRoute{
		{UrlPath: "/qa-good", Method: http.MethodGet},   // 合法
		{UrlPath: "no-slash", Method: http.MethodGet},   // 非法：缺前导斜杠
		{UrlPath: "/qa-bad-method", Method: "get"},      // 非法：小写方法
		{UrlPath: "/qa-good2", Method: http.MethodPost}, // 合法
		{UrlPath: "", Method: http.MethodGet},           // 非法：空路径
		{UrlPath: "/qa-any"},                            // 合法：空方法=通配
	}}
	body, err := proto.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	srv.handleGinProxyFrame(cs, "qa-peer-a", 11, ginSubRegister, newGinProxyFrame(ginSubRegister, 11, body))

	ack := qaAAck(t, st.frames())
	if !ack.GetOk() {
		t.Fatalf("✘ 含合法条目的注册帧应 ack ok，实际 msg=%q", ack.GetMsg())
	}
	if !strings.HasPrefix(ack.GetMsg(), "registered 3 routes") {
		t.Fatalf("✘ 应只注册 3 条合法路由，ack msg=%q", ack.GetMsg())
	}
}

// TestQaAStaleSessionRegistrationRejected A-F3：同名新会话已接管后，
// 旧会话迟到的注册帧必须被拒（否则条目 session 代际被写回旧值，
// 旧会话随后的双键回收会误删，新会话路由静默 404）。
func TestQaAStaleSessionRegistrationRejected(t *testing.T) {
	srv := gpLoopServer("qa-a-stale", 8)
	stOld := &gpRecordStream{}
	stNew := &gpRecordStream{}
	oldSess := &clientSession{id: 1, stream: stOld}
	newSess := &clientSession{id: 2, stream: stNew}
	// 新会话已接管注册表
	srv.nametoclientstream.Store("qa-peer-b", newSess)

	reg := &message.GinRouteRegistration{Routes: []*message.GinRoute{
		{UrlPath: "/qa-stale", Method: http.MethodGet},
	}}
	body, _ := proto.Marshal(reg)
	srv.handleGinProxyFrame(oldSess, "qa-peer-b", 5, ginSubRegister, newGinProxyFrame(ginSubRegister, 5, body))

	ack := qaAAck(t, stOld.frames())
	if ack.GetOk() {
		t.Fatalf("✘ 过期会话注册应被拒，实际 ok=true msg=%q", ack.GetMsg())
	}
	if len(stNew.frames()) != 0 {
		t.Fatal("✘ 过期会话注册不应给新会话发帧")
	}

	// 当前会话自己注册仍然放行
	srv.handleGinProxyFrame(newSess, "qa-peer-b", 6, ginSubRegister, newGinProxyFrame(ginSubRegister, 6, body))
	ack = qaAAck(t, stNew.frames())
	if !ack.GetOk() || ack.GetMsg() != "registered 1 routes" {
		t.Fatalf("✘ 当前会话注册应成功，实际 ok=%v msg=%q", ack.GetOk(), ack.GetMsg())
	}
	gin.UnregisterGRPCRoutes("qa-peer-b", newSess.id)
}

// TestQaADispatchProxyHandlerTimeout A-F6：ctx 超时后回 504，不再无限等待卡死回调。
func TestQaADispatchProxyHandlerTimeout(t *testing.T) {
	client := &RpcClient{fullAddr: "127.0.0.1:0", serverName: "qa-a"}
	RegisterGinProxyHandler("/qa-slow|GET", func(*GinProxyRequest) *GinProxyResponse {
		time.Sleep(500 * time.Millisecond)
		return &GinProxyResponse{Status: http.StatusOK}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	rsp := client.dispatchProxyHandler(ctx, &message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/qa-slow"})
	if rsp.GetStatus() != http.StatusGatewayTimeout {
		t.Fatalf("✘ 超时应回 504，实际 %d", rsp.GetStatus())
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("✘ dispatchProxyHandler 未在 ctx 超时后立即返回（耗时 %v）", elapsed)
	}

	// 超时必须明显短于服务端 15s 口径，保证赶在服务端放弃前回帧
	if proxyHandlerTimeout >= 15*time.Second {
		t.Fatalf("✘ proxyHandlerTimeout=%v 应短于服务端 15s", proxyHandlerTimeout)
	}
}

// TestQaAProxyConcurrencyOverload A-F6：信号量满时 recvLoop 不再起 goroutine，
// 直接回 503 语义的代理响应帧，回调不被执行。
func TestQaAProxyConcurrencyOverload(t *testing.T) {
	handlerCalled := false
	RegisterGinProxyHandler("/qa-overload|GET", func(*GinProxyRequest) *GinProxyResponse {
		handlerCalled = true
		return &GinProxyResponse{Status: http.StatusOK}
	})

	c := &RpcClient{fullAddr: "127.0.0.1:0", serverName: "qa-a"}
	sem := c.proxySemaphore()
	if cap(sem) != defaultProxyConcurrency {
		t.Fatalf("✘ 信号量容量应为 %d，实际 %d", defaultProxyConcurrency, cap(sem))
	}
	// 占满全部令牌，模拟 64 个在跑的代理 handler
	for i := 0; i < defaultProxyConcurrency; i++ {
		sem <- struct{}{}
	}
	defer func() {
		for i := 0; i < defaultProxyConcurrency; i++ {
			<-sem
		}
	}()

	reqBody, _ := proto.Marshal(&message.GinHTTPRequest{Method: http.MethodGet, UrlPath: "/qa-overload", MatchedPattern: "/qa-overload"})
	st := &qaARecvStream{recv: []*message.FSMessage{
		newGinProxyFrame(ginSubProxyReq, 77, reqBody),
	}}
	c.stream.Store(st)
	c.recvLoop(st) // 第 2 次 Recv 报错结束循环

	frames := st.sentFrames()
	if len(frames) != 1 {
		t.Fatalf("✘ 应回 1 帧 503 代理响应，实际 %d 帧", len(frames))
	}
	f := frames[0]
	if f.GetHead().GetProtocol2() != ginSubProxyResp || f.GetHead().GetRequestId() != 77 {
		t.Fatalf("✘ 回帧子码/request_id 不符: %d/%d", f.GetHead().GetProtocol2(), f.GetHead().GetRequestId())
	}
	rsp := &message.GinHTTPResponse{}
	if err := proto.Unmarshal(f.GetBody(), rsp); err != nil {
		t.Fatalf("✘ 回帧解析失败: %v", err)
	}
	if rsp.GetStatus() != http.StatusServiceUnavailable {
		t.Fatalf("✘ 过载应回 503，实际 %d", rsp.GetStatus())
	}
	time.Sleep(20 * time.Millisecond)
	if handlerCalled {
		t.Fatal("✘ 过载路径不应执行回调")
	}
}

// TestQaADrainBackgroundEmpty A-F6：无后台协程时排空立即成功（Close 才允许交还对象池）。
func TestQaADrainBackgroundEmpty(t *testing.T) {
	c := &RpcClient{fullAddr: "127.0.0.1:0", serverName: "qa-a"}
	if !c.drainBackground() {
		t.Fatal("✘ 空 bgWG 排空应返回 true")
	}
}
