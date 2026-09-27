package websocket

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/metrics"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
)

// ============ 改进部分 ============

// 使用原子操作改进的全局变量管理
var (
	serverStats struct {
		totalConnections   atomic.Int64
		currentConnections atomic.Int64
		totalMessages      atomic.Int64
		totalErrors        atomic.Int64
	}
)

// ============ 原有代码 ============

const (
	// 下面两个常量是队列容量，单位是「条数」而不是字节。
	//
	// 修复前它们叫 DEFAULT_WRITE_BUFFER_SIZE / DEFAULT_READ_BUFFER_SIZE，
	// 值是 1MiB，还被直接当成 chan 容量用：每条连接的发送队列一上来就预留
	// 100 万个槽（每槽一个 24B 切片头 ≈ 24MB），几百条连接就能把进程吃穿。
	defaultSendQueueEntries = 1024
	defaultRecvQueueEntries = 1024
	// 背压阈值：当通道满于此比例时，记录警告日志
	BACKPRESSURE_THRESHOLD = 0.9
	// Worker 队列满时的兜底重试预算：只在目标队列上有界等待，超时即丢弃并计数。
	// 预算太小会在突发下丢消息，太大则把唯一消费协程按住太久（全服停摆），故取毫秒级。
	dispatchRetryBudget   = 20 * time.Millisecond
	dispatchRetryInterval = 500 * time.Microsecond
)

// 心跳与超时的默认值（毫秒），可被 ws 配置覆盖
const (
	defaultPingIntervalMS = 30 * 1000
	defaultReadTimeoutMS  = 90 * 1000
	defaultWriteTimeoutMS = 5 * 1000
)

// ============ 认证函数注册 ============

// AuthFunc WebSocket连接认证函数
// 返回 true 表示认证通过，false 表示拒绝连接
// token: 从请求中提取的认证令牌
// remoteAddr: 客户端IP地址
type AuthFunc func(token string, remoteAddr string) bool

var (
	authMu     sync.RWMutex
	wsAuthFunc AuthFunc
)

func SetAuthFunc(fn AuthFunc) {
	authMu.Lock()
	wsAuthFunc = fn
	authMu.Unlock()
}

func GetAuthFunc() AuthFunc {
	authMu.RLock()
	defer authMu.RUnlock()
	return wsAuthFunc
}

// wsQueueParams 是一次 Run 装配的队列/背压参数快照。
//
// 四个参数原先是包级普通变量：Run 写入时上一轮连接的写协程还在读，
// 读写同一 int/bool 就是数据竞争；更要紧的是新旧两轮参数交错，
// 一条老连接会按下一轮的容量/背压策略开队列。收成一个不可变快照整体替换。
type wsQueueParams struct {
	writeEntries int
	readEntries  int
	backpressure bool
	dropOnFull   bool
}

var wsQueue atomic.Pointer[wsQueueParams]

// queueParams 返回当前生效的队列参数（Run 之前为默认值）。
func queueParams() wsQueueParams {
	if p := wsQueue.Load(); p != nil {
		return *p
	}
	return wsQueueParams{
		writeEntries: defaultSendQueueEntries,
		readEntries:  defaultRecvQueueEntries,
	}
}

// runState 是一次 Run 代际的全部可变状态（M4 修复）。
//
// 修复前 closeCh / msgQueue / tickDone / stopOnce 都是包级变量：
//   - 二次 Run 直接覆盖 closeCh/msgQueue，但旧 Tick 协程仍在 select 这两个名字，
//     结果是「旧 Tick + 新 Tick 同时消费新 msgQueue」，新旧连接消息跨代混投；
//   - 旧 Tick 永远收不到旧 closeCh 的关闭信号（那个 chan 已被新值替换且无人引用），
//     协程泄漏直到进程退出；
//   - stopOnce 是 sync.Once 值，Run 重置后旧 Stop 调用可能命中新 Once，
//     把上一代的关闭动作错算到这一代头上。
//
// 收进结构体后每代 Run 持有独立实例：Tick 协程与 Client 都通过自己捕获的指针访问，
// 上一代资源天然与上一代协程绑定，新一代不会观察到旧状态，旧 Tick 也必定退出。
type runState struct {
	// generation 单调递增的代号，仅用于日志/测试观测
	generation uint64
	// ctx 本代 Run 的生命周期上下文，Tick 用它的 Done() 退出
	ctx context.Context
	// closeCh 显式停机信号；Stop 与下一轮 Run 切换前都会 close 它
	closeCh chan struct{}
	// msgQueue 本代的消息队列；Client.readLoop 投递、Tick 消费
	msgQueue chan *msgQueueType
	// tickDone Tick 协程退出后关闭，供 Stop / 下一轮 Run 等待
	tickDone chan struct{}
	// stopOnce 保证 closeCh 只 close 一次（重复 close 会 panic）
	stopOnce sync.Once
}

// currentRunState 持有当前代际的状态；未 Run 或已 Stop 后为 nil。
// 用 atomic.Pointer 而不是普通变量：Run 写入与 Client.readLoop 的读取天然并发。
var currentRunState atomic.Pointer[runState]

// runGenerationCounter 给每代 Run 分配单调递增的代号。
var runGenerationCounter atomic.Uint64

var (
	clientpool *sync.Pool = nil
	clientcall *syncmap.Map[string, *sync.Pool]

	// runCtxValue 是本轮 Run 的生命周期上下文。Run 写入时上一轮的连接协程
	// 可能还在 DialContext/Done 上读它，普通接口变量并发读写即数据竞争。
	// 用 atomic.Pointer 而不是 atomic.Value：后者混存不同具体类型会 panic。
	runCtxValue atomic.Pointer[context.Context]

	// pingInterval / readTimeout / writeTimeout 是心跳与超时参数，按 Run 生效。
	// 必须是原子量：Run 写入时上一轮的读写协程可能仍在热循环里读它们，
	// 普通 time.Duration 变量在并发读写下是数据竞争（不是「读到旧值」而是 UB）。
	pingInterval atomic.Int64 // 纳秒
	readTimeout  atomic.Int64
	writeTimeout atomic.Int64

	// workerPool 非 nil 表示并行消费模式；每次 Run 新建，见 workerPoolState 注释
	workerPool atomic.Pointer[workerPoolState]
)

func init() {
	pingInterval.Store(defaultPingIntervalMS * int64(time.Millisecond))
	readTimeout.Store(defaultReadTimeoutMS * int64(time.Millisecond))
	writeTimeout.Store(defaultWriteTimeoutMS * int64(time.Millisecond))
}

// currentPingInterval 等取值器：连接协程热循环读取，必须与 Run 的写入原子一致
func currentPingInterval() time.Duration { return time.Duration(pingInterval.Load()) }
func currentReadTimeout() time.Duration  { return time.Duration(readTimeout.Load()) }
func currentWriteTimeout() time.Duration { return time.Duration(writeTimeout.Load()) }

// wsRunCtx 返回当前生命周期上下文；未 Run 过时退化为 Background（永不 Done）。
func wsRunCtx() context.Context {
	if p := runCtxValue.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// setWsRunCtx 装配本轮生命周期上下文，供 Run 使用。
func setWsRunCtx(ctx context.Context) { runCtxValue.Store(&ctx) }

// loadRunState 返回当前代际的状态快照（可能为 nil）。
// 调用方应一次取值后存进局部变量再用：两次调用之间宿主可能已经换代。
func loadRunState() *runState { return currentRunState.Load() }

// currentMsgQueue 返回当前代际的消息队列；未 Run 时为 nil。
// 仅供未绑定代际的旁路（如裸构造的测试 Client）回退使用。
func currentMsgQueue() chan *msgQueueType {
	if s := loadRunState(); s != nil {
		return s.msgQueue
	}
	return nil
}

// newRunState 为本次 Run 装配独立的代际状态。
func newRunState(ctx context.Context, readEntries int) *runState {
	if ctx == nil {
		ctx = context.Background()
	}
	if readEntries <= 0 {
		readEntries = defaultRecvQueueEntries
	}
	return &runState{
		generation: runGenerationCounter.Add(1),
		ctx:        ctx,
		closeCh:    make(chan struct{}),
		msgQueue:   make(chan *msgQueueType, readEntries),
		tickDone:   make(chan struct{}),
	}
}

// stopRunState 关闭指定代际并等待其 Tick 协程退出（M4 修复的关键路径）。
//
// 必须在 Run 切换前调用：Tick 退出路径会触发 shutdownWebsocket，后者摘走全局
// serverList 并关闭客户端表里的全部连接；若放任旧 Tick 与新一轮 Run 并发，
// 它会顺手关掉新一代刚注册的 HTTP 服务器与新接受的客户端。
//
// 幂等：stopOnce 保证重复调用只 close 一次；tickDone 已关时立即返回。
func stopRunState(s *runState, waitCtx context.Context) {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.closeCh) })
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	select {
	case <-s.tickDone:
	case <-waitCtx.Done():
		vars.Error("WebSocket Tick(gen=%d) 停止超时: %v", s.generation, waitCtx.Err())
	}
}

// workerPoolState 是一次 Run → Stop 生命周期内的并发消费端。
//
// 收进结构体而不是摊成六个包级全局：workerPoolStop 与 workerPoolWaitGroup 跨
// Run 复用会出两类事故——Stop 只 close 一次通道，第二轮 Run 的 Worker 收到
// 上一轮的关闭信号立刻退出（消息再没人处理）；重复 close 则直接 fatal panic。
// WaitGroup 同理，上一轮残留的计数会让本轮 Wait 提前返回。
type workerPoolState struct {
	queues     []chan *msgQueueType
	stats      []*workerStats
	stop       chan struct{}
	wg         sync.WaitGroup
	size       int
	shardByKey bool
	// fullCount：Worker 队列满、被迫走兜底路径的次数
	fullCount atomic.Int64
	// dispatchSeq：轮询模式自己的派发序号。
	// 不能借用 serverStats.totalMessages——那个计数只在消息真正被处理时递增，
	// 队列持续满、解析连续失败时它不涨，所有消息会一直粘在同一个 Worker 上。
	dispatchSeq atomic.Uint64
	// lastFullWarn：满队列告警的限频时间戳（UnixMilli）
	lastFullWarn atomic.Int64
}

// WorkerPoolFullCount 返回 Worker 队列满的兜底次数（串行模式恒为 0）
func WorkerPoolFullCount() int64 {
	if p := workerPool.Load(); p != nil {
		return p.fullCount.Load()
	}
	return 0
}

// workerStats 用于收集 Worker 的统计信息
type workerStats struct {
	WorkerID      int
	Messages      atomic.Int64 // 处理的消息数量
	Errors        atomic.Int64 // 错误数量
	LastMessageAt time.Time
	Running       atomic.Bool
}

type msgQueueType struct {
	uid  int64
	data []byte
}

type defaultCall struct {
}

func (this *defaultCall) OnConnect(client *Client) bool {
	vars.Info("defaultCall OnConnect")
	return true
}

func (this *defaultCall) OnMessage(client *Client, msg proto.Message) {
	vars.Info("defaultCall OnMessage")
}

func (this *defaultCall) OnClose(client *Client) {
	vars.Info("defaultCall OnClose")
}

func RegisterCall(className string, factoryFunc any) {
	// 优化：在注册时一次性解析类型，避免 Pool.New 每次调用 reflect.TypeOf
	typ := reflect.TypeOf(factoryFunc)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if clientcall == nil {
		clientcall = new(syncmap.Map[string, *sync.Pool])
	}
	clientcall.Store(className, &sync.Pool{
		New: func() any {
			return reflect.New(typ).Interface()
		},
	})
}

// UseClientMap 绑定 WebSocket 客户端表（App 优先，全局 fallback）。
//
// 单锁表在几十个读协程同时按 uid 查客户端时会把锁排队算进消息延迟；
// 想换成分片表用 UseShardedClientMap，两者对本包等价（见 clientTable）。
func UseClientMap(m *syncmap.Map[int64, *Client]) {
	if m != nil {
		storeClientMap(m)
	}
}

// UseShardedClientMap 绑定分片客户端表（S69）。
// 分片表的 Range 是逐片快照，不保证跨片的全局一致顺序；本包只在关服时用它
// 逐个关连接，因此这一差异不可观察。
func UseShardedClientMap(m *syncmap.ShardedMap[int64, *Client]) {
	if m != nil {
		storeClientMap(m)
	}
}

// GetClient 按 UID 获取已连接客户端；不存在返回 nil。
func GetClient(uid int64) *Client {
	table := loadClientMap()
	if table == nil {
		return nil
	}
	c, ok := table.Load(uid)
	if !ok {
		return nil
	}
	return c
}

func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// 无效配置时不改变任何状态（保持旧语义）：先校验再停止上一代。
	setWsRunCtx(ctx)
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.Ws == nil {
		vars.Info("未启动websocket")
		return nil
	}

	// M4 修复：先把上一代彻底收掉再装配新一代。
	// 不等的话，旧 Tick 会在新 serverList 注册后触发 shutdownWebsocket，
	// 把新一代的 HTTP 监听器和客户端一并关掉。
	stopRunState(loadRunState(), ctx)

	if loadClientMap() == nil {
		// 不经 App 直接跑本模块时的兜底表：用分片实现，因为这张表每次消息派发都要查一遍，
		// 单锁版在并发连接下会把锁排队算进消息延迟（S69 实测并行读快 2.4~2.8 倍）。
		storeClientMap(syncmap.NewShardedMap[int64, *Client](0))
	}

	params := wsQueueParams{
		writeEntries: cfg.WriteQueueCapacity(defaultSendQueueEntries),
		readEntries:  cfg.QueueCapacity(defaultRecvQueueEntries),
		// 走到这里说明本轮 Run 已决定启用 WebSocket，背压开关随之生效
		backpressure: true,
		dropOnFull:   cfg.DropOnFull(),
	}
	wsQueue.Store(&params)
	applyTimeoutConfig(cfg.Ws)

	// M4 修复：本代的 closeCh/msgQueue/tickDone 全部收进 state，Tick 与 Client
	// 都通过指针访问，不再跨代混用。
	state := newRunState(ctx, params.readEntries)
	currentRunState.Store(state)

	clientpool = &sync.Pool{
		New: func() interface{} {
			return &Client{
				ICall: nil,
			}
		},
	}

	if size := cfg.Ws.WorkerPoolSize; size > 0 {
		initWorkerPool(size, cfg.Ws.ShardByKey)
		if cfg.Ws.ShardByKey {
			vars.Info("WebSocket Worker Pool 启用: %d workers, 按UID分片", size)
		} else {
			vars.Info("WebSocket Worker Pool 启用: %d workers, 非分片模式", size)
		}
	} else {
		vars.Info("WebSocket 串行处理模式")
	}

	var lastErr error
	started := 0
	for _, port := range cfg.Ws.Port {
		err := ListenAndServe(port.Port, port.CallbackClassName)
		if err != nil {
			vars.Error("websocket服务启动端口%d监听失败:%v", port.Port, err.Error())
			lastErr = err
			continue
		}
		started++
	}
	if started == 0 && lastErr != nil {
		// 启动全部失败：收回刚装的状态，避免后续 Stop 等不到 tickDone
		currentRunState.CompareAndSwap(state, nil)
		return lastErr
	}

	go tickLoop(state)
	vars.Info("websocket服务启动 gen=%d", state.generation)
	return nil
}

func Stop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.Ws == nil {
		return
	}

	// M4 修复：只针对当前代际。上一代的状态在 Run 切换时已被 stopRunState 收掉，
	// 这里不需再递归历史。
	state := loadRunState()
	if state == nil {
		return
	}
	stopRunState(state, ctx)
	currentRunState.CompareAndSwap(state, nil)
}

// applyTimeoutConfig 读取心跳与超时配置，未配置项沿用默认值。
func applyTimeoutConfig(ws *config.WebsocketConfig) {
	pingMS, readMS, writeMS := defaultPingIntervalMS, defaultReadTimeoutMS, defaultWriteTimeoutMS
	if ws != nil {
		if ws.PingIntervalMS > 0 {
			pingMS = ws.PingIntervalMS
		}
		if ws.ReadTimeoutMS > 0 {
			readMS = ws.ReadTimeoutMS
		}
		if ws.WriteTimeoutMS > 0 {
			writeMS = ws.WriteTimeoutMS
		}
	}
	// 读超时必须显著大于 ping 间隔：否则服务端还没发出探测帧，
	// 读协程就先把自己超时断连，表现为「空闲连接被随机踢掉」。
	if readMS <= pingMS*2 {
		vars.Warning("WebSocket read_timeout_ms(%d) 未显著大于 2×ping_interval_ms(%d)，空闲连接可能被误踢",
			readMS, pingMS)
	}
	pingInterval.Store(int64(time.Duration(pingMS) * time.Millisecond))
	readTimeout.Store(int64(time.Duration(readMS) * time.Millisecond))
	writeTimeout.Store(int64(time.Duration(writeMS) * time.Millisecond))
}

func shutdownWebsocket() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	for _, server := range takeServers() {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}
	cancel()
	if table := loadClientMap(); table != nil {
		table.Range(func(key int64, client *Client) bool {
			client.Close("")
			return true
		})
	}
	// msgQueue 不做 close：Tick 靠 closeCh/ctx 退出，读协程仍在往里投递，
	// 关闭会让 send on closed channel 直接 panic。
	stopWorkerPool()
}

// Tick 启动当前代际的消息消费循环（对外 API 兼容入口）。
//
// 调用瞬间快照 currentRunState 并把它绑死给协程：这样后续 Run 切换包级指针
// 也不会让本协程读到下一代的 msgQueue/closeCh（M4 修复的核心约束）。
func Tick() { tickLoop(loadRunState()) }

// tickLoop 是本代际的实际消费循环。state 由调用方显式传入，全程不再
// 回读包级指针，以免与下一代 Run 交错。
func tickLoop(state *runState) {
	if state == nil {
		return
	}
	defer func() {
		select {
		case <-state.tickDone:
		default:
			close(state.tickDone)
		}
	}()
	for {
		select {
		case <-state.closeCh:
			shutdownWebsocket()
			return
		case <-state.ctx.Done():
			shutdownWebsocket()
			return
		case read_msg := <-state.msgQueue:
			if pool := workerPool.Load(); pool != nil {
				pool.dispatch(read_msg)
				continue
			}
			processMessage(read_msg)
		}
	}
}

// processMessage 处理单条消息，并把业务回调的 panic 收敛在这里。
//
// 串行模式下本函数跑在 Tick 唯一的消费协程上，Worker Pool 模式下跑在常驻 Worker 上：
// 两种情况 panic 逃逸都会永久吃掉一个消费者，表现为「跑一段时间后消息全部堆积」，
// 所以绝不能让业务回调的 panic 冒到调用方。返回 true 表示本次处理崩过。
func processMessage(read_msg *msgQueueType) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			UpdateErrorStats()
			metrics.WS.IncErrors("panic")
			vars.Error("WebSocket 业务回调 panic, uid=%d: %v", read_msg.uid, r)
		}
	}()

	table := loadClientMap()
	if table == nil {
		UpdateErrorStats()
		metrics.WS.IncErrors("no_table")
		vars.Error("客户端表未注入，消息无法派发: uid=%d", read_msg.uid)
		return false
	}
	client, h := table.Load(read_msg.uid)
	if !h {
		UpdateErrorStats()
		metrics.WS.IncErrors("not_found")
		vars.Error("客户端未找到: %d", read_msg.uid)
		return false
	}
	// 检查客户端是否已关闭，防止竞态条件
	if client.IsClose() {
		return false
	}
	pbmsg := util.ParseFSMessage(read_msg.data)
	if pbmsg == nil {
		UpdateErrorStats()
		metrics.WS.IncErrors("parse")
		vars.Error("解析消息失败，客户端: %d", read_msg.uid)
		return false
	}
	client.UpdateStatsFromMessage(read_msg.data)
	client.OnMessage(client, pbmsg)
	return false
}

// initWorkerPool 为本次 Run 新建 Worker Pool。
//
// 并发语义（明确契约）：
//   - shardByKey=true：同一 UID 的消息固定落在同一 Worker，UID 内保序；
//   - shardByKey=false：轮询派发，跨消息不保证任何顺序；
//   - 无论哪种，业务 OnMessage 都会被多个 Worker 协程并发调用，
//     回调实现必须自己保证对共享状态的并发安全。
func initWorkerPool(size int, shard bool) {
	if size <= 0 {
		return
	}
	pool := &workerPoolState{
		queues:     make([]chan *msgQueueType, size),
		stats:      make([]*workerStats, size),
		stop:       make(chan struct{}),
		size:       size,
		shardByKey: shard,
	}
	entries := queueParams().readEntries
	for i := 0; i < size; i++ {
		pool.queues[i] = make(chan *msgQueueType, entries)
		pool.stats[i] = &workerStats{WorkerID: i}
		pool.stats[i].Running.Store(true)
		pool.wg.Add(1)
		go pool.workerLoop(i)
	}
	workerPool.Store(pool)
}

// stopWorkerPool 停止本轮 Worker Pool 并取回实例；未启用时直接返回。
func stopWorkerPool() {
	pool := workerPool.Swap(nil)
	if pool == nil {
		return
	}
	close(pool.stop)
	pool.wg.Wait()
	vars.Info("WebSocket Worker Pool 已停止")
}

// dispatch 把消息投递到对应 Worker 的队列。
//
// 本方法跑在 Tick 唯一的消费协程上：这里长时间阻塞 = 全服消息处理停摆，
// 因此绝不内联执行 processMessage，兜底重试也有硬预算。
func (pool *workerPoolState) dispatch(msg *msgQueueType) {
	var workerIdx int
	if pool.shardByKey {
		// 按UID分片：保证同一UID的消息由同一Worker处理，保证顺序性
		workerIdx = int(msg.uid % int64(pool.size))
	} else {
		// 轮询模式：均匀分配，序号由池自己推进，不依赖业务处理计数
		workerIdx = int(pool.dispatchSeq.Add(1) % uint64(pool.size))
	}

	if pool.tryDispatch(workerIdx, msg, pool.shardByKey) {
		return
	}

	// 目标 Worker 满：只在本队列上做有界重试，绝不回投 msgQueue。
	// 回投会让这条消息排到「同 UID 更新的消息」之后，直接破坏 shard_by_key 的保序契约；
	// 而 msgQueue 唯一的消费者就是当前协程，下一轮立刻取到同一条、再撞满、再回投 = 零退避忙等。
	if !pool.waitQueueSlot(workerIdx, msg) {
		pool.dropDispatched(workerIdx, msg)
	}
}

// tryDispatch 非阻塞投递；非分片模式下目标满时顺延试探其它 Worker（无保序要求，换取吞吐）。
func (pool *workerPoolState) tryDispatch(workerIdx int, msg *msgQueueType, keepOrder bool) bool {
	select {
	case pool.queues[workerIdx] <- msg:
		return true
	default:
	}
	if keepOrder {
		return false
	}
	for i := 1; i < pool.size; i++ {
		idx := (workerIdx + i) % pool.size
		select {
		case pool.queues[idx] <- msg:
			return true
		default:
		}
	}
	return false
}

// waitQueueSlot 在预算内等待目标队列腾出位置；退出条件：投递成功、池停止、预算耗尽。
func (pool *workerPoolState) waitQueueSlot(workerIdx int, msg *msgQueueType) bool {
	deadline := time.Now().Add(dispatchRetryBudget)
	for {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case pool.queues[workerIdx] <- msg:
			return true
		case <-pool.stop:
			// 停机路径不再兜底重投，交给自己丢弃计数，避免把消息投进已无人消费的队列
			return false
		case <-time.After(dispatchRetryInterval):
		}
	}
}

// dropDispatched 记录一次因队列满而发生的丢弃
func (pool *workerPoolState) dropDispatched(workerIdx int, msg *msgQueueType) {
	pool.fullCount.Add(1)
	pool.stats[workerIdx].Errors.Add(1)
	UpdateErrorStats()
	metrics.WS.IncErrors("queue_full")
	if pool.shouldWarnFull() {
		vars.Warning("Worker队列满，重试预算内仍未腾出位置，丢弃: 累计=%d uid=%d", pool.fullCount.Load(), msg.uid)
	}
}

// shouldWarnFull 对「队列满」告警按秒限频，避免高负载下日志本身成为瓶颈
func (pool *workerPoolState) shouldWarnFull() bool {
	now := util.CurrentMS()
	last := pool.lastFullWarn.Load()
	for now-last >= 1000 {
		if pool.lastFullWarn.CompareAndSwap(last, now) {
			return true
		}
		last = pool.lastFullWarn.Load()
	}
	return false
}

// workerLoop Worker处理循环
func (pool *workerPoolState) workerLoop(workerID int) {
	defer pool.wg.Done()

	queue := pool.queues[workerID]
	for {
		select {
		case <-pool.stop:
			// 处理剩余消息
			for len(queue) > 0 {
				msg := <-queue
				pool.safeProcess(workerID, msg)
			}
			pool.stats[workerID].Running.Store(false)
			return

		case msg := <-queue:
			pool.safeProcess(workerID, msg)
		}
	}
}

// safeProcess 执行一条消息的业务处理并维护该 Worker 的计数。
//
// panic 已在 processMessage 内收敛：这里拿返回值补错误计数，Worker 协程本身不受影响。
func (pool *workerPoolState) safeProcess(workerID int, msg *msgQueueType) {
	if processMessage(msg) {
		pool.stats[workerID].Errors.Add(1)
	}
	pool.stats[workerID].Messages.Add(1)
	pool.stats[workerID].LastMessageAt = util.CurrentTime()
}

// ============ 新增改进功能 ============

// GetServerStats 获取服务器统计信息
func GetServerStats() struct {
	TotalConnections   int64
	CurrentConnections int64
	TotalMessages      int64
	TotalErrors        int64
} {
	return struct {
		TotalConnections   int64
		CurrentConnections int64
		TotalMessages      int64
		TotalErrors        int64
	}{
		TotalConnections:   serverStats.totalConnections.Load(),
		CurrentConnections: serverStats.currentConnections.Load(),
		TotalMessages:      serverStats.totalMessages.Load(),
		TotalErrors:        serverStats.totalErrors.Load(),
	}
}

// UpdateConnectionStats 更新连接统计
func UpdateConnectionStats(connected bool) {
	if connected {
		serverStats.totalConnections.Add(1)
		serverStats.currentConnections.Add(1)
		metrics.WS.IncConnection()
	} else {
		serverStats.currentConnections.Add(-1)
		metrics.WS.DecConnection()
	}
}

// UpdateMessageStats 更新消息统计
func UpdateMessageStats() {
	serverStats.totalMessages.Add(1)
	metrics.WS.IncMessages("inbound")
}

// UpdateErrorStats 更新错误统计
func UpdateErrorStats() {
	serverStats.totalErrors.Add(1)
}
