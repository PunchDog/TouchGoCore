package websocket

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
)

// ============================================================================
// 阶段 9 复核整改回归用例（三份独立评审中被确认的缺陷）：
//   R1 Worker 队列满不再回投 msgQueue，也就不再重复统计消息
//   R2 业务回调 panic 必须被收敛，串行/并行两条路径都不能吃掉消费者
//   R3 内网豁免不得跳过 Origin 白名单
//   R4 心跳/超时参数改为原子量，Run 写入不与连接协程读并发冲突
// ============================================================================

// panicCall 会在业务回调里崩溃的 ICall 实现
type panicCall struct {
	OnConnectPanics bool
	OnMessagePanics bool
}

func (p *panicCall) OnConnect(client *Client) bool {
	if p.OnConnectPanics {
		panic("OnConnect 崩了")
	}
	return true
}

func (p *panicCall) OnMessage(client *Client, message proto.Message) {
	if p.OnMessagePanics {
		panic("OnMessage 崩了")
	}
}

func (p *panicCall) OnClose(client *Client) {}

// testMsgBytes 打包一条已注册协议的消息体，供解析路径使用
func testMsgBytes(t *testing.T) []byte {
	t.Helper()
	util.RegisterProtocolType(901, 902, wrapperspb.String(""))
	fs := util.NewFSMessageWithID(901, 902, 1, wrapperspb.String("hi"))
	if fs == nil {
		t.Fatal("打包失败")
	}
	data, err := proto.Marshal(fs)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return data
}

// useTestClient 在 clientMap 里挂一个可用客户端，测试结束摘除
func useTestClient(t *testing.T, uid int64, icall ICall) *Client {
	t.Helper()
	table := loadClientMap()
	if table == nil {
		table = syncmap.NewMap[int64, *Client]()
		prev := loadClientMap()
		storeClientMap(table)
		t.Cleanup(func() { storeClientMap(prev) })
	}
	c := &Client{UID: uid, remoteAddr: "bare://review"}
	c.ICall = icall
	c.initChannels()
	table.Store(uid, c)
	t.Cleanup(func() { table.Delete(uid) })
	return c
}

// ----------------------------------------------------------------------------
// R2 业务回调 panic 隔离
// ----------------------------------------------------------------------------

// TestProcessMessageContainsCallbackPanic 回归（整改 R2）：业务 OnMessage panic
// 必须被 processMessage 收敛并如实报告。串行模式下它跑在 Tick 唯一的消费协程上，
// 逃逸一次就让全服消息处理永久停摆。
func TestProcessMessageContainsCallbackPanic(t *testing.T) {
	useTestClient(t, 901, &panicCall{OnMessagePanics: true})
	before := serverStats.totalErrors.Load()

	done := make(chan bool, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("✘ panic 逃出了 processMessage: %v", r)
			}
			done <- false
		}()
		done <- processMessage(&msgQueueType{uid: 901, data: testMsgBytes(t)})
	}()

	select {
	case panicked := <-done:
		if !panicked {
			t.Fatal("✘ 业务回调 panic 未上报，Worker 错误计数会长期为 0")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("✘ processMessage 未返回")
	}
	if after := serverStats.totalErrors.Load(); after <= before {
		t.Fatalf("✘ 未计入服务器错误统计: before=%d after=%d", before, after)
	}
}

// TestSafeProcessCountsPanicAndContinues 回归（整改 R2）：一条消息崩溃后，
// 同一 Worker 仍要处理后续消息，且崩溃只计一次错误。
func TestSafeProcessCountsPanicAndContinues(t *testing.T) {
	useTestClient(t, 902, &panicCall{OnMessagePanics: true})
	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType, 4)},
		stats:      []*workerStats{{WorkerID: 0}},
		stop:       make(chan struct{}),
		size:       1,
		shardByKey: true,
	}
	withWorkerPool(t, pool)

	bad := testMsgBytes(t)
	pool.safeProcess(0, &msgQueueType{uid: 902, data: bad})

	// 换成不崩溃的回调后，同一 Worker 必须照常工作
	client, _ := loadClientMap().Load(902)
	client.ICall = &panicCall{}
	pool.safeProcess(0, &msgQueueType{uid: 902, data: bad})

	if m := pool.stats[0].Messages.Load(); m != 2 {
		t.Fatalf("✘ 崩溃后 Worker 停止了计数: Messages=%d", m)
	}
	if e := pool.stats[0].Errors.Load(); e != 1 {
		t.Fatalf("✘ 崩溃次数计数错误: Errors=%d want=1", e)
	}
}

// ----------------------------------------------------------------------------
// R1 消息计数口径
// ----------------------------------------------------------------------------

// TestDispatchDoesNotCountMessage 回归（整改 R1）：dispatch 只做投递，不得统计消息。
// 修复前它每投一次就 +1，队列满重试时同一条消息被计数多轮，
// 与 UpdateStatsFromMessage 的真实计数重复，总消息数失去意义。
func TestDispatchDoesNotCountMessage(t *testing.T) {
	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType, 4)},
		stats:      []*workerStats{{WorkerID: 0}},
		stop:       make(chan struct{}),
		size:       1,
		shardByKey: true,
	}
	withWorkerPool(t, pool)

	before := serverStats.totalMessages.Load()
	pool.dispatch(&msgQueueType{uid: 1, data: []byte("x")})
	pool.dispatch(&msgQueueType{uid: 1, data: []byte("x")})

	if got := serverStats.totalMessages.Load(); got != before {
		t.Fatalf("✘ 投递阶段统计了消息: before=%d got=%d", before, got)
	}
	if len(pool.queues[0]) != 2 {
		t.Fatalf("✘ 消息未进入 Worker 队列: len=%d", len(pool.queues[0]))
	}
}

// TestProcessMessageCountsOnce 回归（整改 R1）：真实计数点在 processMessage，
// 一条消息恰好 +1。
func TestProcessMessageCountsOnce(t *testing.T) {
	useTestClient(t, 903, &panicCall{})
	before := serverStats.totalMessages.Load()
	if processMessage(&msgQueueType{uid: 903, data: testMsgBytes(t)}) {
		t.Fatal("✘ 正常消息被判定为崩溃")
	}
	if got := serverStats.totalMessages.Load(); got != before+1 {
		t.Fatalf("✘ 单条消息计数错误: before=%d got=%d", before, got)
	}
}

// ----------------------------------------------------------------------------
// R3 Origin 内网豁免
// ----------------------------------------------------------------------------

// TestIntranetSkipDoesNotBypassWhitelist 回归（整改 R3）：skip_origin_for_intranet
// 只放行「不带 Origin 的内网原生客户端」。修复前它对内网整条跳过白名单，
// 内网被控主机或 DNS rebinding 带着任意站点的 Origin 即可跨站握手。
func TestIntranetSkipDoesNotBypassWhitelist(t *testing.T) {
	p := originPolicy{
		allowedOrigins:        []string{"https://ok.local"},
		checkOrigin:           true,
		skipOriginForIntranet: true,
	}

	r := httptest.NewRequest("GET", "/ws", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Origin", "https://evil.local")
	if p.check(r) {
		t.Fatal("✘ 内网请求带着非法 Origin 被放行，白名单形同虚设")
	}

	r2 := httptest.NewRequest("GET", "/ws", nil)
	r2.RemoteAddr = "127.0.0.1:5555"
	if !p.check(r2) {
		t.Fatal("✘ 内网无 Origin 的原生客户端应被放行")
	}

	r3 := httptest.NewRequest("GET", "/ws", nil)
	r3.RemoteAddr = "127.0.0.1:5555"
	r3.Header.Set("Origin", "https://ok.local")
	if !p.check(r3) {
		t.Fatal("✘ 内网白名单内 Origin 应被放行")
	}

	// 公网侧行为不变：无 Origin 仍然拒绝
	r4 := httptest.NewRequest("GET", "/ws", nil)
	r4.RemoteAddr = "8.8.8.8:5555"
	if p.check(r4) {
		t.Fatal("✘ 公网无 Origin 请求不应放行")
	}
}

// ----------------------------------------------------------------------------
// R4 超时参数原子量
// ----------------------------------------------------------------------------

// TestTimeoutAccessorsSeeLatestRun 回归（整改 R4）：连接协程热循环读取心跳/超时参数，
// Run 写入必须原子可见。修复前是普通 time.Duration 变量，重启服务与旧连接读并发即数据竞争。
func TestTimeoutAccessorsSeeLatestRun(t *testing.T) {
	defer func() {
		applyTimeoutConfig(nil) // 还原默认，避免污染其它用例
	}()

	applyTimeoutConfig(&config.WebsocketConfig{
		PingIntervalMS: 1000,
		ReadTimeoutMS:  9000,
		WriteTimeoutMS: 300,
	})
	if got := currentPingInterval(); got != time.Second {
		t.Fatalf("✘ ping 间隔未生效: %v", got)
	}
	if got := currentReadTimeout(); got != 9*time.Second {
		t.Fatalf("✘ 读超时未生效: %v", got)
	}
	if got := currentWriteTimeout(); got != 300*time.Millisecond {
		t.Fatalf("✘ 写超时未生效: %v", got)
	}

	// 未配置项回落默认值，且 NewTicker(0) 一类的致命用法被排除
	applyTimeoutConfig(nil)
	if currentPingInterval() <= 0 || currentReadTimeout() <= 0 || currentWriteTimeout() <= 0 {
		t.Fatalf("✘ 默认超时被清零: ping=%v read=%v write=%v",
			currentPingInterval(), currentReadTimeout(), currentWriteTimeout())
	}
}

// TestRunConnectGateContainsPanic 回归（整改 R2）：业务 OnConnect panic 必须按「拒绝连接」收敛。
// 修复前 panic 直接冒到 HTTP 处理协程，连接被静默掐断，日志里查不到原因。
func TestRunConnectGateContainsPanic(t *testing.T) {
	panicking := &Client{UID: 1, remoteAddr: "bare://panic"}
	panicking.ICall = &panicCall{OnConnectPanics: true}
	if panicking.runConnectGate() {
		t.Fatal("✘ panic 的 OnConnect 被判为放行")
	}

	rejected := &Client{UID: 2, remoteAddr: "bare://reject"}
	rejected.ICall = &rejectCall{}
	if rejected.runConnectGate() {
		t.Fatal("✘ 返回 false 的 OnConnect 被放行")
	}

	ok := &Client{UID: 3, remoteAddr: "bare://ok"}
	ok.ICall = &panicCall{}
	if !ok.runConnectGate() {
		t.Fatal("✘ 正常 OnConnect 被拒绝")
	}
}

// TestWorkerLoopSurvivesCallbackPanic 回归（整改 R2）：一条消息的回调 panic 不得带走
// Worker 常驻协程——修复前 recover 只在派发侧，Worker 一次崩溃就永久少一个消费者。
func TestWorkerLoopSurvivesCallbackPanic(t *testing.T) {
	useTestClient(t, 904, &panicCall{OnMessagePanics: true})
	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType, 4)},
		stats:      []*workerStats{{WorkerID: 0}},
		stop:       make(chan struct{}),
		size:       1,
		shardByKey: true,
	}
	pool.stats[0].Running.Store(true)
	pool.wg.Add(1)
	go pool.workerLoop(0)

	data := testMsgBytes(t)
	pool.queues[0] <- &msgQueueType{uid: 904, data: data}
	pool.queues[0] <- &msgQueueType{uid: 904, data: data}

	if !waitTrue(3*time.Second, func() bool { return pool.stats[0].Messages.Load() == 2 }) {
		t.Fatalf("✘ 第一条 panic 后 Worker 就停了：Messages=%d", pool.stats[0].Messages.Load())
	}
	if e := pool.stats[0].Errors.Load(); e != 2 {
		t.Fatalf("✘ 两次崩溃只记了 %d 个错误", e)
	}

	close(pool.stop)
	done := make(chan struct{})
	go func() {
		pool.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("✘ Worker 协程未随 stop 退出")
	}
	if pool.stats[0].Running.Load() {
		t.Fatal("✘ 退出后 Running 仍为 true，统计口径失真")
	}
}

// TestDispatchRotatesWithoutProcessedMessages 回归：轮询模式的派发序号由池自己推进。
// 借用「已处理消息数」取模时，只要消息持续解析失败/队列满，计数不涨，
// 所有消息会一直粘在同一个 Worker 上，其它 Worker 全程空闲。
func TestDispatchRotatesWithoutProcessedMessages(t *testing.T) {
	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType, 1), make(chan *msgQueueType, 1), make(chan *msgQueueType, 1)},
		stats:      []*workerStats{{WorkerID: 0}, {WorkerID: 1}, {WorkerID: 2}},
		stop:       make(chan struct{}),
		size:       3,
		shardByKey: false,
	}
	withWorkerPool(t, pool)

	// 全程没有一条消息被处理（无人消费队列），派发仍要摊开
	for i := 0; i < 3; i++ {
		pool.dispatch(&msgQueueType{uid: int64(1000 + i), data: []byte("x")})
	}
	for i, q := range pool.queues {
		if len(q) != 1 {
			t.Fatalf("✘ 第 %d 号 Worker 收到 %d 条，轮询未摊开（修复前全部粘在 0 号）", i, len(q))
		}
	}
}

// waitTrue 在 d 内轮询 cond。
func waitTrue(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// rejectCall 总是拒绝连接的 ICall 实现
type rejectCall struct{}

func (rejectCall) OnConnect(client *Client) bool               { return false }
func (rejectCall) OnMessage(client *Client, msg proto.Message) {}
func (rejectCall) OnClose(client *Client)                      {}

// ----------------------------------------------------------------------------
// 阶段11：Run 装配的队列参数与生命周期上下文必须是原子快照
// ----------------------------------------------------------------------------

// TestQueueParamsSnapshotNotMixed 回归：一轮 Run 换参期间，正在建连的旧连接
// 不得拿到「半新半旧」的参数。修复前四个包级普通变量逐个赋值，
// writeQueueCap 与 dropOnFull 可能来自不同两轮，背压开关也可能整体丢失。
func TestQueueParamsSnapshotNotMixed(t *testing.T) {
	useQueueParams(t, wsQueueParams{writeEntries: 8, readEntries: 16, dropOnFull: true})
	if got := writeQueueCap(); got != 8 {
		t.Fatalf("✘ 快照未生效: writeQueueCap=%d", got)
	}
	// 整体替换后必须一次看到全套新值
	next := wsQueueParams{writeEntries: 64, readEntries: 128, backpressure: true}
	wsQueue.Store(&next)
	if got := writeQueueCap(); got != 64 {
		t.Fatalf("✘ 换轮后仍读到旧容量: %d", got)
	}
	if p := queueParams(); p.dropOnFull || !p.backpressure || p.readEntries != 128 {
		t.Fatalf("✘ 新旧两轮参数混用: %+v", p)
	}
}

// TestWsRunCtxSwapVisibleToReaders 回归：Run 写入的生命周期上下文对
// 上一轮仍存活的连接协程必须原子可见（旧代码是普通接口变量，并发读写即竞争）。
func TestWsRunCtxSwapVisibleToReaders(t *testing.T) {
	prev := wsRunCtx()
	t.Cleanup(func() { setWsRunCtx(prev) })

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(func() { cancelA(); cancelB() })

	setWsRunCtx(ctxA)
	if wsRunCtx() != ctxA {
		t.Fatal("✘ 读取到的不是本轮上下文")
	}
	stop := make(chan struct{})
	var seen sync.WaitGroup
	for i := 0; i < 4; i++ {
		seen.Add(1)
		go func() {
			defer seen.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = wsRunCtx().Done()
					writeQueueCap()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		setWsRunCtx(ctxB)
		setWsRunCtx(ctxA)
	}
	close(stop)
	seen.Wait()
	cancelB()
	select {
	case <-wsRunCtx().Done():
		t.Fatal("✘ 当前快照应为未取消的 ctxA")
	default:
	}
}
