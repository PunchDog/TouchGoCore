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
	clientcall.Store(className, &sync.Pool{
		New: func() any {
			// 使用反射创建新的ICall实例
			typ := reflect.TypeOf(factoryFunc)
			if typ.Kind() == reflect.Ptr {
				typ = typ.Elem()
			}
			newCall := reflect.New(typ).Interface()
			return newCall
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
			return
		case read_msg := <-msgQueue:
			// 处理消息队列
			if client, h := clientMap.Load(read_msg.uid); h {
				// 检查客户端是否已关闭，防止竞态条件
				if client.IsClose() {
					continue
				}
				pbmsg := util.PasreFSMessage(read_msg.data)
				if pbmsg != nil {
					// ============ 改进：更新统计 ============
					if client != nil {
						client.UpdateStatsFromMessage(read_msg.data)
					}
					client.OnMessage(client, pbmsg)
				} else {
					// ============ 改进：记录解析错误 ============
					UpdateErrorStats()
					vars.Error("解析消息失败，客户端: %d", read_msg.uid)
				}
			} else {
				// ============ 改进：记录客户端未找到错误 ============
				UpdateErrorStats()
				vars.Error("客户端未找到: %d", read_msg.uid)
			}
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
