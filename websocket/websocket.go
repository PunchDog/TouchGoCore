package websocket

import (
	"reflect"
	"sync"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
)

const (
	DEFAULT_WRITE_BUFFER_SIZE = 10240
	DEFAULT_READ_BUFFER_SIZE  = 102400
	// 背压阈值：当通道满于此比例时，记录警告日志
	BACKPRESSURE_THRESHOLD = 0.9
)

var (
	closeCh               chan bool          = nil
	msgQueue              chan *msgQueueType = nil
	clientpool            *sync.Pool         = nil
	clientcall            syncmap.Map
	writeBufferSize       int                = DEFAULT_WRITE_BUFFER_SIZE
	readBufferSize        int                = DEFAULT_READ_BUFFER_SIZE
	enableBackpressure    bool                = false
	dropMessageOnFull     bool                = false
	workerPoolEnabled     bool                = false // 是否启用 Worker Pool
	workerPoolSize        int                = 0     // Worker 数量
	workerPoolQueues      []chan *msgQueueType       // Worker 消息队列
	workerPoolStop        chan struct{}              // Worker Pool 停止信号
	workerPoolWaitGroup   sync.WaitGroup             // Worker 等待组
	workerPoolStats       []*workerStats             // Worker 统计信息
	workerPoolStatsMutex  sync.Mutex                 // 统计信息保护锁
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

func RegisterCall(className string, factoryFunc ICall) {
	clientcall.Store(className, sync.Pool{
		New: func() interface{} {
			newCall := reflect.New(reflect.TypeOf(factoryFunc).Elem()).Interface().(ICall)
			return newCall
		},
	})
}

func Run() {
	if config.Cfg_.Ws == nil {
		return
	}

	// 从配置中读取背压设置
	if config.Cfg_.Websocket != nil {
		// 这里可以添加配置读取逻辑
		// 目前使用默认值
		enableBackpressure = true // 启用背压控制
		dropMessageOnFull = false // 通道满时是否丢弃消息
	}

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
			clientMap.Range(func(key, value interface{}) bool {
				client := value.(*Client)
				client.Close("")
				return true
			})

			//关闭消息队列
			close(msgQueue)
			return
		case read_msg := <-msgQueue:
			// 	处理消息队列
			if c, h := clientMap.Load(read_msg.uid); h {
				client := c.(*Client)
				// 检查客户端是否已关闭，防止竞态条件
				if client.IsClose() {
					continue
				}
				pbmsg := util.PasreFSMessage(read_msg.data)
				if pbmsg != nil {
					client.OnMessage(client, pbmsg)
				}
			}
		}
	}
}
