package websocket

import (
	"reflect"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/config"
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
	DEFAULT_WRITE_BUFFER_SIZE = 10240
	DEFAULT_READ_BUFFER_SIZE  = 102400
	// 背压阈值：当通道满于此比例时，记录警告日志
	BACKPRESSURE_THRESHOLD = 0.9
)

// ============ 认证函数注册 ============

// AuthFunc WebSocket连接认证函数
// 返回 true 表示认证通过，false 表示拒绝连接
// token: 从请求中提取的认证令牌
// remoteAddr: 客户端IP地址
type AuthFunc func(token string, remoteAddr string) bool

var wsAuthFunc AuthFunc = nil

// SetAuthFunc 注册WebSocket连接认证函数
// 业务层应在启动前调用此函数注册自定义认证逻辑
func SetAuthFunc(fn AuthFunc) {
	wsAuthFunc = fn
}

// GetAuthFunc 获取当前注册的认证函数
func GetAuthFunc() AuthFunc {
	return wsAuthFunc
}

var (
	closeCh              chan bool          = nil
	msgQueue             chan *msgQueueType = nil
	clientpool           *sync.Pool         = nil
	clientcall           *syncmap.Map[string, *sync.Pool]
	writeBufferSize      int                  = DEFAULT_WRITE_BUFFER_SIZE
	readBufferSize       int                  = DEFAULT_READ_BUFFER_SIZE
	enableBackpressure   bool                 = false
	dropMessageOnFull    bool                 = false
	workerPoolEnabled    bool                 = false // 是否启用 Worker Pool
	workerPoolSize       int                  = 0     // Worker 数量
	shardByKey           bool                 = false // 是否按UID分片
	workerPoolQueues     []chan *msgQueueType         // Worker 消息队列
	workerPoolStop       chan struct{}                // Worker Pool 停止信号
	workerPoolWaitGroup  sync.WaitGroup               // Worker 等待组
	workerPoolStats      []*workerStats               // Worker 统计信息
	workerPoolStatsMutex sync.Mutex                   // 统计信息保护锁
)

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

func Run() {
	if config.Cfg_.Ws == nil {
		vars.Info("未启动websocket")
		return
	}

	clientMap = syncmap.NewMap[int64, *Client]()

	// 从配置中读取背压设置
	enableBackpressure = true // 启用背压控制
	dropMessageOnFull = false // 通道满时是否丢弃消息

	writeBufferSize = DEFAULT_WRITE_BUFFER_SIZE
	readBufferSize = DEFAULT_READ_BUFFER_SIZE

	closeCh = make(chan bool)
	msgQueue = make(chan *msgQueueType, readBufferSize)
	clientpool = &sync.Pool{
		New: func() interface{} {
			return &Client{
				ICall: nil,
			}
		},
	}

	// 读取 Worker Pool 配置
	workerPoolSize = config.Cfg_.Ws.WorkerPoolSize
	if workerPoolSize > 0 {
		workerPoolEnabled = true
		shardByKey = config.Cfg_.Ws.ShardByKey
		if shardByKey {
			vars.Info("WebSocket Worker Pool 启用: %d workers, 按UID分片", workerPoolSize)
		} else {
			vars.Info("WebSocket Worker Pool 启用: %d workers, 非分片模式", workerPoolSize)
		}
		initWorkerPool()
	} else {
		vars.Info("WebSocket 串行处理模式")
	}

	//启动监听
	for _, port := range config.Cfg_.Ws.Port {
		err := ListenAndServe(port.Port, port.CallbackClassName)
		if err != nil {
			vars.Error("websocket服务启动端口%d监听失败:%v", port.Port, err.Error())
			continue
		}
	}

	go Tick()
	vars.Info("websocket服务启动")
}

func Stop() {
	if config.Cfg_.Ws == nil {
		return
	}

	close(closeCh)
}

func Tick() {
	for {
		select {
		case <-closeCh:
			//关闭所有服务器
			for _, server := range serverList {
				server.Close()
			}
			//关闭所有客户端
			clientMap.Range(func(key int64, client *Client) bool {
				client.Close("")
				return true
			})

			//关闭消息队列
			close(msgQueue)

			//停止 Worker Pool
			if workerPoolEnabled {
				stopWorkerPool()
			}
			return
		case read_msg := <-msgQueue:
			// Worker Pool 模式：按UID分片分发到不同Worker
			if workerPoolEnabled {
				dispatchToWorker(read_msg)
				continue
			}

			// 串行模式：直接处理
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
		pbmsg := util.PasreFSMessage(read_msg.data)
		if pbmsg != nil {
			if client != nil {
				client.UpdateStatsFromMessage(read_msg.data)
			}
			client.OnMessage(client, pbmsg)
		} else {
			UpdateErrorStats()
			vars.Error("解析消息失败，客户端: %d", read_msg.uid)
		}
	} else {
		UpdateErrorStats()
		vars.Error("客户端未找到: %d", read_msg.uid)
	}
}

// initWorkerPool 初始化 Worker Pool
func initWorkerPool() {
	workerPoolQueues = make([]chan *msgQueueType, workerPoolSize)
	workerPoolStats = make([]*workerStats, workerPoolSize)
	workerPoolStop = make(chan struct{})

	for i := 0; i < workerPoolSize; i++ {
		workerPoolQueues[i] = make(chan *msgQueueType, 102400)
		workerPoolStats[i] = &workerStats{
			WorkerID: i,
		}
		workerPoolStats[i].Running.Store(true)

		workerPoolWaitGroup.Add(1)
		go workerLoop(i, workerPoolQueues[i])
	}
}

// stopWorkerPool 停止 Worker Pool
func stopWorkerPool() {
	close(workerPoolStop)
	workerPoolWaitGroup.Wait()
	vars.Info("WebSocket Worker Pool 已停止")
}

// dispatchToWorker 将消息分发到对应的Worker
func dispatchToWorker(msg *msgQueueType) {
	var workerIdx int
	if shardByKey {
		// 按UID分片：保证同一UID的消息由同一Worker处理，保证顺序性
		workerIdx = int(msg.uid % int64(workerPoolSize))
	} else {
		// 轮询模式：均匀分配
		workerIdx = int(serverStats.totalMessages.Load() % int64(workerPoolSize))
	}

	UpdateMessageStats()

	select {
	case workerPoolQueues[workerIdx] <- msg:
		// 发送成功
	default:
		// Worker队列满，回退到当前goroutine处理
		workerPoolStats[workerIdx].Errors.Add(1)
		vars.Warning("Worker[%d]队列满，回退到同步处理", workerIdx)
		processMessage(msg)
	}
}

// workerLoop Worker处理循环
func workerLoop(workerID int, queue chan *msgQueueType) {
	defer workerPoolWaitGroup.Done()

	for {
		select {
		case <-workerPoolStop:
			// 处理剩余消息
			for len(queue) > 0 {
				msg := <-queue
				processMessage(msg)
				workerPoolStats[workerID].Messages.Add(1)
			}
			workerPoolStats[workerID].Running.Store(false)
			return

		case msg := <-queue:
			processMessage(msg)
			workerPoolStats[workerID].Messages.Add(1)
			workerPoolStats[workerID].LastMessageAt = util.CurrentTime()
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
	} else {
		serverStats.currentConnections.Add(-1)
	}
}

// UpdateMessageStats 更新消息统计
func UpdateMessageStats() {
	serverStats.totalMessages.Add(1)
}

// UpdateErrorStats 更新错误统计
func UpdateErrorStats() {
	serverStats.totalErrors.Add(1)
}
