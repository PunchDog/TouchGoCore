package websocket

import (
	"context"
	"github.com/gin-gonic/gin"
	gorillaws "github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/syncmap"
	"touchgocore/util"
)

// ============================================================================
// M2 / M3 回归用例：Client 对象池的「统计计数」与「连接计数」交接。
//
//   M2 池复用不重置统计计数器：recycle 只清 wsConnect/UID/ICall，五个原子计数器
//      原封不动留在实例上，NewClient 复用后 GetStats 把两段连接的流量叠在一起。
//   M3 recycle 先回池后减计数：Put 之后另一条 NewClient 可以立刻 Get 到同一实例
//      并重置 counted，旧主人的 CAS 随即失效，currentConnections 单调漂移。
//
// 本文件刻意不复用 p0_/p1_ 等轮次夹具：那些文件被 .gitignore 的 **/p[0-3]_*_test.go
// 规则挡在版本库外，依赖它们的用例在 clone 后无法编译。这里自带全部夹具。
//
// 本机无 C 工具链，go test -race 不可用；并发正确性用「不变量断言 + 高并发借还
// 压测」替代（见 TestConcurrentBorrowReturnKeepsConnectionCount 的说明）。
// ============================================================================

// reuseCallName 是本文件专用的 ICall 注册名，避免与其它用例的注册项冲突
const reuseCallName = "wsPoolStatsCall"

// 池化实例上「遗留计数」的取值：互不相同，便于失败信息里定位是哪个计数器没清
const (
	dirtyMessagesSent     int64 = 11
	dirtyMessagesReceived int64 = 22
	dirtyBytesSent        int64 = 333
	dirtyBytesReceived    int64 = 444
	dirtyErrors           int64 = 5
)

// reuseCall 是不打日志的 ICall 实现：压测要跑几万轮借还，默认实现的 Info 日志
// 会把用例淹掉。
type reuseCall struct{}

func (reuseCall) OnConnect(client *Client) bool               { return true }
func (reuseCall) OnMessage(client *Client, msg proto.Message) {}
func (reuseCall) OnClose(client *Client)                      {}

// dirtyClient 造一个「带着上一条连接遗留计数」的池化实例。
func dirtyClient() *Client {
	c := &Client{}
	c.stats.messagesSent.Store(dirtyMessagesSent)
	c.stats.messagesReceived.Store(dirtyMessagesReceived)
	c.stats.bytesSent.Store(dirtyBytesSent)
	c.stats.bytesReceived.Store(dirtyBytesReceived)
	c.stats.errors.Store(dirtyErrors)
	return c
}

// setupPoolStatsEnv 装配 M2/M3 用例需要的包级依赖，测试结束自动还原。
//
// newPooled 决定池未命中时造出什么样的实例：M2 用例传入 dirtyClient，
// 这样即使 Get 没命中上一轮 Put 回去的脏实例（GC 清池等），取出的实例
// 依然带非零计数，断言不会因为「池恰好是干净的」而假通过。
func setupPoolStatsEnv(t *testing.T, newPooled func() any) {
	t.Helper()

	prevPool, prevCall, prevMap := clientpool.Swap(nil), clientcall.Swap(nil), loadClientMap()
	prevState := loadRunState()
	// 队列容量按「条」计，压小避免每条连接白白分配 1024 槽的发送队列
	prevQueue := wsQueue.Swap(&wsQueueParams{writeEntries: 16, readEntries: 8, dropOnFull: true})

	clientpool.Store(&sync.Pool{New: newPooled})
	RegisterCall(reuseCallName, &reuseCall{}) // clientcall 已由 RegisterCall 惰性建表
	storeClientMap(syncmap.NewMap[int64, *Client]())
	// 代际状态：readLoop 需要 dispatchQueue() 非 nil，否则启动即退出
	currentRunState.Store(newRunState(context.Background(), 8))

	t.Cleanup(func() {
		clientpool.Store(prevPool)
		clientcall.Store(prevCall)
		storeClientMap(prevMap)
		currentRunState.Store(prevState)
		wsQueue.Store(prevQueue)
	})
}

// waitCounter 在预算内轮询 cond（本机调度抖动较大，用轮询替代固定 sleep）
func waitCounter(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// waitConnBaseline 等待连接计数回到基线：recycle 由最后退出的常驻协程异步执行，
// 不等的话这一 -1 会落到后续用例头上，制造跨用例的假失败。
func waitConnBaseline(t *testing.T, base int64, d time.Duration) {
	t.Helper()
	if !waitCounter(d, func() bool { return GetServerStats().CurrentConnections == base }) {
		t.Fatalf("✘ 连接计数未在 %v 内回到基线: base=%d got=%d",
			d, base, GetServerStats().CurrentConnections)
	}
}

// newUpgradedConn 起一个临时 HTTP 服务完成真实握手，返回「服务端侧 / 对端侧」两条连接。
// 服务端侧交给 NewClient 接管，对端侧用于制造真实的收发流量。
func newUpgradedConn(t *testing.T) (serverSide, peer *gorillaws.Conn) {
	t.Helper()

	upgraded := make(chan *gorillaws.Conn, 1)
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := &gorillaws.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(*http.Request) bool { return true },
		}
		sc, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- sc
		<-hold // 握手协程必须活到用例结束，否则连接立刻被服务端关闭
		_ = sc.Close()
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	dialed, _, err := gorillaws.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		close(hold)
		srv.Close()
		t.Fatalf("✘ 测试握手拨号失败: %v", err)
	}
	select {
	case serverSide = <-upgraded:
	case <-time.After(5 * time.Second):
		dialed.Close()
		close(hold)
		srv.Close()
		t.Fatal("✘ 测试握手升级超时")
	}

	t.Cleanup(func() {
		close(hold)
		dialed.Close()
		srv.Close()
	})
	return serverSide, dialed
}

// ----------------------------------------------------------------------------
// M2：池复用必须重置统计计数器
// ----------------------------------------------------------------------------

// TestPooledClientStatsResetOnReuse 回归（M2）：从对象池取出的实例必须把
// messagesSent / messagesReceived / bytesSent / bytesReceived / errors 五个
// 计数器清零，GetStats 只反映本条连接。
//
// 修复前 NewClient 只刷 connectTime / lastActivity 两个时间戳，池化实例带着
// 上一条连接的计数继续累加，监控上表现为「新连接一上来就有几万条消息」。
func TestPooledClientStatsResetOnReuse(t *testing.T) {
	setupPoolStatsEnv(t, func() any { return dirtyClient() })
	base := GetServerStats().CurrentConnections

	var second *Client
	t.Cleanup(func() {
		if second != nil {
			second.Close("用例结束")
		}
		waitConnBaseline(t, base, 3*time.Second)
	})

	// --- 第一段：出厂即带遗留计数的实例，建连后必须干净 ---
	firstConn, peer := newUpgradedConn(t)
	first, err := NewClient(firstConn, "reuse://first", reuseCallName)
	if err != nil {
		t.Fatalf("✘ 第一条连接建立失败: %v", err)
	}
	if st := first.GetStats(); st.MessagesSent != 0 || st.MessagesReceived != 0 ||
		st.BytesSent != 0 || st.BytesReceived != 0 || st.Errors != 0 {
		t.Fatalf("✘ 池中取出的实例未清零统计（遗留值 %d/%d/%d/%d/%d 泄漏进本条连接）: %+v",
			dirtyMessagesSent, dirtyMessagesReceived, dirtyBytesSent, dirtyBytesReceived, dirtyErrors, st)
	}
	if st := first.GetStats(); st.ConnectTime.IsZero() || st.LastActivity.IsZero() {
		t.Fatalf("✘ 连接时间戳未随复用重新初始化: %+v", st)
	}

	// --- 让计数真的跑起来：出站经 handleLoop，入站经 readLoop ---
	first.SendMsg([]byte("hello"))
	if !waitCounter(3*time.Second, func() bool { return first.stats.messagesSent.Load() >= 1 }) {
		t.Fatalf("✘ 出站计数未推进（用例前提不成立）: %+v", first.GetStats())
	}
	// W2 之后入站计数的权威点在 processMessage（消息真正被处理），readLoop 只入队不计数。
	// 因此从本代 msgQueue 取出对端消息交给 processMessage，推进入站计数。
	if err := peer.WriteMessage(gorillaws.BinaryMessage, testMsgBytes(t)); err != nil {
		t.Fatalf("✘ 对端写入失败: %v", err)
	}
	q := first.dispatchQueue()
	if q == nil {
		t.Fatal("✘ 用例前提不成立：无代际队列")
	}
	var inbound *msgQueueType
	select {
	case inbound = <-q:
	case <-time.After(3 * time.Second):
		t.Fatalf("✘ readLoop 未把入站消息投递到队列: %+v", first.GetStats())
	}
	processMessage(inbound)
	if first.stats.messagesReceived.Load() < 1 {
		t.Fatalf("✘ 入站计数未推进（用例前提不成立）: %+v", first.GetStats())
	}
	// 补齐另外三个计数器，凑成「五个都脏」的复用前状态
	first.stats.bytesSent.Add(1000)
	first.stats.bytesReceived.Add(2000)
	first.stats.errors.Add(3)
	before := first.GetStats()

	// --- 关闭第一段：实例带着脏计数回池（recycle 按设计不清统计） ---
	first.Close("第一段结束")
	if !waitCounter(3*time.Second, func() bool { return first.liveLoops.Load() == 0 }) {
		t.Fatal("✘ 常驻协程未退出，实例不会回池")
	}
	waitConnBaseline(t, base, 3*time.Second)

	// --- 第二段：复用后必须从零起算 ---
	secondConn, _ := newUpgradedConn(t)
	second, err = NewClient(secondConn, "reuse://second", reuseCallName)
	if err != nil {
		t.Fatalf("✘ 第二条连接建立失败: %v", err)
	}
	if second == first {
		t.Log("✔ 第二段命中池复用（与第一段同一实例）")
	} else {
		t.Log("ℹ 第二段未命中第一段的实例，改用池 New 造出的脏实例校验清零路径")
	}

	got := second.GetStats()
	if got.MessagesSent != 0 || got.MessagesReceived != 0 ||
		got.BytesSent != 0 || got.BytesReceived != 0 || got.Errors != 0 {
		t.Fatalf("✘ 复用后 GetStats 跨连接失真:\n  上一条连接: %+v\n  本条连接:   %+v", before, got)
	}
	if got.ConnectTime.IsZero() || got.LastActivity.IsZero() {
		t.Fatalf("✘ 复用后时间戳未重新初始化: %+v", got)
	}
	t.Logf("✔ 复用后计数从零起算（上一条连接遗留 %+v 已清空）", before)
}

// ----------------------------------------------------------------------------
// M3：recycle 必须先减连接数再回池
// ----------------------------------------------------------------------------

// TestRecycleDecrementsBeforePoolReturn 回归（M3）：单协程下的顺序契约——
// recycle 返回时连接数已复原、counted 已摘除，且回池的实例对新主人呈现
// counted=false（若 Put 早于 CAS，池里会短暂存在「仍标记为已计数」的实例）。
func TestRecycleDecrementsBeforePoolReturn(t *testing.T) {
	setupPoolStatsEnv(t, func() any { return &Client{} })
	base := GetServerStats().CurrentConnections

	c := &Client{}
	c.iCallName = reuseCallName
	// ICall 留 nil：本用例只关心连接计数与回池顺序，不走回调池
	c.counted.Store(true)
	c.liveLoops.Store(1)
	UpdateConnectionStats(true) // 复刻 NewClient 成功路径的 +1
	if got := GetServerStats().CurrentConnections; got != base+1 {
		t.Fatalf("✘ 建连 +1 未生效: base=%d got=%d", base, got)
	}

	c.finishLoop() // liveLoops → 0 → recycle

	waitConnBaseline(t, base, time.Second)
	if c.counted.Load() {
		t.Fatal("✘ 回收后 counted 仍为 true，下一次复用会重复 -1")
	}

	// 回池的实例必须已经摘掉计数：新主人拿到手时 counted 必为 false
	pooled, ok := clientpool.Load().Get().(*Client)
	if !ok || pooled == nil {
		t.Fatal("✘ 对象池未归还实例")
	}
	if pooled == c {
		t.Log("✔ 实例已归还对象池（同协程 Put→Get 命中）")
	}
	if pooled.counted.Load() {
		t.Fatal("✘ 池中实例仍标记为已计数：说明 Put 早于 CAS，新主人会顶掉这次 -1")
	}
	clientpool.Load().Put(pooled)

	// 重复回收必须是 no-op，不得二次 -1
	c.recycle()
	c.recycle()
	if got := GetServerStats().CurrentConnections; got != base {
		t.Fatalf("✘ 重复回收造成连接数二次递减: base=%d got=%d", base, got)
	}
}

// TestConcurrentBorrowReturnKeepsConnectionCount 回归（M3）：高并发借还后
// currentConnections 必须精确回到基线，不得漂移。
//
// 缺陷形态：recycle 先 clientpool.Put(c) 再 CAS 减计数。Put 之后实例已归新主人，
// 新主人的 counted.Store(false) 一旦抢在旧主人的 CAS 之前，这次 -1 就永久丢失
// （另一种交错是旧 CAS 摘走新主人的 +1，新主人回收时同样不减），
// currentConnections 只增不减。
//
// 用例设计（本机无 -race，用不变量替代竞态检测）：
//   - 生产者：不断造连接（counted=true 且 +1）并经 finishLoop → recycle 归还，
//     全程只碰原子量，实例都是全新对象，不存在 sync.Once 复位的写竞争；
//   - 探测器：热循环从池里 Get，一旦发现 counted=true 就记录违约（这正是
//     「Put 早于 CAS」的签名），随后复刻新主人的 counted.Store(false)——
//     就是这一步会让旧主人的 CAS 失效，把窗口命中固化成可观测的计数漂移。
//
// 窗口只有 Put 与 CAS 之间的几条指令，是否命中取决于调度；因此本用例的价值在于
// 「修复后恒定通过、一旦顺序被改回去就有机会立刻抓到」，顺序本身由代码评审背书。
func TestConcurrentBorrowReturnKeepsConnectionCount(t *testing.T) {
	setupPoolStatsEnv(t, func() any { return &Client{} })
	base := GetServerStats().CurrentConnections

	const (
		producers      = 16
		cyclesEach     = 20000
		proberCount    = 6
		proberBudgetMS = 8000
	)

	var (
		violations atomic.Int64 // 从池里取到「仍已计数」实例的次数
		probed     atomic.Int64 // 探测器累计取样次数
		stop       = make(chan struct{})
		probers    sync.WaitGroup
	)

	for i := 0; i < proberCount; i++ {
		probers.Add(1)
		go func() {
			defer probers.Done()
			deadline := time.Now().Add(proberBudgetMS * time.Millisecond)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if time.Now().After(deadline) {
					return
				}
				pool := clientpool.Load()
				if pool == nil {
					return
				}
				x, ok := pool.Get().(*Client)
				if !ok || x == nil {
					continue
				}
				probed.Add(1)
				if x.counted.Load() {
					violations.Add(1)
				}
				// 复刻新主人的重置动作：旧主人尚未 CAS 时，这一步会让它的 -1 落空。
				// 取出的实例不再放回：探测器每次 Get 都必须走「本地无货 → 跨 P 偷取」
				// 的慢路径，才可能抓到别的协程刚 Put 进来的那一件（放回会让 Get
				// 一直命中自己 P 的私有槽，永远看不见别人刚归还的实例）。
				x.counted.Store(false)
			}
		}()
	}

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < cyclesEach; i++ {
				c := &Client{}
				c.iCallName = reuseCallName
				c.counted.Store(true)
				c.liveLoops.Store(1)
				UpdateConnectionStats(true) // NewClient 成功路径的 +1
				c.finishLoop()              // 最后一个协程退出 → recycle（-1 并回池）
			}
		}()
	}
	wg.Wait()
	close(stop)
	probers.Wait()

	if v := violations.Load(); v != 0 {
		t.Fatalf("✘ 从对象池取到 %d 个仍标记 counted 的实例：回池发生在减计数之前", v)
	}
	if got := GetServerStats().CurrentConnections; got != base {
		t.Fatalf("✘ %d 轮并发借还后连接数漂移: base=%d got=%d 差=%d",
			producers*cyclesEach, base, got, got-base)
	}
	t.Logf("✔ %d 轮并发借还 + %d 次池探测，连接数精确归位（基线 %d）",
		producers*cyclesEach, probed.Load(), base)
}

// ============================================================================
// tickLoop 的 pool 代际隔离回归。
//
// 事故形状：workerPool 是包级原子指针，每次 Run 重建；tickLoop 原先直接回读它。
// Run→Stop→Run 换代窗口里，旧代 Tick 退出前仍可能消费旧 msgQueue 的存量消息，
// 此时包级指针已指向新一代的池——旧连接的消息被投进新池，与新代 Tick 混投。
// 修复：runState 增加 pool 字段，initWorkerPool 同时写包级指针与本代快照，
// tickLoop 只读 state.pool。本机无 -race，用「旧代消息只进旧池计数」直接验证。
// ============================================================================

func poolTotalMessages(p *workerPoolState) int64 {
	var n int64
	for _, s := range p.stats {
		n += s.Messages.Load()
	}
	return n
}

func stopPoolAndWait(p *workerPoolState) {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.wg.Wait()
}

func TestTickLoop_DispatchesOnlyToOwnGenerationPool(t *testing.T) {
	// 隔离包级 workerPool，测后还原
	prevPool := workerPool.Swap(nil)
	t.Cleanup(func() {
		if p := workerPool.Swap(prevPool); p != nil {
			stopPoolAndWait(p)
		}
	})

	state := newRunState(context.Background(), 8)
	go tickLoop(state)
	t.Cleanup(func() {
		close(state.closeCh)
		select {
		case <-state.tickDone:
		case <-time.After(2 * time.Second):
			t.Error("tickLoop 未在预算内退出")
		}
	})

	// 第一代：poolA 同时挂在包级指针与 state.pool
	initWorkerPool(2, false, state)
	poolA := workerPool.Load()
	if poolA == nil {
		t.Fatal("poolA 未装配")
	}
	if state.pool.Load() != poolA {
		t.Fatal("state.pool 未绑定本代池")
	}

	// 模拟换代：新一轮 Run 重建包级池，但旧代 state.pool 必须仍指 poolA
	initWorkerPool(2, false, nil)
	poolB := workerPool.Load()
	if poolB == nil || poolB == poolA {
		t.Fatalf("新代池未生效: %p vs %p", poolB, poolA)
	}
	if state.pool.Load() != poolA {
		t.Fatal("旧代 state.pool 漂到了新一代（tickLoop 会跨代投递）")
	}

	// 投进旧代 msgQueue：Tick 只能消费进 poolA（uid 无客户端，processMessage
	// 报错不影响 safeProcess 计数，计数即「进了哪个池」的证据）
	state.msgQueue <- &msgQueueType{uid: 889900, data: []byte("probe")}
	if !waitForMsg(func() bool { return poolTotalMessages(poolA) >= 1 }, 2*time.Second) {
		t.Fatal("旧代消息未被旧代池消费")
	}
	// 给误投新池一点传播时间再反证
	time.Sleep(100 * time.Millisecond)
	if n := poolTotalMessages(poolB); n != 0 {
		t.Fatalf("旧代 Tick 把消息投进了新一代的池: poolB 计数=%d", n)
	}
	if seq := poolB.dispatchSeq.Load(); seq != 0 {
		t.Fatalf("新池不应有任何派发: dispatchSeq=%d", seq)
	}

	// 收掉 poolA（Tick 的 shutdownWebsocket 只会停包级指针指向的 poolB）
	stopPoolAndWait(poolA)
	t.Log("✔ 旧代 Tick 只向本代池投递，换代隔离成立")
}

func waitForMsg(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// ============================================================================
// M4 回归用例：websocket 包级状态跨 Run 代际混用修复。
//
// 修复前的三连缺陷：
//  1. closeCh / msgQueue / tickDone / stopOnce 都是包级变量，二次 Run 直接覆盖；
//  2. 旧 Tick 仍在 select 这两个名字 → 「旧 Tick + 新 Tick 同时消费新 msgQueue」，
//     新旧连接的消息跨代混投；
//  3. 旧 Tick 永远收不到旧 closeCh 的关闭信号（那个 chan 已被新值替换且无人引用），
//     协程泄漏直到进程退出。
//
// 修复后这些字段全部收进 runState 实例：每代 Run 持有独立 state，Tick 与 Client
// 通过自己捕获的指针访问，Run 在切换前 stopRunState 旧代并等其 tickDone。
//
// 本机无 gcc，go test -race 不可用，因此用「代际指针身份 + tickDone 关闭 + 队列
// 滞留观测」三组事实替代竞态检测：旧 Tick 真退出 ↔ 旧 tickDone 真关闭；
// 新代消息只被新代消费 ↔ 旧 msgQueue 永远滞留、新 msgQueue 立刻被取走。
// ============================================================================

// makeM4Cfg 装配一个最小可启动的 ws 配置：监听端口为空（不起 HTTP 服务器），
// 仅用于驱动 Run 的代际状态切换路径。
func makeM4Cfg() *config.Cfg {
	return &config.Cfg{
		Ws: &config.WebsocketConfig{},
	}
}

// waitTickDone 在预算内等待 Tick 协程退出。
func waitTickDone(t *testing.T, s *runState, d time.Duration, what string) {
	t.Helper()
	select {
	case <-s.tickDone:
	case <-time.After(d):
		t.Fatalf("✘ %s: tickDone 在 %v 内未关闭", what, d)
	}
}

// assertTickNotDone 断言 tickDone 仍未关闭（Tick 还在跑）。
func assertTickNotDone(t *testing.T, s *runState, what string) {
	t.Helper()
	select {
	case <-s.tickDone:
		t.Fatalf("✘ %s: tickDone 不应已关闭", what)
	default:
	}
}

// snapshotM4Globals 在测试前后还原会被 Run 改写的全局指针，避免污染其它用例。
func snapshotM4Globals(t *testing.T) {
	t.Helper()
	prevState := loadRunState()
	prevQueue := wsQueue.Swap(nil)
	prevRunCtx := runCtxValue.Swap(nil)
	prevClientMap := loadClientMap()
	prevPool := clientpool.Swap(nil)
	t.Cleanup(func() {
		// 先把测试期间起的 Tick 收掉，避免它在新测试里继续消费旧 state
		if s := loadRunState(); s != nil {
			stopRunState(s, context.Background())
		}
		currentRunState.Store(prevState)
		wsQueue.Store(prevQueue)
		runCtxValue.Store(prevRunCtx)
		storeClientMap(prevClientMap)
		clientpool.Store(prevPool)
	})
}

// TestStopRunStateTerminatesBoundTick 验证 stopRunState 必定让绑定该代的 Tick 退出。
//
// 修复前 closeCh 是包级变量，二次 Run 覆盖后旧 Tick 永远收不到关闭信号 → 协程泄漏。
// 现在 closeCh 是 state 字段，Tick 通过自己的 state 指针读它，stopRunState 一关必退。
func TestStopRunStateTerminatesBoundTick(t *testing.T) {
	snapshotM4Globals(t)

	s1 := newRunState(context.Background(), 4)
	currentRunState.Store(s1)
	go tickLoop(s1)

	// 给 Tick 协程一点时间进入 select；本机调度抖动较大，50ms 已足够
	time.Sleep(50 * time.Millisecond)
	assertTickNotDone(t, s1, "未触发停机时 Tick 不应自行退出")

	stopRunState(s1, context.Background())
	waitTickDone(t, s1, time.Second, "stopRunState 后旧 Tick 必须退出")

	// 幂等：再次调用必须立即返回，不得 panic（重复 close 已关闭的 chan）
	stopRunState(s1, context.Background())
}

// TestRunTwiceIsolatesGenerations 验证：二次 Run（中间不调 Stop）后
//   - 旧 Tick 已退出；
//   - 新旧 runState 是不同实例，代号递增；
//   - 新代 msgQueue 仍被新代 Tick 消费；
//   - 旧代 msgQueue 不再有消费者，消息滞留（不会跨代混投）。
func TestRunTwiceIsolatesGenerations(t *testing.T) {
	snapshotM4Globals(t)

	ctx1 := corectx.WithCfg(context.Background(), makeM4Cfg())
	if err := Run(ctx1); err != nil {
		t.Fatalf("Run #1 失败: %v", err)
	}
	state1 := loadRunState()
	if state1 == nil {
		t.Fatal("✘ Run #1 后未装上代际状态")
	}
	gen1 := state1.generation
	assertTickNotDone(t, state1, "Run #1 后新 Tick 应在运行")

	// Run #2 不先 Stop：必须自己把上一代收掉。这是修复前协程泄漏的复现路径。
	ctx2 := corectx.WithCfg(context.Background(), makeM4Cfg())
	if err := Run(ctx2); err != nil {
		t.Fatalf("Run #2 失败: %v", err)
	}
	state2 := loadRunState()
	if state2 == nil {
		t.Fatal("✘ Run #2 后未装上代际状态")
	}
	if state2 == state1 {
		t.Fatal("✘ Run #2 复用了 Run #1 的 runState 实例（包级状态未隔离）")
	}
	if state2.generation <= gen1 {
		t.Fatalf("✘ 代号未递增: gen1=%d gen2=%d", gen1, state2.generation)
	}
	if state2.msgQueue == state1.msgQueue {
		t.Fatal("✘ 新旧 msgQueue 是同一个 chan（跨代混投的根源）")
	}

	// Run 内部已 stopRunState 旧代并等过 tickDone：此处必为已关闭。
	waitTickDone(t, state1, time.Second, "Run #2 切换后旧 Tick 必须已退出")
	assertTickNotDone(t, state2, "Run #2 后新 Tick 应在运行")

	// 新代 msgQueue 必须被新代 Tick 消费（无 client 时 processMessage 会记录
	// 「客户端未找到」错误，但消息一定会被取走）。
	probe := &msgQueueType{uid: 999001, data: []byte("probe")}
	select {
	case state2.msgQueue <- probe:
	default:
		t.Fatal("✘ 新代 msgQueue 容量异常，无法投递探针")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(state2.msgQueue) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(state2.msgQueue) != 0 {
		t.Fatalf("✘ 新代 Tick 未消费新代 msgQueue: len=%d", len(state2.msgQueue))
	}

	// 旧代 msgQueue 不再有消费者：投一条进去后必须滞留。
	stale := &msgQueueType{uid: 999002, data: []byte("stale")}
	select {
	case state1.msgQueue <- stale:
	default:
		t.Fatal("✘ 旧代 msgQueue 容量异常，无法投递滞留探针")
	}
	time.Sleep(100 * time.Millisecond)
	if len(state1.msgQueue) != 1 {
		t.Fatalf("✘ 旧代 msgQueue 被消费了（不应再有活跃 Tick）: len=%d", len(state1.msgQueue))
	}
	if got := <-state1.msgQueue; got != stale {
		t.Fatal("✘ 旧代 msgQueue 中的探针被替换")
	}
}

// TestClientBindsToRunStateAtConnect 验证 Client 在 NewClient 时锁定本代 runState，
// 后续 Run 换代不会让旧连接的消息流向新一代的 msgQueue。
//
// 修复前 readLoop 直接读写包级 msgQueue：Run 换代后旧连接的消息会落进新代队列，
// 被新代 Tick 当成新连接的消息处理（即「跨代混投」）。
func TestClientBindsToRunStateAtConnect(t *testing.T) {
	snapshotM4Globals(t)

	// 装第一代。本用例不起 Tick，预关 tickDone 让 cleanup 里的 stopRunState 立即返回。
	s1 := newRunState(context.Background(), 8)
	close(s1.tickDone)
	currentRunState.Store(s1)

	// 模拟 NewClient 中的代际快照（不走完整 NewClient，避免依赖 ws 连接）
	c := &Client{UID: 4242, remoteAddr: "bare://m4"}
	c.runState = loadRunState()
	c.initChannels()

	if c.runState != s1 {
		t.Fatal("✘ Client 未绑定到当前代际")
	}
	if c.dispatchQueue() != s1.msgQueue {
		t.Fatal("✘ dispatchQueue 未返回绑定代际的 msgQueue")
	}

	// 换代：currentRunState 指向 s2，但 c.runState 仍是 s1
	s2 := newRunState(context.Background(), 8)
	close(s2.tickDone)
	currentRunState.Store(s2)

	if c.dispatchQueue() != s1.msgQueue {
		t.Fatal("✘ Run 换代后 Client.dispatchQueue 漂到了新一代（跨代混投）")
	}
	if c.dispatchQueue() == s2.msgQueue {
		t.Fatal("✘ 旧连接的消息会落进新一代 msgQueue")
	}

	// 裸构造（未走 NewClient）的 Client 才回退到当前代——这是测试旁路，业务路径不会进
	bare := &Client{UID: 1}
	if bare.dispatchQueue() != s2.msgQueue {
		t.Fatal("✘ 裸 Client 应回退到当前代际 msgQueue")
	}
}

func TestWebsocketPath(t *testing.T) {
	cases := []struct {
		raw, fallback, want string
	}{
		{"", "/ws", "/ws"},
		{"ws", "/ws", "/ws"},
		{"/game", "/ws", "/game"},
		{"wss://example.com/inner", "/ws", "/inner"},
		{"http://127.0.0.1:8000/ws", "/ws", "/ws"},
	}
	for _, c := range cases {
		if got := websocketPath(c.raw, c.fallback); got != c.want {
			t.Fatalf("websocketPath(%q)=%q want %q", c.raw, got, c.want)
		}
	}
}

func TestWebsocketListenPathsDedup(t *testing.T) {
	paths := websocketListenPaths(&config.WebsocketConfig{URL: "/ws", InURL: "ws"})
	if len(paths) != 1 || paths[0] != "/ws" {
		t.Fatalf("paths=%v", paths)
	}
	paths = websocketListenPaths(&config.WebsocketConfig{URL: "/ws", InURL: "/inws"})
	if len(paths) != 2 {
		t.Fatalf("expected two paths, got %v", paths)
	}
}

func TestOriginPolicyAllows(t *testing.T) {
	p := originPolicy{
		allowedOrigins: []string{"https://example.com", "*.game.local"},
		checkOrigin:    true,
	}
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"a.game.local", true},
		{"evil.com", false},
		// 通配不得被「拼前缀」绕过
		{"evil-example.com", false},
		{"game.local", false}, // 裸域本身不属于子域
	}
	for _, c := range cases {
		if got := p.allows(c.host); got != c.want {
			t.Fatalf("allows(%q)=%v want %v", c.host, got, c.want)
		}
	}
}

func TestGetClientIPIgnoresUntrustedXFF(t *testing.T) {
	prev := wsRunCtx()
	setWsRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Ws: &config.WebsocketConfig{TrustedProxies: []string{"10.0.0.1"}},
	}))
	t.Cleanup(func() { setWsRunCtx(prev) })

	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	req.RemoteAddr = "8.8.8.8:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if ip := getClientIP(req); ip != "8.8.8.8" {
		t.Fatalf("untrusted XFF should be ignored, got %s", ip)
	}

	req.RemoteAddr = "10.0.0.1:80"
	if ip := getClientIP(req); ip != "1.2.3.4" {
		t.Fatalf("trusted proxy XFF, got %s", ip)
	}
}

func TestExtractAuthToken(t *testing.T) {
	prev := wsRunCtx()
	setWsRunCtx(corectx.WithCfg(context.Background(), &config.Cfg{
		Ws: &config.WebsocketConfig{AuthTokenHeader: "X-Auth-Token", AuthTokenQuery: "token"},
	}))
	t.Cleanup(func() { setWsRunCtx(prev) })

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/ws?token=fromquery", nil)
	if got := extractAuthToken(c); got != "fromquery" {
		t.Fatalf("query token=%q", got)
	}
	c.Request = httptest.NewRequest(http.MethodGet, "/ws?token=fromquery", nil)
	c.Request.Header.Set("X-Auth-Token", "fromheader")
	if got := extractAuthToken(c); got != "fromheader" {
		t.Fatalf("header should win, got %q", got)
	}
}

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
