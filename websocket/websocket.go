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

var (
	closeCh            chan bool          = nil
	msgQueue           chan *msgQueueType = nil
	wsRunCtx           context.Context    = context.Background()
	clientpool         *sync.Pool         = nil
	clientcall         *syncmap.Map[string, *sync.Pool]
	writeQueueEntries  int  = defaultSendQueueEntries
	readQueueEntries   int  = defaultRecvQueueEntries
	enableBackpressure bool = false
	dropMessageOnFull  bool = false
	stopOnce           sync.Once
	tickDone           chan struct{}

	// pingInterval / readTimeout / writeTimeout 是心跳与超时参数，按 Run 生效
	pingInterval = defaultPingIntervalMS * time.Millisecond
	readTimeout  = defaultReadTimeoutMS * time.Millisecond
	writeTimeout = defaultWriteTimeoutMS * time.Millisecond

	// workerPool 非 nil 表示并行消费模式；每次 Run 新建，见 workerPoolState 注释
	workerPool atomic.Pointer[workerPoolState]
)

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
func UseClientMap(m *syncmap.Map[int64, *Client]) {
	if m != nil {
		clientMap = m
	}
}

// GetClient 按 UID 获取已连接客户端；不存在返回 nil。
func GetClient(uid int64) *Client {
	if clientMap == nil {
		return nil
	}
	c, ok := clientMap.Load(uid)
	if !ok {
		return nil
	}
	return c
}

func Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	wsRunCtx = ctx
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.Ws == nil {
		vars.Info("未启动websocket")
		return nil
	}

	if clientMap == nil {
		clientMap = syncmap.NewMap[int64, *Client]()
	}

	enableBackpressure = true
	dropMessageOnFull = cfg.DropOnFull()

	writeQueueEntries = cfg.WriteQueueCapacity(defaultSendQueueEntries)
	readQueueEntries = cfg.QueueCapacity(defaultRecvQueueEntries)
	applyTimeoutConfig(cfg.Ws)

	closeCh = make(chan bool)
	tickDone = make(chan struct{})
	stopOnce = sync.Once{}
	msgQueue = make(chan *msgQueueType, readQueueEntries)
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
		return lastErr
	}

	go Tick()
	vars.Info("websocket服务启动")
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

	stopOnce.Do(func() {
		if closeCh != nil {
			close(closeCh)
		}
	})
	if tickDone != nil {
		select {
		case <-tickDone:
		case <-ctx.Done():
			vars.Error("WebSocket Tick 停止超时: %v", ctx.Err())
		}
	}
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
	pingInterval = time.Duration(pingMS) * time.Millisecond
	readTimeout = time.Duration(readMS) * time.Millisecond
	writeTimeout = time.Duration(writeMS) * time.Millisecond
}

func shutdownWebsocket() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	for _, server := range takeServers() {
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
	}
	cancel()
	if clientMap != nil {
		clientMap.Range(func(key int64, client *Client) bool {
			client.Close("")
			return true
		})
	}
	// msgQueue 不做 close：Tick 靠 closeCh/ctx 退出，读协程仍在往里投递，
	// 关闭会让 send on closed channel 直接 panic。
	stopWorkerPool()
}

func Tick() {
	defer func() {
		if tickDone != nil {
			select {
			case <-tickDone:
			default:
				close(tickDone)
			}
		}
	}()
	for {
		select {
		case <-closeCh:
			shutdownWebsocket()
			return
		case <-wsRunCtx.Done():
			shutdownWebsocket()
			return
		case read_msg := <-msgQueue:
			if pool := workerPool.Load(); pool != nil {
				pool.dispatch(read_msg)
				continue
			}
			processMessage(read_msg)
		}
	}
}

// processMessage 处理单条消息（从Tick中提取，便于Worker Pool复用）
func processMessage(read_msg *msgQueueType) {
	if client, h := clientMap.Load(read_msg.uid); h {
		// 检查客户端是否已关闭，防止竞态条件
		if client.IsClose() {
			return
		}
		pbmsg := util.ParseFSMessage(read_msg.data)
		if pbmsg != nil {
			if client != nil {
				client.UpdateStatsFromMessage(read_msg.data)
			}
			client.OnMessage(client, pbmsg)
		} else {
			UpdateErrorStats()
			metrics.WS.IncErrors("parse")
			vars.Error("解析消息失败，客户端: %d", read_msg.uid)
		}
	} else {
		UpdateErrorStats()
		metrics.WS.IncErrors("not_found")
		vars.Error("客户端未找到: %d", read_msg.uid)
	}
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
	for i := 0; i < size; i++ {
		pool.queues[i] = make(chan *msgQueueType, readQueueEntries)
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
// 本方法跑在 Tick 唯一的消费协程上：这里阻塞 = 全服消息处理停摆，
// 因此绝不内联执行 processMessage。
func (pool *workerPoolState) dispatch(msg *msgQueueType) {
	var workerIdx int
	if pool.shardByKey {
		// 按UID分片：保证同一UID的消息由同一Worker处理，保证顺序性
		workerIdx = int(msg.uid % int64(pool.size))
	} else {
		// 轮询模式：均匀分配
		workerIdx = int(serverStats.totalMessages.Load() % int64(pool.size))
	}

	UpdateMessageStats()

	select {
	case pool.queues[workerIdx] <- msg:
		return
	default:
	}

	// Worker 队列满：先回投 msgQueue 兜底，让 Tick 下一轮再试。
	// 修复前是在这里内联 processMessage——一个慢业务回调就把唯一的消费协程
	// 占住，所有连接的其它消息一起卡死，等于用「并行」换来了更差的串行。
	pool.fullCount.Add(1)
	pool.stats[workerIdx].Errors.Add(1)
	metrics.WS.IncErrors("queue_full")
	if pool.shouldWarnFull() {
		vars.Warning("Worker[%d]队列满，回投接收队列兜底: 累计=%d", workerIdx, pool.fullCount.Load())
	}
	select {
	case msgQueue <- msg:
		return
	default:
	}

	// 接收队列也满：只能丢弃并计数，绝不阻塞消费协程
	UpdateErrorStats()
	metrics.WS.IncErrors("drop")
	vars.Error("Worker 与接收队列同时满，丢弃消息: uid=%d", msg.uid)
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
				processMessage(msg)
				pool.stats[workerID].Messages.Add(1)
			}
			pool.stats[workerID].Running.Store(false)
			return

		case msg := <-queue:
			processMessage(msg)
			pool.stats[workerID].Messages.Add(1)
			pool.stats[workerID].LastMessageAt = util.CurrentTime()
		}
	}
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
