package websocket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	"touchgocore/config"
	"touchgocore/corectx"
)

// ============================================================================
// qa_w 修复回归（槽位 W）：
//   W1 closeOnce 内裸调 OnClose 无 recover → 关服/单连接关闭路径 panic 崩进程
//   W2 readLoop 入队 + processMessage 双计 → 同一条消息统计两遍
//   W3 processMessage 缺少 pin/UID 复核 → 池别名窗口把旧消息喂给新连接
//   W4 acquireICall 裸类型断言 → 池返回异常类型直接 panic
//   W5 Run 端口全败分支不收 Worker Pool → 常驻协程泄漏
// ============================================================================

// qaWClosePanicCall 在 OnClose 里崩溃的 ICall 实现
type qaWClosePanicCall struct{ closed *atomic.Int32 }

func (q *qaWClosePanicCall) OnConnect(client *Client) bool             { return true }
func (q *qaWClosePanicCall) OnMessage(client *Client, m proto.Message) {}
func (q *qaWClosePanicCall) OnClose(client *Client) {
	if q.closed != nil {
		q.closed.Add(1)
	}
	panic("qa_w OnClose 崩了")
}

// qaWRecordCall 记录 OnMessage 是否被调用
type qaWRecordCall struct{ msgs atomic.Int32 }

func (q *qaWRecordCall) OnConnect(client *Client) bool { return true }
func (q *qaWRecordCall) OnMessage(client *Client, m proto.Message) {
	q.msgs.Add(1)
}
func (q *qaWRecordCall) OnClose(client *Client) {}

// ----------------------------------------------------------------------------
// W1 OnClose panic 收敛
// ----------------------------------------------------------------------------

// TestQaW_OnClosePanicContained 单连接 Close：用户 OnClose panic 不得外抛，
// 且关闭流程（closeCh 关闭）必须走完。修复前 panic 直接冒到调用方。
func TestQaW_OnClosePanicContained(t *testing.T) {
	wsTestEnv(t)
	c := newBareClient(t)
	c.ICall = &qaWClosePanicCall{}

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		c.Close("测试 OnClose panic")
	}()

	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("✘ OnClose panic 逃出 Close（关服时会崩进程/吃掉消费者）: %v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("✘ Close 未返回")
	}

	select {
	case <-c.closeCh:
	default:
		t.Fatal("✘ panic 吞掉了关闭流程，closeCh 未关闭")
	}
}

// TestQaW_ShutdownSurvivesOnClosePanic 关服路径：shutdownWebsocket 串行 Range
// 关闭全部客户端，一个 OnClose panic 不得中断后续客户端的关闭。
func TestQaW_ShutdownSurvivesOnClosePanic(t *testing.T) {
	wsTestEnv(t)

	bad := newBareClient(t)
	bad.ICall = &qaWClosePanicCall{}
	var goodClosed atomic.Int32
	good := newBareClient(t)
	good.ICall = &qaWClosePanicCall{closed: &goodClosed}

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		shutdownWebsocket()
	}()
	select {
	case r := <-done:
		if r != nil {
			t.Fatalf("✘ shutdownWebsocket 被 OnClose panic 击穿: %v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("✘ shutdownWebsocket 未返回")
	}
	if goodClosed.Load() == 0 {
		t.Fatal("✘ panic 客户端之后的连接未被关闭（Range 被中断）")
	}
}

// ----------------------------------------------------------------------------
// W2 消息统计只计一次
// ----------------------------------------------------------------------------

// TestQaW_MessageStatsCountedExactlyOnce 端到端：真实 readLoop 入队 +
// processMessage 处理，客户端与服务端统计对同一条消息都只 +1。
// 修复前 readLoop 入队时先计一遍，processMessage 的 UpdateStatsFromMessage
// 再计一遍 → MessagesReceived=2。
func TestQaW_MessageStatsCountedExactlyOnce(t *testing.T) {
	wsTestEnv(t)

	up := &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverClient := make(chan *Client, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			serverClient <- nil
			return
		}
		c, err := NewClient(conn, r.RemoteAddr, "wsTestCall")
		if err != nil {
			serverClient <- nil
			return
		}
		serverClient <- c
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/"
	cli, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	sc := <-serverClient
	if sc == nil {
		t.Fatal("✘ 服务端 NewClient 失败")
	}

	payload := testMsgBytes(t)
	if err := cli.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	state := loadRunState()
	var item *msgQueueType
	select {
	case item = <-state.msgQueue:
	case <-time.After(3 * time.Second):
		t.Fatal("✘ readLoop 未把消息投进本代 msgQueue")
	}

	// 入队阶段不得计数（权威口径是「消息真正被处理」）
	if got := sc.GetStats().MessagesReceived; got != 0 {
		t.Fatalf("✘ readLoop 入队即计数（与 processMessage 双计）: MessagesReceived=%d", got)
	}

	beforeSrv := serverStats.totalMessages.Load()
	if processMessage(item) {
		t.Fatal("✘ 正常消息被判为 panic")
	}
	if got := serverStats.totalMessages.Load() - beforeSrv; got != 1 {
		t.Fatalf("✘ 服务端消息计数增量错误: got=%d want=1", got)
	}
	if got := sc.GetStats().MessagesReceived; got != 1 {
		t.Fatalf("✘ 客户端消息计数错误: got=%d want=1", got)
	}
	if got := sc.GetStats().BytesReceived; got != int64(len(payload)) {
		t.Fatalf("✘ 客户端字节计数错误: got=%d want=%d", got, len(payload))
	}
	sc.Close("测试结束")
}

// ----------------------------------------------------------------------------
// W3 processMessage 别名复核
// ----------------------------------------------------------------------------

// TestQaW_ProcessMessageRejectsAliasedClient 队列里的旧 uid 消息，若表内实例
// 已被复用为另一条连接（UID 不匹配），必须丢弃而不是把旧数据喂给新回调。
func TestQaW_ProcessMessageRejectsAliasedClient(t *testing.T) {
	wsTestEnv(t)
	rec := &qaWRecordCall{}
	table := loadClientMap()
	c := &Client{UID: 700701, remoteAddr: "bare://alias"}
	c.ICall = rec
	c.initChannels()
	table.Store(700701, c)
	t.Cleanup(func() { table.Delete(700701) })

	// 模拟池别名：实例已被新连接复用（UID 变更），旧 uid 的滞留消息到达
	c.UID = 700702

	if processMessage(&msgQueueType{uid: 700701, data: testMsgBytes(t)}) {
		t.Fatal("✘ 别名消息被误判为 panic")
	}
	if n := rec.msgs.Load(); n != 0 {
		t.Fatalf("✘ 别名窗口未复核 UID，旧消息被投给新连接的回调: calls=%d", n)
	}
}

// ----------------------------------------------------------------------------
// W4 acquireICall 类型断言降级
// ----------------------------------------------------------------------------

// qaWNotACall 不实现 ICall 的类型
type qaWNotACall struct{ X int }

// TestQaW_AcquireICallNoPanicOnBadType 池返回异常类型时必须降级为错误。
// 修复前 client.go 的裸断言 icall.(ICall) 直接 panic。
func TestQaW_AcquireICallNoPanicOnBadType(t *testing.T) {
	prev := clientcall.Swap(nil)
	t.Cleanup(func() { clientcall.Store(prev) })

	RegisterCall("qa_w_bad_call", &qaWNotACall{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("✘ acquireICall 对异常类型 panic（修复前为裸断言）: %v", r)
		}
	}()
	call, _, err := acquireICall("qa_w_bad_call")
	if err == nil || call != nil {
		t.Fatalf("✘ 异常类型应返回错误: call=%v err=%v", call, err)
	}
	// 未注册类名同样返回错误
	if _, _, err := acquireICall("qa_w_missing"); err == nil {
		t.Fatal("✘ 未注册类名应返回错误")
	}
}

// ----------------------------------------------------------------------------
// W5 Run 全端口失败必须收回 Worker Pool
// ----------------------------------------------------------------------------
// TestQaW_RunAllPortsFailStopsWorkerPool 所有端口绑定失败时，已建好的
// Worker Pool 必须被 stopWorkerPool 收回，否则常驻 worker 协程泄漏。
func TestQaW_RunAllPortsFailStopsWorkerPool(t *testing.T) {
	snapshotM4Globals(t)
	prevPool := workerPool.Swap(nil)
	t.Cleanup(func() { workerPool.Store(prevPool) })

	// 70000 超出合法端口范围，net.Listen 必定失败（跨平台确定）
	cfg := &config.Cfg{Ws: &config.WebsocketConfig{
		Port:           []*config.WebsocketPort{{Port: 70000}},
		WorkerPoolSize: 2,
	}}
	ctx := corectx.WithCfg(context.Background(), cfg)

	if err := Run(ctx); err == nil {
		t.Fatal("✘ 非法端口 Run 竟然成功了")
	}
	if p := workerPool.Load(); p != nil {
		t.Fatalf("✘ 全端口失败后 Worker Pool 未收回（%d 个常驻协程泄漏）", p.size)
	}
	// 端口未被占用：ListenAndServe 失败不应登记 server
	if len(takeServers()) != 0 {
		t.Fatal("✘ 失败端口不应登记监听器")
	}
}

// ----------------------------------------------------------------------------
// clientpool/clientcall 原子化（裸包级变量换代竞争）
// ----------------------------------------------------------------------------

// TestQaW_ClientPoolAtomicSwapSmoke Run 换代写 clientpool/clientcall 与旧连接
// recycle/acquireICall 的读并发进行，不得崩溃。修复前两者是普通包级指针变量，
// 换代瞬间的裸写裸读即数据竞争（本机无 gcc 跑不了 -race，以并发冒烟 + 原子指针
// 结构消除竞争面）。
func TestQaW_ClientPoolAtomicSwapSmoke(t *testing.T) {
	prevPool, prevCall := clientpool.Swap(nil), clientcall.Swap(nil)
	t.Cleanup(func() { clientpool.Store(prevPool); clientcall.Store(prevCall) })

	stop := make(chan struct{})
	var wg sync.WaitGroup
	panics := make(chan any, 8)

	// 换代写方：模拟 Run 反复重建池
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panics <- r
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
			}
			clientpool.Store(&sync.Pool{New: func() any { return &Client{} }})
			RegisterCall("qa_w_smoke", &qaWRecordCall{})
		}
	}()

	// 读方：借还 Client 与回调实例（NewClient/recycle 的取值路径）
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if p := clientpool.Load(); p != nil {
					obj := p.Get()
					if c, ok := obj.(*Client); ok {
						p.Put(c)
					}
				}
				if call, pool, err := acquireICall("qa_w_smoke"); err == nil {
					if call != nil && pool != nil {
						pool.Put(call)
					}
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(panics)
	for r := range panics {
		t.Fatalf("✘ 换代并发下崩溃: %v", r)
	}
	if clientpool.Load() == nil || clientcall.Load() == nil {
		t.Fatal("✘ 冒烟结束后原子指针不应为空")
	}
}
