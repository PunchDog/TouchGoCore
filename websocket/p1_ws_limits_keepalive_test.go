package websocket

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
)

// ============================================================================
// 阶段 9（S41-S44）回归用例：
//   S41 握手缓冲与消息上限不得按「字节」当「条数」用，也不再写死 10MiB/1MiB
//   S42 UID 取号必须单次原子；拨号退避必须可被生命周期取消
//   S43 Origin 校验不再有「空 Origin 自比自」和通配前缀绕过；upgrader 按次构造；
//       监听器登记表并发安全
//   S44 Worker 队列满不得回头阻塞唯一的消费协程；Worker Pool 每次 Run 重建
// ============================================================================

// mustReturn 断言 fn 在 d 内返回，否则视为挂死（本机无 gcc，用超时替代 race 检测）
func mustReturn(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("✘ %s 未在 %v 内返回（疑似阻塞）", what, d)
	}
}

// useWsCfg 把 wsRunCtx 换成带配置的生命周期，测试结束自动还原
func useWsCfg(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	prev := wsRunCtx()
	setWsRunCtx(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() { setWsRunCtx(prev) })
}

// ----------------------------------------------------------------------------
// S41
// ----------------------------------------------------------------------------

// TestUpgraderBufferSizeBounded 回归（S41）：握手读写缓冲默认必须是 KB 级。
// 每条连接 2×10MiB 的 bufio 缓冲，让「大量半开握手」直接变成内存型 DoS。
func TestUpgraderBufferSizeBounded(t *testing.T) {
	if defaultUpgraderBufferSize > 64*1024 {
		t.Fatalf("✘ 握手缓冲默认值过大: %d", defaultUpgraderBufferSize)
	}

	u := newUpgrader(nil)
	if u.ReadBufferSize != defaultUpgraderBufferSize || u.WriteBufferSize != defaultUpgraderBufferSize {
		t.Fatalf("✘ 未配置时缓冲应为默认值: read=%d write=%d", u.ReadBufferSize, u.WriteBufferSize)
	}

	u = newUpgrader(&config.WebsocketConfig{UpgraderReadBuffer: 8192, UpgraderWriteBuffer: 4096})
	if u.ReadBufferSize != 8192 || u.WriteBufferSize != 4096 {
		t.Fatalf("✘ 配置未生效: read=%d write=%d", u.ReadBufferSize, u.WriteBufferSize)
	}
}

// TestMaxMessageSizeFollowsConfig 回归（S41）：读上限必须跟随配置，
// 否则业务把包上限调到 4MiB 后，超包连接会被传输层静默掐断。
func TestMaxMessageSizeFollowsConfig(t *testing.T) {
	useWsCfg(t, &config.Cfg{Server: &config.ServerConfig{MaxMsgSize: 4 << 20}})
	if got := currentMaxMessageSize(); got != 4<<20 {
		t.Fatalf("✘ 未采用 server.max_msg_size: %d", got)
	}

	useWsCfg(t, &config.Cfg{
		Server: &config.ServerConfig{MaxMsgSize: 4 << 20},
		Ws:     &config.WebsocketConfig{MaxMessageSize: 64 << 10},
	})
	if got := currentMaxMessageSize(); got != 64<<10 {
		t.Fatalf("✘ ws.max_message_size 应优先于 server.max_msg_size: %d", got)
	}

	useWsCfg(t, &config.Cfg{})
	if got := currentMaxMessageSize(); got != defaultMaxMessageSize {
		t.Fatalf("✘ 全部未配置时应回落默认值: got=%d want=%d", got, defaultMaxMessageSize)
	}
}

// TestQueueCapacityUnitIsEntries 回归（S41）：队列容量的语义是「条数」。
// 修复前 DEFAULT_WRITE_BUFFER_SIZE=1MiB 被直接当容量传给 make(chan)，
// 每条连接的发送队列一上来预留 100 万个槽。
func TestQueueCapacityUnitIsEntries(t *testing.T) {
	if defaultSendQueueEntries > 8192 || defaultRecvQueueEntries > 8192 {
		t.Fatalf("✘ 默认队列容量不像「条数」: send=%d recv=%d", defaultSendQueueEntries, defaultRecvQueueEntries)
	}

	prevEntry := wsQueueParams{writeEntries: 0}
	useQueueParams(t, prevEntry) // 结束前恢复默认快照，避免污染其它用例
	if got := writeQueueCap(); got != defaultSendQueueEntries {
		t.Fatalf("✘ 未配置时发送队列容量应为默认条数: got=%d want=%d", got, defaultSendQueueEntries)
	}
	useQueueParams(t, wsQueueParams{writeEntries: 32})
	if got := writeQueueCap(); got != 32 {
		t.Fatalf("✘ 配置的队列容量未生效: %d", got)
	}
}

// ----------------------------------------------------------------------------
// S42
// ----------------------------------------------------------------------------

// TestNextUIDUniqueUnderConcurrency 回归（S42 M?）：并发建连时 UID 必须两两不同。
// 修复前「Add 之后再 Load」，两条连接能拿到同一个 UID —— 后落库者把前者的
// clientMap 表项覆盖，表现为互相踢线。
func TestNextUIDUniqueUnderConcurrency(t *testing.T) {
	const n = 500
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = nextUID()
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]struct{}, n)
	for _, id := range ids {
		if id <= 0 {
			t.Fatalf("✘ 非法 UID: %d", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("✘ UID 重复（并发双主）: %d", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != n {
		t.Fatalf("✘ UID 数量不守恒: %d != %d", len(seen), n)
	}
}

// TestDialBackoffHonorsLifecycleCancel 回归（S42）：拨号退避必须可取消。
// 修复前用裸 time.Sleep，停机途中最坏要睡满 2+4 秒才肯放手。
func TestDialBackoffHonorsLifecycleCancel(t *testing.T) {
	prev := wsRunCtx()
	ctx, cancel := context.WithCancel(context.Background())
	setWsRunCtx(corectx.WithCfg(ctx, &config.Cfg{}))
	cancel() // 生命周期已结束
	t.Cleanup(func() { setWsRunCtx(prev) })

	c := &Client{}
	start := time.Now()
	err := c.connectionDial("ws://127.0.0.1:1/ws")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("✘ 已取消的生命周期里拨号不该成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("✘ 未把取消原因回传: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("✘ 退避未被取消打断: 耗时=%v", elapsed)
	}
}

// ----------------------------------------------------------------------------
// S43
// ----------------------------------------------------------------------------

// TestOriginCheckRejectsEmptyAndWildcardBypass 回归（S43）：
// 空 Origin 不再拿 r.Host 自比自，通配项不再被拼前缀绕过。
func TestOriginCheckRejectsEmptyAndWildcardBypass(t *testing.T) {
	p := originPolicy{
		allowedOrigins: []string{"https://game.local"},
		checkOrigin:    true,
	}

	newReq := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/ws", nil)
		r.RemoteAddr = "8.8.8.8:1234"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}

	if p.check(newReq("")) {
		t.Fatal("✘ 空 Origin 默认被放行（修复前用 r.Host 自比自）")
	}
	p.allowEmptyOrigin = true
	if !p.check(newReq("")) {
		t.Fatal("✘ allow_empty_origin 打开后仍拒绝空 Origin")
	}
	p.allowEmptyOrigin = false

	if !p.check(newReq("https://game.local")) {
		t.Fatal("✘ 白名单内 Origin 被拒")
	}
	if p.check(newReq("https://evil-game.local")) {
		t.Fatal("✘ 通配/前后缀拼接绕过白名单")
	}
	if p.check(newReq("not a valid url ://")) {
		t.Fatal("✘ 非法 Origin 被放行")
	}

	// CheckOrigin 关闭时保持既有语义：放行但告警
	if !(originPolicy{}).check(newReq("https://anything")) {
		t.Fatal("✘ CheckOrigin=false 时不应拦截握手")
	}
}

// TestUpgraderBuiltPerListenCall 回归（S43）：upgrader 不能再由包级 sync.Once
// 固化——首次调用时的配置会永久钉死后续所有 Run（也污染同包其它测试）。
func TestUpgraderBuiltPerListenCall(t *testing.T) {
	first := newUpgrader(&config.WebsocketConfig{UpgraderReadBuffer: 1024})
	second := newUpgrader(&config.WebsocketConfig{UpgraderReadBuffer: 2048})

	if first == second {
		t.Fatal("✘ 两次构造返回了同一个 upgrader 实例")
	}
	if first.ReadBufferSize == second.ReadBufferSize {
		t.Fatalf("✘ 新配置未生效，仍被首次构造钉死: %d", first.ReadBufferSize)
	}
}

// TestServerListRegisterIsConcurrencySafe 回归（S43）：登记表并发追加不得丢监听器。
// 丢掉的监听器永远不会被 Shutdown，端口一直占到进程退出。
func TestServerListRegisterIsConcurrencySafe(t *testing.T) {
	prev := takeServers()
	t.Cleanup(func() {
		for _, s := range prev {
			registerServer(s)
		}
	})

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registerServer(&http.Server{Addr: "127.0.0.1:0"})
		}()
	}
	wg.Wait()

	got := takeServers()
	if len(got) != n {
		t.Fatalf("✘ 并发登记丢失监听器: got=%d want=%d", len(got), n)
	}
	if len(takeServers()) != 0 {
		t.Fatal("✘ takeServers 之后登记表未清空")
	}
}

// ----------------------------------------------------------------------------
// S44
// ----------------------------------------------------------------------------

// withWorkerPool 装入一个测试用池，测试结束还原并停掉 Worker 协程
func withWorkerPool(t *testing.T, pool *workerPoolState) *workerPoolState {
	t.Helper()
	prev := workerPool.Swap(pool)
	t.Cleanup(func() {
		workerPool.Store(prev)
		if pool != nil && pool.stop != nil {
			select {
			case <-pool.stop:
			default:
				close(pool.stop)
			}
		}
	})
	return prev
}

// TestWorkerPoolLifecycleIsPerRun 回归（S44）：每轮 Run 必须拿到全新的池。
// 复用包级 stop 通道时，第二轮 Worker 一启动就收到上一轮的关闭信号（消息再没人
// 处理），而重复 close 直接 fatal panic。
func TestWorkerPoolLifecycleIsPerRun(t *testing.T) {
	useQueueParams(t, wsQueueParams{writeEntries: 4, readEntries: 4})
	prev := withWorkerPool(t, nil)
	t.Cleanup(func() {
		workerPool.Store(prev)
	})

	initWorkerPool(2, false)
	first := workerPool.Load()
	if first == nil {
		t.Fatal("✘ 首轮未建立 Worker Pool")
	}
	if len(first.queues) != 2 {
		t.Fatalf("✘ Worker 数量错误: %d", len(first.queues))
	}

	mustReturn(t, 3*time.Second, "stopWorkerPool 挂死", stopWorkerPool)
	if workerPool.Load() != nil {
		t.Fatal("✘ 停止后仍留有池实例")
	}
	// 二次停止必须安静返回，而不是 close 已关闭通道
	mustReturn(t, 3*time.Second, "重复 stopWorkerPool", stopWorkerPool)

	initWorkerPool(3, true)
	second := workerPool.Load()
	if second == first {
		t.Fatal("✘ 第二轮复用了上一轮的池实例")
	}
	if second.size != 3 || !second.shardByKey {
		t.Fatalf("✘ 新一轮参数未生效: size=%d shard=%v", second.size, second.shardByKey)
	}
	mustReturn(t, 3*time.Second, "stopWorkerPool(第二轮)", stopWorkerPool)
}

// TestWorkerFullDropsWithinBudget 回归（S44 整改）：Worker 队列满时只能在有界
// 预算内重试目标队列，预算耗尽即丢弃计数。
//
// 旧实现的兜底是「回投 msgQueue」，而 msgQueue 唯一的消费者就是调用 dispatch 的
// 当前协程——回投等于下一轮立刻取出同一条、再撞满、再回投（零退避忙等），
// 还会让这条消息排到同 UID 后续消息之后，破坏 shard_by_key 的保序契约。
func TestWorkerFullDropsWithinBudget(t *testing.T) {
	prevQueue := msgQueue
	msgQueue = make(chan *msgQueueType, 4)
	t.Cleanup(func() { msgQueue = prevQueue })

	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType)}, // 无缓冲：必定满
		stats:      []*workerStats{{WorkerID: 0}},
		stop:       make(chan struct{}),
		size:       1,
		shardByKey: true,
	}
	withWorkerPool(t, pool)

	msg := &msgQueueType{uid: 42, data: []byte("payload")}
	start := time.Now()
	mustReturn(t, time.Second, "Worker 队列满时 dispatch 挂死", func() {
		pool.dispatch(msg)
	})
	if elapsed := time.Since(start); elapsed > dispatchRetryBudget*4 {
		t.Fatalf("✘ 重试没有硬预算，耗时 %v 超过预算 %v 的 4 倍", elapsed, dispatchRetryBudget)
	}

	if n := pool.fullCount.Load(); n != 1 {
		t.Fatalf("✘ 队列满未计入丢弃: %d", n)
	}
	if len(msgQueue) != 0 {
		t.Fatalf("✘ 队列满仍回投了接收队列，破坏保序: len=%d", len(msgQueue))
	}
	if len(pool.queues[0]) != 0 {
		t.Fatalf("✘ 消息被投进了已满的 Worker 队列: len=%d", len(pool.queues[0]))
	}
	if e := pool.stats[0].Errors.Load(); e != 1 {
		t.Fatalf("✘ Worker 错误计数未增加: %d", e)
	}
}

// TestShardModeNeverSpillsToOtherWorker 回归（S44 整改）：保序模式下目标 Worker 满
// 时绝不顺延到别的 Worker，否则同一 UID 的消息会被两个协程并发处理。
func TestShardModeNeverSpillsToOtherWorker(t *testing.T) {
	pool := &workerPoolState{
		queues: []chan *msgQueueType{
			make(chan *msgQueueType),    // 目标：无缓冲，必定满
			make(chan *msgQueueType, 8), // 邻居：空着也不许用
		},
		stats:      []*workerStats{{WorkerID: 0}, {WorkerID: 1}},
		stop:       make(chan struct{}),
		size:       2,
		shardByKey: true,
	}
	withWorkerPool(t, pool)

	if pool.tryDispatch(0, &msgQueueType{uid: 0}, pool.shardByKey) {
		t.Fatal("✘ 保序模式下消息顺延到了其它 Worker")
	}
	if len(pool.queues[1]) != 0 {
		t.Fatalf("✘ 邻居队列被写入: len=%d", len(pool.queues[1]))
	}
}

// TestNonShardModeSpillsToFreeWorker 回归（S44 整改）：无保序要求时目标满应顺延到
// 空闲 Worker 换取吞吐，而不是直接丢消息。
func TestNonShardModeSpillsToFreeWorker(t *testing.T) {
	pool := &workerPoolState{
		queues: []chan *msgQueueType{
			make(chan *msgQueueType),    // 目标：必定满
			make(chan *msgQueueType, 8), // 邻居：可用
		},
		stats:      []*workerStats{{WorkerID: 0}, {WorkerID: 1}},
		stop:       make(chan struct{}),
		size:       2,
		shardByKey: false,
	}
	withWorkerPool(t, pool)

	msg := &msgQueueType{uid: 1, data: []byte("x")}
	if !pool.tryDispatch(0, msg, pool.shardByKey) {
		t.Fatal("✘ 非保序模式下未顺延到空闲 Worker")
	}
	if got := <-pool.queues[1]; got != msg {
		t.Fatal("✘ 顺延后写入的不是原消息")
	}
}

// TestWaitQueueSlotExitsOnStop 回归（S44 整改）：停机信号必须立刻终结重试，
// 否则 shutdownWebsocket 会带着最多 size×预算 的延迟收尾。
func TestWaitQueueSlotExitsOnStop(t *testing.T) {
	pool := &workerPoolState{
		queues:     []chan *msgQueueType{make(chan *msgQueueType)},
		stats:      []*workerStats{{WorkerID: 0}},
		stop:       make(chan struct{}),
		size:       1,
		shardByKey: true,
	}
	withWorkerPool(t, pool)
	close(pool.stop)

	start := time.Now()
	if pool.waitQueueSlot(0, &msgQueueType{uid: 7}) {
		t.Fatal("✘ 池已停止却报告投递成功")
	}
	if elapsed := time.Since(start); elapsed > dispatchRetryBudget {
		t.Fatalf("✘ 停止信号未立刻终结重试: %v", elapsed)
	}
}

// TestDispatchDoesNotSharePoolAcrossRounds 回归（S44）：停止后的旧池不能被新池
// 的计数与通道污染——每轮统计独立起算。
func TestDispatchCountersArePerPool(t *testing.T) {
	prevQueue := msgQueue
	msgQueue = make(chan *msgQueueType, 4)
	t.Cleanup(func() { msgQueue = prevQueue })

	old := &workerPoolState{
		queues: []chan *msgQueueType{make(chan *msgQueueType)},
		stats:  []*workerStats{{WorkerID: 0}},
		stop:   make(chan struct{}),
		size:   1,
	}
	fresh := &workerPoolState{
		queues: []chan *msgQueueType{make(chan *msgQueueType, 4)},
		stats:  []*workerStats{{WorkerID: 0}},
		stop:   make(chan struct{}),
		size:   1,
	}
	old.fullCount.Add(7)
	withWorkerPool(t, old)
	workerPool.Store(fresh)

	mustReturn(t, time.Second, "dispatch 阻塞", func() {
		fresh.dispatch(&msgQueueType{uid: 3, data: []byte("x")})
	})

	if n := fresh.fullCount.Load(); n != 0 {
		t.Fatalf("✘ 新池沿用了上一轮的兜底计数: %d", n)
	}
	if n := old.fullCount.Load(); n != 7 {
		t.Fatalf("✘ 旧池计数被改写: %d", n)
	}
}
