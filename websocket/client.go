package websocket

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

var maxUID int64 = 0
var clientMap *syncmap.Map[int64, *Client]

// ============ 改进部分 ============

// ClientStats 客户端统计信息
type ClientStats struct {
	ConnectTime      time.Time
	Uptime           time.Duration
	MessagesSent     int64
	MessagesReceived int64
	BytesSent        int64
	BytesReceived    int64
	Errors           int64
	LastActivity     time.Time
}

// ICall WebSocket回调接口（在client.go中也定义，避免循环依赖）
type ICall interface {
	// OnConnect 创建连接时的处理
	OnConnect(client *Client) bool
	// OnMessage 收到消息时的处理
	OnMessage(client *Client, message proto.Message)
	// OnClose 关闭连接时的处理
	OnClose(client *Client)
}

// ============ 原有代码 ============

// 客户端
// 修改Client结构体定义
type Client struct {
	ICall
	wsConnect  *websocket.Conn
	remoteAddr string
	closeCh    chan bool
	msgChan    chan []byte
	UID        int64
	iCallName  string
	// 原子关闭标志，防止竞态条件
	closed atomic.Bool

	// ============ 改进：添加统计字段 ============
	stats struct {
		connectTime      time.Time
		messagesSent     atomic.Int64
		messagesReceived atomic.Int64
		bytesSent        atomic.Int64
		bytesReceived    atomic.Int64
		errors           atomic.Int64
		lastActivity     atomic.Value // time.Time
	}
}

// 新增带重试机制的WebSocket连接方法
func (c *Client) connectionDial(url string) error {
	const maxRetries = 3
	retryInterval := time.Second * 2

	for i := 0; i < maxRetries; i++ {
		wsConn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			c.wsConnect = wsConn
			wsConn.SetReadLimit(1 << 20)
			c.remoteAddr = url
			c.closeCh = make(chan bool, 1)
			c.msgChan = make(chan []byte, DEFAULT_WRITE_BUFFER_SIZE)

			// ============ 改进：初始化统计 ============
			c.stats.connectTime = util.CurrentTime()
			c.stats.lastActivity.Store(util.CurrentTime())

			return nil
		}

		vars.Error("连接尝试 %d/%d 失败: %v", i+1, maxRetries, err)
		time.Sleep(retryInterval)
		retryInterval *= 2 // 指数退避
	}

	return fmt.Errorf("连接失败，超过最大重试次数 (%d)", maxRetries)
}

func (c *Client) handleLoop() {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("客户端handleLoop发生panic错误: %v, 客户端地址: %s", err, c.remoteAddr)
		}
		c.Close("")
		runtime.Goexit()
	}()

	// 设置写超时时间，5秒
	writeTimeout := 5 * time.Second

	for c.Connected() {
		select {
		case msg, ok := <-c.msgChan:
			if !ok {
				return
			}
			if c.Connected() {
				// 设置写超时
				if err := c.wsConnect.SetWriteDeadline(util.CurrentTime().Add(writeTimeout)); err != nil {
					vars.Error("设置写超时失败: %v, 客户端地址: %s", err, c.remoteAddr)
					c.Close("设置写超时失败")
					return
				}
				// 执行写操作
				if err := c.wsConnect.WriteMessage(websocket.BinaryMessage, msg); err != nil {
					vars.Error("写消息失败: %v, 客户端地址: %s", err, c.remoteAddr)
					c.Close("写消息失败")
					return
				}

				// ============ 改进：更新统计 ============
				c.stats.messagesSent.Add(1)
				c.stats.bytesSent.Add(int64(len(msg)))
				c.stats.lastActivity.Store(util.CurrentTime())
			} else {
				return
			}
		}
	}
}

func (c *Client) readLoop() {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("客户端readLoop发生panic错误: %v, 客户端地址: %s", err, c.remoteAddr)
		}
		c.Close("")
		runtime.Goexit()
	}()

	for c.Connected() {
		if _, data, err := c.wsConnect.ReadMessage(); err == nil {
			if c.Connected() {
				select {
				case msgQueue <- &msgQueueType{uid: c.UID, data: data}:
					c.stats.messagesReceived.Add(1)
					c.stats.bytesReceived.Add(int64(len(data)))
					c.stats.lastActivity.Store(util.CurrentTime())
				case <-closeCh:
					return
				}
			}
		} else {
			return
		}
	}
}

func (c *Client) IsClose() bool {
	// 先检查原子关闭标志，性能更好
	if c.closed.Load() {
		return true
	}

	if c.closeCh == nil {
		return true
	}

	select {
	case _, ok := <-c.closeCh:
		return !ok
	default:
		return false
	}
}

func (c *Client) Connected() bool {
	return !c.IsClose()
}

func (c *Client) Close(reason string) {
	// 使用原子操作确保只关闭一次
	if c.closed.CompareAndSwap(false, true) {
		// 先从映射中移除，防止新消息到达
		clientMap.Delete(c.UID)

		// 调用 OnClose 回调
		c.OnClose(c)

		// 关闭通道和连接
		close(c.closeCh)
		if c.wsConnect != nil {
			c.wsConnect.Close()
		}
		close(c.msgChan)

		// 清理客户端资源
		addr := c.remoteAddr
		c.wsConnect = nil
		c.remoteAddr = ""
		c.UID = 0

		// 归还 ICall 到对象池
		if clientpool != nil && c.ICall != nil {
			icallpool, ok := clientcall.Load(c.iCallName)
			if ok {
				// 使用指针避免复制sync.Pool
				icallpool.Put(c.ICall)
			} else {
				vars.Error("未找到类名对应的ICall接口实现: %s", c.iCallName)
			}
			c.ICall = nil
		}

		// 归还 Client 到对象池
		if clientpool != nil {
			clientpool.Put(c)
		}

		vars.Info("%s 连接关闭，原因：%s", addr, reason)

		// ============ 改进：更新服务器统计 ============
		UpdateConnectionStats(false)
	}
}

// 发送消息
func (c *Client) SendMsg(msg ...any) {
	if !c.Connected() {
		return
	}

	l := len(msg)
	if l == 0 {
		return
	}

	// 背压控制：检查通道是否接近满
	if enableBackpressure {
		chanLen := len(c.msgChan)
		chanCap := cap(c.msgChan)
		if float64(chanLen) >= float64(chanCap)*BACKPRESSURE_THRESHOLD {
			vars.Warning("WebSocket 发送通道背压过高: len=%d, cap=%d, client=%s", chanLen, chanCap, c.remoteAddr)
			if dropMessageOnFull {
				vars.Error("WebSocket 发送通道已满，丢弃消息: client=%s", c.remoteAddr)
				return
			}
		}
	}

	if l == 1 {
		if v, ok := msg[0].([]byte); ok {
			select {
			case c.msgChan <- v:
			default:
				// 通道满时记录错误
				vars.Error("WebSocket 发送通道已满，丢弃消息: client=%s", c.remoteAddr)
				c.stats.errors.Add(1)
			}
			return
		}
	} else if l == 3 {
		p1, ok1 := msg[0].(int32)
		p2, ok2 := msg[1].(int32)
		v, ok3 := msg[2].(proto.Message)
		if !ok1 || !ok2 || !ok3 {
			vars.Error("WebSocket SendMsg 参数类型错误, 需要 (int32, int32, proto.Message)")
			c.stats.errors.Add(1)
			return
		}
		pb := util.NewFSMessage(p1, p2, v)
		if pb == nil {
			c.stats.errors.Add(1)
			return
		}
		data, err := proto.Marshal(pb)
		if err != nil {
			vars.Error("打包数据失败: %v", err)
			c.stats.errors.Add(1)
			return
		}
		select {
		case c.msgChan <- data:
		default:
			vars.Error("WebSocket 发送通道已满，丢弃消息: client=%s", c.remoteAddr)
			c.stats.errors.Add(1)
		}
		return
	}
}

// 修改InitConnection为NewClient
func NewClient(connType interface{}, remoteAddr string, className string) (*Client, error) {
	now := util.CurrentTime().UnixNano()
	for {
		cur := atomic.LoadInt64(&maxUID)
		if cur != 0 && cur <= now+1 {
			break
		}
		if atomic.CompareAndSwapInt64(&maxUID, cur, now) {
			break
		}
	}
	atomic.AddInt64(&maxUID, 1)

	var client *Client = nil
	var err error = nil
	if clientpool != nil {
		client = clientpool.Get().(*Client)
		if client == nil {
			return nil, errors.New("内存池获取失败")
		}
		// 重置原子关闭标志
		client.closed.Store(false)
	} else {
		client = &Client{}
		// 原子标志自动初始化为 false
	}

	client.UID = atomic.LoadInt64(&maxUID)
	client.remoteAddr = remoteAddr
	client.closeCh = make(chan bool, 1)
	client.msgChan = make(chan []byte, DEFAULT_WRITE_BUFFER_SIZE)
	client.iCallName = className

	// ============ 改进：初始化统计 ============
	client.stats.connectTime = util.CurrentTime()
	client.stats.lastActivity.Store(util.CurrentTime())

	defer func() {
		if err != nil {
			if client != nil && clientpool != nil {
				client.ICall = nil
				clientpool.Put(client)
			}
		}
	}()

	switch v := connType.(type) {
	case string: // 客户端主动连接模式
		if err := client.connectionDial(v); err != nil {
			return nil, err
		}
	case *websocket.Conn: // 服务端接收连接模式
		client.wsConnect = v
		v.SetReadLimit(1 << 20)
	default:
		return nil, errors.New("无效的连接类型参数")
	}

	client.remoteAddr = remoteAddr
	//使用反射创建ICall接口
	if className != "" {
		if icallpool, h := clientcall.Load(className); h {
			// 使用指针避免复制sync.Pool
			icall := icallpool.Get()
			if icall == nil {
				vars.Error("内存池获取失败: %s", className)
				return nil, errors.New("内存池获取失败")
			}
			client.ICall = icall.(ICall)
		} else {
			vars.Error("未找到类名对应的ICall接口实现: %s", className)
			return nil, errors.New("未找到类名对应的ICall接口实现")
		}
	} else {
		//使用默认的
		client.ICall = &defaultCall{}
	}

	if !client.OnConnect(client) {
		client.Close("连接初始化失败")
		return nil, errors.New("连接回调验证失败")
	}

	clientMap.Store(client.UID, client)
	// vars.Info("%s 连接建立成功", client.remoteAddr)
	go client.readLoop()
	go client.handleLoop()

	// ============ 改进：更新服务器统计 ============
	UpdateConnectionStats(true)
	UpdateMessageStats()

	return client, nil
}

// ============ 新增改进方法 ============

// GetStats 获取客户端统计信息
func (c *Client) GetStats() ClientStats {
	lastAct := c.stats.lastActivity.Load()
	var lastActivity time.Time
	if lastAct != nil {
		lastActivity = lastAct.(time.Time)
	} else {
		lastActivity = c.stats.connectTime
	}

	return ClientStats{
		ConnectTime:      c.stats.connectTime,
		Uptime:           time.Since(c.stats.connectTime),
		MessagesSent:     c.stats.messagesSent.Load(),
		MessagesReceived: c.stats.messagesReceived.Load(),
		BytesSent:        c.stats.bytesSent.Load(),
		BytesReceived:    c.stats.bytesReceived.Load(),
		Errors:           c.stats.errors.Load(),
		LastActivity:     lastActivity,
	}
}

// UpdateStatsFromMessage 从消息更新统计（用于Tick函数）
func (c *Client) UpdateStatsFromMessage(data []byte) {
	c.stats.messagesReceived.Add(1)
	c.stats.bytesReceived.Add(int64(len(data)))
	c.stats.lastActivity.Store(util.CurrentTime())
	UpdateMessageStats()
}
