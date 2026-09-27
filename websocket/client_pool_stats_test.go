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

	gorillaws "github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	"touchgocore/syncmap"
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

	prevPool, prevCall, prevMap := clientpool, clientcall, loadClientMap()
	prevState := loadRunState()
	// 队列容量按「条」计，压小避免每条连接白白分配 1024 槽的发送队列
	prevQueue := wsQueue.Swap(&wsQueueParams{writeEntries: 16, readEntries: 8, dropOnFull: true})

	clientpool = &sync.Pool{New: newPooled}
	clientcall = syncmap.NewMap[string, *sync.Pool]()
	RegisterCall(reuseCallName, &reuseCall{})
	storeClientMap(syncmap.NewMap[int64, *Client]())
	// 代际状态：readLoop 需要 dispatchQueue() 非 nil，否则启动即退出
	currentRunState.Store(newRunState(context.Background(), 8))

	t.Cleanup(func() {
		clientpool, clientcall = prevPool, prevCall
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
	if err := peer.WriteMessage(gorillaws.BinaryMessage, []byte("world!")); err != nil {
		t.Fatalf("✘ 对端写入失败: %v", err)
	}
	if !waitCounter(3*time.Second, func() bool { return first.stats.messagesReceived.Load() >= 1 }) {
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
	pooled, ok := clientpool.Get().(*Client)
	if !ok || pooled == nil {
		t.Fatal("✘ 对象池未归还实例")
	}
	if pooled == c {
		t.Log("✔ 实例已归还对象池（同协程 Put→Get 命中）")
	}
	if pooled.counted.Load() {
		t.Fatal("✘ 池中实例仍标记为已计数：说明 Put 早于 CAS，新主人会顶掉这次 -1")
	}
	clientpool.Put(pooled)

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
				x, ok := clientpool.Get().(*Client)
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
