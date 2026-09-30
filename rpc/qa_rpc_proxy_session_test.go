package rpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
	"touchgocore/gin"
	"touchgocore/network/message"

	"google.golang.org/protobuf/proto"
)

// ============================================================================
// 槽位 RPC 复核修复回归（T6-G）：
//   - P2-1：注册被拒条目不得计入 ack 成功数（ack 明细带被拒 path）；
//   - P2-2：同名新会话顶替时清理旧会话代际的残留路由条目；
//   - P3-1：代理回调超时（504）后孤儿 goroutine 纳入 bgWG 计数。
// ============================================================================

// qaRpcNopStream gin.GRPCProxyStream 的最小替身（只做属主占位，不接请求）。
type qaRpcNopStream struct{}

func (qaRpcNopStream) Do(context.Context, *gin.ProxyRequest) (*gin.ProxyResponse, error) {
	return nil, nil
}

// qaRpcRegFrame 组一帧注册消息。
func qaRpcRegFrame(t *testing.T, reqID uint64, routes ...*message.GinRoute) *message.FSMessage {
	t.Helper()
	body, err := proto.Marshal(&message.GinRouteRegistration{Routes: routes})
	if err != nil {
		t.Fatal(err)
	}
	return newGinProxyFrame(ginSubRegister, reqID, body)
}

// qaRpcAckOf 取记录帧里的注册回执（要求恰 1 帧）。
func qaRpcAckOf(t *testing.T, frames []*message.FSMessage) *message.GinRegisterAck {
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

// TestQaRpcRegisterAckExcludesRejected P2-1：跨属主被拒的条目不得计入 ack 成功数。
// 修复前 gin.RegisterGRPCRoute 无返回值、服务端无条件 n++ 且 ok=true：
// 客户端 B 会收到 "registered 2 routes"（含被 A 占用的 /x），误以为自己在服务
// 该路由；修复后成功数只含落库条目，被拒 path 进 ack 明细（红→绿）。
func TestQaRpcRegisterAckExcludesRejected(t *testing.T) {
	srv := gpLoopServer("qa-rpc-rej", 8)

	// owner A 先占住 /qa-rpc-rej/x
	if !gin.RegisterGRPCRoute("qa-rpc-rej-a", 1, "/qa-rpc-rej/x", http.MethodGet, qaRpcNopStream{}) {
		t.Fatal("✘ 预置：A 注册 /x 应成功")
	}
	defer gin.UnregisterGRPCRoutes("qa-rpc-rej-a", 1)

	stB := &gpRecordStream{}
	csB := &clientSession{id: 2, stream: stB}
	srv.nametoclientstream.Store("qa-rpc-rej-b", csB)
	defer gin.UnregisterGRPCRoutes("qa-rpc-rej-b", csB.id)

	// B 的注册帧：/x（A 占用 → 应被拒）+ /y（新路由 → 应成功）
	srv.handleGinProxyFrame(csB, "qa-rpc-rej-b", 1, ginSubRegister, qaRpcRegFrame(t, 1,
		&message.GinRoute{UrlPath: "/qa-rpc-rej/x", Method: http.MethodGet},
		&message.GinRoute{UrlPath: "/qa-rpc-rej/y", Method: http.MethodGet},
	))
	ack := qaRpcAckOf(t, stB.frames())
	if !ack.GetOk() {
		t.Fatalf("✘ 部分成功应回 ok=true，实际 msg=%q", ack.GetMsg())
	}
	if !strings.HasPrefix(ack.GetMsg(), "registered 1 routes") {
		t.Fatalf("✘ 被拒条目不得计入成功数，ack msg=%q", ack.GetMsg())
	}
	if !strings.Contains(ack.GetMsg(), "/qa-rpc-rej/x") {
		t.Fatalf("✘ ack 明细应带被拒 path，实际 msg=%q", ack.GetMsg())
	}

	// 全部被拒 → ok=false，客户端能明确感知注册未生效
	stB2 := &gpRecordStream{}
	csB2 := &clientSession{id: 3, stream: stB2}
	srv.nametoclientstream.Store("qa-rpc-rej-b", csB2)
	srv.handleGinProxyFrame(csB2, "qa-rpc-rej-b", 2, ginSubRegister, qaRpcRegFrame(t, 2,
		&message.GinRoute{UrlPath: "/qa-rpc-rej/x", Method: http.MethodGet},
	))
	ack2 := qaRpcAckOf(t, stB2.frames())
	if ack2.GetOk() {
		t.Fatalf("✘ 全部被拒应回 ok=false，实际 msg=%q", ack2.GetMsg())
	}
	if !strings.HasPrefix(ack2.GetMsg(), "registered 0 routes") {
		t.Fatalf("✘ 全部被拒成功数应为 0，ack msg=%q", ack2.GetMsg())
	}
}

// TestQaRpcSessionTakeoverClearsStaleRoutes P2-2：S1 注册 {a,b,c} → S2 同名顶替
// 并注册 {a,b} → S1 代际的 {c} 残条目必须在顶替时被清掉（否则永久悬挂，
// 按名解析失败回 502 而非 404），且清理不得误伤 S2 代际条目（A-F3 不回退）。
// 探针口径：intruder（异属主）能否注册同 path —— 残条目仍在则被拒（红）。
func TestQaRpcSessionTakeoverClearsStaleRoutes(t *testing.T) {
	srv := gpLoopServer("qa-rpc-take", 8)
	intruder := qaRpcNopStream{}

	st1 := &gpRecordStream{}
	cs1 := srv.openSession("qa-rpc-take", st1)
	defer gin.UnregisterGRPCRoutes("qa-rpc-take", cs1.id)

	// S1 注册 {a,b,c}
	srv.handleGinProxyFrame(cs1, "qa-rpc-take", 1, ginSubRegister, qaRpcRegFrame(t, 1,
		&message.GinRoute{UrlPath: "/qa-rpc-take/a", Method: http.MethodGet},
		&message.GinRoute{UrlPath: "/qa-rpc-take/b", Method: http.MethodGet},
		&message.GinRoute{UrlPath: "/qa-rpc-take/c", Method: http.MethodGet},
	))

	// S2 同名顶替（openSession 覆盖注册表）
	st2 := &gpRecordStream{}
	cs2 := srv.openSession("qa-rpc-take", st2)
	defer gin.UnregisterGRPCRoutes("qa-rpc-take", cs2.id)

	// 顶替即应清掉 S1 代际全部条目：c（以及未被 S2 重注册的 a,b 旧代条目）
	if !gin.RegisterGRPCRoute("qa-rpc-intruder", 99, "/qa-rpc-take/c", http.MethodGet, intruder) {
		t.Fatal("✘ 顶替后旧会话 /c 残条目未回收：intruder 注册被拒（悬挂条目将按名解析失败回 502）")
	}
	gin.UnregisterGRPCRoutes("qa-rpc-intruder", 99)

	// S2 注册 {a,b}（新代际）
	srv.handleGinProxyFrame(cs2, "qa-rpc-take", 2, ginSubRegister, qaRpcRegFrame(t, 2,
		&message.GinRoute{UrlPath: "/qa-rpc-take/a", Method: http.MethodGet},
		&message.GinRoute{UrlPath: "/qa-rpc-take/b", Method: http.MethodGet},
	))
	// a,b 属 S2 代际：intruder 必须被拒（顶替清理没有把新会话的注册面打穿）
	if gin.RegisterGRPCRoute("qa-rpc-intruder", 99, "/qa-rpc-take/a", http.MethodGet, intruder) {
		t.Fatal("✘ /a 应属 S2 代际，intruder 不应注册成功")
	}

	// S1 迟到的 closeSession（own=false 分支）不得抹掉 S2 的条目
	srv.closeSession("qa-rpc-take", cs1)
	if gin.RegisterGRPCRoute("qa-rpc-intruder", 99, "/qa-rpc-take/a", http.MethodGet, intruder) {
		t.Fatal("✘ S1 延迟回收抹掉了 S2 的 /a 条目（A-F3 回退）")
	}

	// S2 正常关闭（own=true）后 a,b 才真正摘除 → c 与 a,b 均不可再命中旧属主
	srv.closeSession("qa-rpc-take", cs2)
	if !gin.RegisterGRPCRoute("qa-rpc-intruder", 99, "/qa-rpc-take/a", http.MethodGet, intruder) {
		t.Fatal("✘ S2 回收后 /a 应可被重新注册")
	}
	gin.UnregisterGRPCRoutes("qa-rpc-intruder", 99)
}

// TestQaRpcOrphanProxyCallbackTrackedByBgWG P3-1：dispatchProxyHandler 超时回 504
// 后，仍在跑的回调 goroutine 必须计入 bgWG（Close 的排空/不交还对象池防线
// 依赖它）。修复前 bgWG 对孤儿回调零感知：Wait 立即返回（红）。
func TestQaRpcOrphanProxyCallbackTrackedByBgWG(t *testing.T) {
	c := &RpcClient{fullAddr: "127.0.0.1:0", serverName: "qa-rpc-orphan"}
	release := make(chan struct{})
	started := make(chan struct{})
	RegisterGinProxyHandler("/qa-rpc-orphan|GET", func(*GinProxyRequest) *GinProxyResponse {
		close(started)
		<-release
		return &GinProxyResponse{Status: http.StatusOK}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rsp := c.dispatchProxyHandler(ctx, &message.GinHTTPRequest{Method: http.MethodGet, MatchedPattern: "/qa-rpc-orphan"})
	if rsp.GetStatus() != http.StatusGatewayTimeout {
		t.Fatalf("✘ 超时应回 504，实际 %d", rsp.GetStatus())
	}
	<-started // 回调确实在跑（504 已回，孤儿形成）

	waited := make(chan struct{})
	go func() {
		c.bgWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("✘ 孤儿回调未纳入 bgWG：504 返回后 Wait 立即通过，Close 排空看不见仍在跑的回调")
	case <-time.After(150 * time.Millisecond):
	}

	close(release) // 回调结束后 bgWG 必须随之释放
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ 回调结束后 bgWG 仍未释放")
	}
}
