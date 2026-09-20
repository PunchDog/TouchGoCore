package rpc

import (
	"context"
	"errors"
	"io"
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
// 阶段9 复核整改回归（S41–S48）：响应串号、鉴权空指针、停机排空、
// 错误包序列化、并发令牌回收。
// 每个用例都用「修复前必然失败」的构造来证明缺陷已闭合。
// ============================================================================

// fakeRecvStream 只实现 Recv：按脚本依次吐出响应，脚本耗尽后返回错误结束 recvLoop。
type fakeRecvStream struct {
	message.Grpc_MsgClient
	queue []*message.FSMessage
	idx   int
}

func (s *fakeRecvStream) Recv() (*message.FSMessage, error) {
	if s.idx >= len(s.queue) {
		return nil, errors.New("脚本已耗尽")
	}
	msg := s.queue[s.idx]
	s.idx++
	return msg, nil
}

// recordStream 记录服务端发出的每一帧。
type recordStream struct {
	message.Grpc_MsgServer
	mu   sync.Mutex
	sent []*message.FSMessage
}

func (s *recordStream) Send(msg *message.FSMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *recordStream) frames() []*message.FSMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*message.FSMessage(nil), s.sent...)
}

// fakeMsgServer 服务端流的最小替身：带 client-name 元数据，按脚本吐出请求，并记录回包。
type fakeMsgServer struct {
	message.Grpc_MsgServer
	name string
	recv []*message.FSMessage
	sent []*message.FSMessage
	idx  int
}

func (s *fakeMsgServer) Context() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", s.name))
}

func (s *fakeMsgServer) Send(msg *message.FSMessage) error {
	s.sent = append(s.sent, msg)
	return nil
}

func (s *fakeMsgServer) Recv() (*message.FSMessage, error) {
	if s.idx >= len(s.recv) {
		return nil, io.EOF
	}
	msg := s.recv[s.idx]
	s.idx++
	return msg, nil
}

func rspFor(p1, p2 int32, requestID uint64, body string) *message.FSMessage {
	raw := util.NewFSMessageWithID(p1, p2, requestID, wrapperspb.String(body))
	if raw == nil {
		panic("构造响应失败")
	}
	return raw
}

// TestRecvLoopDoesNotDeliverUnknownRequestID request_id 有值但无人等待时，
// 不得走「投给任意等待者」的兜底：那会把 A 的响应交给还在等的 B，
// B 拿到一条内容完全无关、看起来却正常的消息。
func TestRecvLoopDoesNotDeliverUnknownRequestID(t *testing.T) {
	rpcNoneAuthEnv(t)
	util.RegisterProtocolType(131, 131, wrapperspb.String(""))

	client := &RpcClient{serverName: "s", fullAddr: "127.0.0.1:0", timeout: time.Second}
	client.SetCallbacks(NewClientCallbacks())

	waiter := make(chan *message.FSMessage, 2)
	const myID, orphanID uint64 = 7, 99
	client.pending.Store(myID, waiter)
	defer client.pending.Delete(myID)

	stream := &fakeRecvStream{queue: []*message.FSMessage{
		rspFor(131, 131, orphanID, "belongs-to-nobody"),
		rspFor(131, 131, myID, "mine"),
	}}
	client.recvLoop(stream)

	if len(waiter) != 1 {
		t.Fatalf("✘ 等待者收到 %d 帧（修复前：孤儿响应也会被塞进来，请求直接串号）", len(waiter))
	}
	if got := (<-waiter).GetHead().GetRequestId(); got != myID {
		t.Fatalf("✘ 等待者拿到 request_id=%d，应为 %d", got, myID)
	}
}

// TestRecvLoopStillDeliversLegacyZeroID 旧服务端不回填 request_id，
// 这类响应仍需投递给任意等待者——收紧串号修复不能把兼容路径一起关掉。
func TestRecvLoopStillDeliversLegacyZeroID(t *testing.T) {
	rpcNoneAuthEnv(t)
	util.RegisterProtocolType(132, 132, wrapperspb.String(""))

	client := &RpcClient{serverName: "s", fullAddr: "127.0.0.1:0", timeout: time.Second}
	client.SetCallbacks(NewClientCallbacks())

	waiter := make(chan *message.FSMessage, 1)
	client.pending.Store(uint64(5), waiter)
	defer client.pending.Delete(uint64(5))

	client.recvLoop(&fakeRecvStream{queue: []*message.FSMessage{rspFor(132, 132, 0, "legacy")}})

	select {
	case got := <-waiter:
		if got.GetHead().GetProtocol2() != 132 {
			t.Fatalf("✘ 旧协议响应内容错误: %v", got)
		}
	default:
		t.Fatal("✘ request_id=0 的旧服务端响应被丢弃")
	}
}

// TestAllowlistModeWithoutListRejects allowlist 模式下漏配名单必须拒绝而不是崩溃。
// 修复前 authenticate 连着读两次 rpcAuthCfg()：两次之间配置换掉会拿到
// 「mode=allowlist 但 cfg=nil」的组合，遍历 cfg.AllowList 直接空指针，
// 而流处理协程没有 recover，一条连接就能带走服务端。
func TestAllowlistModeWithoutListRejects(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))

	// mode 显式为 allowlist、名单为空
	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{
		Auth: &config.RpcAuthConfig{Mode: "allowlist"},
	}})
	if err := authenticate(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("✘ 空名单被放行: %v", err)
	}

	// mode 归一化：带空格的 mode 要认得出来，nil 配置按默认 token 处理
	if got := authModeOf(&config.RpcAuthConfig{Mode: " allowlist "}); got != "allowlist" {
		t.Fatalf("authModeOf 归一化失败: %q", got)
	}
	if got := authModeOf(nil); got != defaultAuthMode {
		t.Fatalf("✘ nil 配置默认模式 %q != %q", got, defaultAuthMode)
	}
	// Bearer scheme 按 RFC 7235 大小写不敏感，否则合法客户端会被自己的写法拒掉
	if got := bearerToken(metadata.Pairs("authorization", "bearer abc")); got != "abc" {
		t.Fatalf("✘ 小写 bearer 未被解析: %q", got)
	}
}

// TestAuthenticateUsesAllowlistSnapshot 名单命中/未命中的基本判定。
func TestAuthenticateUsesAllowlistSnapshot(t *testing.T) {
	rpcTestEnvCfg(t, &config.Cfg{Rpc: &config.RpcConfig{
		Auth: &config.RpcAuthConfig{Mode: "allowlist", AllowList: []string{"gate"}},
	}})
	ok := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "gate"))
	if err := authenticate(ok); err != nil {
		t.Fatalf("✘ 名单内服务名被拒绝: %v", err)
	}
	bad := metadata.NewIncomingContext(context.Background(), metadata.Pairs("client-name", "evil"))
	if status.Code(authenticate(bad)) != codes.Unauthenticated {
		t.Fatal("✘ 名单外服务名被放行")
	}
}

// TestSendErrorPacketMarshalable 服务端回的错误包必须可序列化。
// FSMessage.Body 在 proto2 里是 required，不设 Body 时 proto.Marshal 报
// “required field network.message.FSMessage.body not set”，错误包发不出去，
// 客户端只能干等到自己的调用超时。
func TestSendErrorPacketMarshalable(t *testing.T) {
	rpcNoneAuthEnv(t)
	st := &recordStream{}
	srv := newBareServer("err-packet")
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})

	srv.sendErrorPacket(&MessageInfo{ClientNameKey: "peer", Protol1: 133, Protol2: 133, RequestID: 8})
	if len(st.sent) != 1 {
		t.Fatalf("✘ 未发出错误包，sent=%d", len(st.sent))
	}
	if _, err := proto.Marshal(st.sent[0]); err != nil {
		t.Fatalf("✘ 错误包无法序列化，客户端收不到: %v", err)
	}
	if got := st.sent[0].GetHead().GetCmd(); got != ErrorCmd {
		t.Fatalf("错误包 Cmd=%q, 应为 %q", got, ErrorCmd)
	}
	// request_id=0 的旧客户端按「下一条响应即本次响应」匹配，回错误包反而串号
	srv.sendErrorPacket(&MessageInfo{ClientNameKey: "peer", Protol1: 133, Protol2: 133, RequestID: 0})
	if len(st.sent) != 1 {
		t.Fatalf("✘ 旧客户端（request_id=0）也被回了错误包，sent=%d", len(st.sent))
	}
}

// newLoopServer 构造一个带完整停机字段的服务端，够跑两段式排空流程。
func newLoopServer(name string, queueSize int) *RpcServer {
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

// TestDrainReadHandsOffBacklog 接收段收到「停止接收」后，readchannel 的存量
// 必须整批交给下游：这些请求已经从客户端流里读走了，客户端在等回包。
// 修复前根本没有 drainRead，信号一到就直接 return，存量全部失联。
func TestDrainReadHandsOffBacklog(t *testing.T) {
	rpcNoneAuthEnv(t)
	useHandlerTimeout(t, 10*time.Second)
	registerHandler(t, 134, 134, func(_ context.Context, _ *MessageInfo) proto.Message {
		return wrapperspb.String("drained")
	})

	srv := newLoopServer("drain-read", 4)
	st := &recordStream{}
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})

	const backlog = 3
	for i := 0; i < backlog; i++ {
		srv.readchannel <- &MessageInfo{
			Req:           rspFor(134, 134, uint64(i+1), "queued"),
			ClientNameKey: "peer",
			Protol1:       134,
			Protol2:       134,
			RequestID:     uint64(i + 1),
		}
	}
	// 只关接收信号，处理段仍在：这是 Stop 的真实顺序
	close(srv.readClose)
	srv.readChannel() // 同步调用，走完 drainRead 才返回

	if len(srv.readchannel) != 0 {
		t.Fatalf("✘ readChannel 退出时 readchannel 仍残留 %d 条", len(srv.readchannel))
	}
	if got := len(srv.handlechannel); got != backlog {
		t.Fatalf("✘ 存量只交出了 %d/%d 条给下游", got, backlog)
	}
}

// TestDrainBacklogRunsQueuedRequests 处理段退出前要把 handlechannel 的存量串行跑完，
// 而不是直接收工。修复前 handleChannel 只看 done，队列里的请求连同回包一起被吞。
func TestDrainBacklogRunsQueuedRequests(t *testing.T) {
	rpcNoneAuthEnv(t)
	useHandlerTimeout(t, 10*time.Second)

	var ran atomic.Int32
	registerHandler(t, 141, 141, func(_ context.Context, _ *MessageInfo) proto.Message {
		ran.Add(1)
		return wrapperspb.String("drained")
	})

	srv := newLoopServer("drain-backlog", 4)
	srv.handlerSem = make(chan struct{}, 2)
	st := &recordStream{}
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})
	srv.drainDeadline.Store(time.Now().Add(5 * time.Second).UnixNano())

	const backlog = 3
	for i := 0; i < backlog; i++ {
		srv.handlechannel <- &MessageInfo{
			Req:           rspFor(141, 141, uint64(i+1), "queued"),
			ClientNameKey: "peer",
			Protol1:       141,
			Protol2:       141,
			RequestID:     uint64(i + 1),
		}
	}
	close(srv.done)
	srv.handleChannel() // 同步调用：走完 drainBacklog 才返回

	// done 与队列同时就绪时 select 会走并发派发分支，少数请求由独立协程收尾，等它们回完
	if !within(3*time.Second, func() bool { return len(st.frames()) == backlog }) {
		t.Fatalf("✘ 存量请求只回了 %d/%d 条（其余等到客户端超时）", len(st.frames()), backlog)
	}
	if got := ran.Load(); got != backlog {
		t.Fatalf("✘ 存量请求只处理了 %d/%d 条", got, backlog)
	}
	if len(srv.handlechannel) != 0 {
		t.Fatalf("✘ 退出时 handlechannel 仍残留 %d 条", len(srv.handlechannel))
	}
	for _, frame := range st.frames() {
		if frame.GetHead().GetCmd() == ErrorCmd {
			t.Fatalf("✘ 预算内本该排空的请求被回错误包: %v", frame.GetHead())
		}
	}
}

// TestStopKeepsInFlightResponseWithCancelledCallerCtx App 的关闭顺序是
// 「先 cancel app.ctx，再逐个 Stop」，传进来的 ctx 必然已经 Done。
// 若把它当成调用方等不及而立刻强杀传输层，正在处理的请求回包全丢——
// 客户端看到的是连接被掐断，而不是它等待的响应。
func TestStopKeepsInFlightResponseWithCancelledCallerCtx(t *testing.T) {
	rpcNoneAuthEnv(t)
	SetShutdownBudget(10*time.Second, 3*time.Second)
	t.Cleanup(func() { SetShutdownBudget(0, 0) })
	useHandlerTimeout(t, 10*time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	registerHandler(t, 142, 142, func(_ context.Context, _ *MessageInfo) proto.Message {
		close(started)
		<-release
		return wrapperspb.String("in-flight")
	})

	port := freePort(t)
	srv := startServer(t, "inflight-srv", port)
	client := dialClient(t, "inflight-client", port)
	client.timeout = 15 * time.Second

	got := make(chan string, 1)
	// SendMsg 会等回包，必须放协程里发，否则下面的 <-started 与它互等
	go client.SendMsg(142, 142, wrapperspb.String("ping"), func(pb proto.Message) {
		if sv, ok := pb.(*wrapperspb.StringValue); ok {
			got <- sv.GetValue()
		}
	})
	<-started

	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // 复刻 app.Shutdown 交给 Stop 的那个 ctx
	stopDone := make(chan struct{})
	go func() {
		srv.Stop(cancelled)
		close(stopDone)
	}()

	// 给强杀一点时间落地：修复前 service.Stop() 在这一刻就掐断了传输层，
	// 之后 handler 攒出的响应再也没人收得到。
	time.Sleep(300 * time.Millisecond)
	close(release)
	select {
	case v := <-got:
		if v != "in-flight" {
			t.Fatalf("✘ 在途请求拿到的是 %q，应为真实响应", v)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("✘ 在途请求的回包被停机的强杀吞掉了")
	}
	_ = client.Close() // 关掉流，让 GracefulStop 早点收工
	select {
	case <-stopDone:
	case <-time.After(8 * time.Second):
		t.Fatal("✘ Stop 未收工")
	}
}

// TestMsgStopsWhenReadClosed 接收段关闭后 Msg 要退出，
// 继续 Recv 只会把更多请求塞进已经无人消费的队列。
func TestMsgStopsWhenReadClosed(t *testing.T) {
	rpcNoneAuthEnv(t)
	util.RegisterProtocolType(143, 143, wrapperspb.String(""))

	srv := newLoopServer("msg-readclose", 1)
	srv.SetCallbacks(NewServerCallbacks())
	// 先把队列占死：否则「入队」和「停止接收」两个 case 同时就绪，select 随机选
	srv.readchannel <- &MessageInfo{ClientNameKey: "peer"}
	close(srv.readClose)

	stream := &fakeMsgServer{name: "peer", recv: []*message.FSMessage{rspFor(143, 143, 1, "x")}}
	done := make(chan struct{})
	go func() {
		_ = srv.Msg(stream)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ readClose 后 Msg 仍卡在接收循环里")
	}
	if len(stream.sent) != 1 || stream.sent[0].GetHead().GetCmd() != ErrorCmd {
		t.Fatalf("✘ 停止接收时未退回错误包，客户端会空等超时: sent=%d", len(stream.sent))
	}
}

// TestRunSerialWithoutDrainDeadlineRejects 没有排空预算时不得无上限地跑 handler：
// Serve 启动失败等异常路径只关了 done，此时应明确回错误包。
func TestRunSerialWithoutDrainDeadlineRejects(t *testing.T) {
	rpcNoneAuthEnv(t)
	srv := newLoopServer("no-deadline", 1)
	st := &recordStream{}
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})

	srv.runSerial(&MessageInfo{ClientNameKey: "peer", Protol1: 135, Protol2: 135, RequestID: 1})

	if len(st.sent) != 1 || st.sent[0].GetHead().GetCmd() != ErrorCmd {
		t.Fatalf("✘ 无排空预算时未回错误包，sent=%d", len(st.sent))
	}
	if len(srv.handlerSem) != 0 {
		t.Fatalf("✘ 拒绝路径不应占用并发令牌，令牌=%d", len(srv.handlerSem))
	}
}

// TestRunSerialRespectsDrainBudget 排空超时的请求要回错误包，而不是把停机拖成长阻塞。
func TestRunSerialRespectsDrainBudget(t *testing.T) {
	rpcNoneAuthEnv(t)
	registerHandler(t, 136, 136, func(_ context.Context, _ *MessageInfo) proto.Message {
		time.Sleep(2 * time.Second)
		return wrapperspb.String("slow")
	})

	srv := newLoopServer("drain-budget", 1)
	// 令牌占满：排空只能等预算，等不到就拒绝
	srv.handlerSem = make(chan struct{}, 1)
	srv.handlerSem <- struct{}{}
	st := &recordStream{}
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})
	srv.drainDeadline.Store(time.Now().Add(150 * time.Millisecond).UnixNano())

	started := time.Now()
	srv.runSerial(&MessageInfo{ClientNameKey: "peer", Protol1: 136, Protol2: 136, RequestID: 2})
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("✘ 排空未受预算约束，阻塞了 %v", elapsed)
	}
	if len(st.sent) != 1 || st.sent[0].GetHead().GetCmd() != ErrorCmd {
		t.Fatalf("✘ 排空超时未回错误包，sent=%d", len(st.sent))
	}
}

// panicOnDoneCtx 用来在「取到令牌 → handler 协程跑起来」之间制造 panic。
type panicOnDoneCtx struct {
	context.Context
}

func (panicOnDoneCtx) Done() <-chan struct{} { panic("上下文本身炸了") }

// setWorkParentForTest 在 workMu 保护下替换 handler 父上下文：
// 直接写包级变量会与残留 handler 协程里的 workParent 构成数据竞争。
func setWorkParentForTest(ctx context.Context) {
	workMu.Lock()
	defer workMu.Unlock()
	rpcWorkCtx = ctx
}

// TestHandleOneReleasesTokenOnEarlyPanic 「拿到令牌 → handler 起来」之间崩溃必须把令牌还回去，
// 否则令牌只减不增，服务在若干次异常后彻底不再处理请求。
func TestHandleOneReleasesTokenOnEarlyPanic(t *testing.T) {
	rpcNoneAuthEnv(t)
	prevParent := workParent()
	setWorkParentForTest(panicOnDoneCtx{Context: context.Background()})
	t.Cleanup(func() { setWorkParentForTest(prevParent) })

	srv := newLoopServer("token-leak", 1)
	st := &recordStream{}
	srv.nametoclientstream.Store("peer", &clientSession{stream: st})

	const rounds = 3
	for i := 0; i < rounds; i++ {
		srv.handlerSem <- struct{}{}
		before := len(srv.handlerSem)
		srv.handleOne(&MessageInfo{ClientNameKey: "peer", Protol1: 137, Protol2: 137, RequestID: uint64(i)})
		if len(srv.handlerSem) != before-1 {
			t.Fatalf("✘ 第 %d 轮 panic 后令牌未归还（剩余 %d，本应归还 1 个）", i, len(srv.handlerSem))
		}
	}
	if srv.inFlight.Load() != 0 {
		t.Fatalf("✘ panic 后 in-flight 未归零: %d", srv.inFlight.Load())
	}
}

// TestWorkCtxSurvivesRunCtxCancel handler 的工作上下文不能跟着 app.ctx 一起死：
// App 先 cancel 再逐个 Stop，若 handler 挂在 app.ctx 上，
// GracefulStop 与排空流程从第一毫秒起就是死代码（S45 只交付了一半的根因）。
func TestWorkCtxSurvivesRunCtxCancel(t *testing.T) {
	cfg := &config.Cfg{Rpc: &config.RpcConfig{Auth: &config.RpcAuthConfig{Mode: "none"}}}
	outer, cancel := context.WithCancel(context.Background())
	setRunCtx(corectx.WithCfg(outer, cfg))
	t.Cleanup(func() { setRunCtx(context.Background()) })

	resetWorkCtx()
	t.Cleanup(cancelWorkCtx)

	after := workParent()
	if after == nil {
		t.Fatal("✘ 工作上下文未初始化")
	}
	cancel()
	if workParent().Err() != nil {
		t.Fatalf("✘ handler 工作上下文随 app.ctx 一起结束，停机排空不可能执行: %v", after.Err())
	}

	// resetWorkCtx 必须作废上一轮，否则重启后老 handler 挂在已废弃的 ctx 上
	resetWorkCtx()
	if third := workParent(); third == after {
		t.Fatal("✘ resetWorkCtx 未新建上下文")
	}
	if after.Err() == nil {
		t.Fatal("✘ 上一轮工作上下文未被取消，停机后仍会接受新 handler")
	}
}

// TestCloseTriggersDisconnectedOnceWithIdentity 主动关闭要恰好触发一次断开回调，
// 且回调里拿到的还是本客户端的身份。
// 修复前回调在 Remove 之后触发：实例已交还对象池，可能立刻被下一个客户端认领，
// 回调读到的 serverName/地址就成了别人的数据。
func TestCloseTriggersDisconnectedOnceWithIdentity(t *testing.T) {
	rpcNoneAuthEnv(t)
	client := NewRpcClient("disc-client", "127.0.0.1", freePort(t))
	if client == nil {
		t.Fatal("客户端未创建")
	}
	var names atomic.Int32
	var emptyNames atomic.Int32
	client.SetCallbacks(&ClientCallbacks{OnDisconnected: func(name string, _ error) {
		names.Add(1)
		if name == "" {
			emptyNames.Add(1)
		}
	}})

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if got := names.Load(); got != 1 {
		t.Fatalf("✘ 主动关闭触发了 %d 次断开回调", got)
	}
	if got := emptyNames.Load(); got != 0 {
		t.Fatalf("✘ 断开回调拿到空身份（实例已回池）：%d 次", got)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if got := names.Load(); got != 1 {
		t.Fatalf("✘ 重复 Close 又触发断开回调，共 %d 次", got)
	}
}
