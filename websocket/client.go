package websocket

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/metrics"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// uidSeed 是 UID 发号器的当前水位（纳秒时间戳基线）
var uidSeed atomic.Int64

// nextUID 取一个全局唯一、单调递增的客户端 UID。
//
// 修复前是「CAS 抢时间基线 → Add → 再 Load」三步：Add 与 Load 之间另一个并发
// 连接可以插进来，两条连接拿到同一个 UID——后写入 clientMap 的把前一条覆盖，
// 表现为互相踢线且前一条连接的读协程成了无人回收的孤儿。
// 现在「取号」与「推进号段」合成为一次原子操作。
func nextUID() int64 {
	cur := uidSeed.Load()
	if now := util.CurrentTime().UnixNano(); now > cur {
		// 让号段追上当前时间；CAS 失败说明并发方已推进过，直接 Add 依然唯一
		uidSeed.CompareAndSwap(cur, now)
	}
	return uidSeed.Add(1)
}

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
	// closeOnce/recycleOnce：关闭动作与回收动作各只执行一次
	closeOnce   sync.Once
	recycleOnce sync.Once
	// liveLoops：readLoop/handleLoop 常驻协程计数，归零后才允许回池
	liveLoops atomic.Int32
	// counted：是否已计入服务器连接统计，决定回池时是否做 -1
	counted atomic.Bool

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

// initChannels 建立连接期资源。msgChan 在实例生命周期内永不 close，
// 关闭只靠 closeCh 通知，避免「发送方仍在写入 → send on closed channel」。
func (c *Client) initChannels() {
	c.closeCh = make(chan bool, 1)
	c.msgChan = make(chan []byte, writeQueueCap())
}

// writeQueueCap 发送队列容量（单位：条）
func writeQueueCap() int {
	if writeQueueEntries > 0 {
		return writeQueueEntries
	}
	return defaultSendQueueEntries
}

// 新增带重试机制的WebSocket连接方法
func (c *Client) connectionDial(url string) error {
	const maxRetries = 3
	retryInterval := time.Second * 2
	limit := currentMaxMessageSize()

	for i := 0; i < maxRetries; i++ {
		wsConn, _, err := websocket.DefaultDialer.DialContext(wsRunCtx, url, nil)
		if err == nil {
			c.wsConnect = wsConn
			wsConn.SetReadLimit(limit)
			c.remoteAddr = url

			// ============ 改进：初始化统计 ============
			c.stats.connectTime = util.CurrentTime()
			c.stats.lastActivity.Store(util.CurrentTime())

			return nil
		}

		vars.Error("连接尝试 %d/%d 失败: %v", i+1, maxRetries, err)

		// 退避必须可取消：修复前是裸 time.Sleep，停机途中会把退出流程拖满
		// 2s+4s+8s；而生命周期已结束时继续重连更是无意义的复活。
		select {
		case <-wsRunCtx.Done():
			return fmt.Errorf("连接中止(生命周期已结束): %w", wsRunCtx.Err())
		case <-time.After(retryInterval):
		}
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
		c.finishLoop()
	}()

	conn := c.wsConnect
	if conn == nil {
		return
	}

	// 心跳与业务写共用本协程：gorilla 禁止并发写连接，单独起协程发 ping 就得
	// 再补一把写锁。放在这里天然串行，也不需要第三个常驻协程。
	pinger := time.NewTicker(currentPingInterval())
	defer pinger.Stop()

	for c.Connected() {
		select {
		case <-c.closeCh:
			// 关闭通知：立即退出，交由最后一个退出的协程完成回收
			return

		case <-pinger.C:
			if err := c.writePing(conn); err != nil {
				vars.Error("发送心跳失败: %v, 客户端地址: %s", err, c.remoteAddr)
				c.Close("发送心跳失败")
				return
			}

		case msg, ok := <-c.msgChan:
			if !ok {
				return
			}
			if !c.Connected() {
				return
			}
			// 设置写超时：对端不收时 WriteMessage 会永久阻塞，把本协程
			// 连同这条连接的发送队列钉死，背压也变成死锁。
			if err := conn.SetWriteDeadline(util.CurrentTime().Add(currentWriteTimeout())); err != nil {
				vars.Error("设置写超时失败: %v, 客户端地址: %s", err, c.remoteAddr)
				c.Close("设置写超时失败")
				return
			}
			// 执行写操作
			if err := conn.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				vars.Error("写消息失败: %v, 客户端地址: %s", err, c.remoteAddr)
				c.Close("写消息失败")
				return
			}

			// ============ 改进：更新统计 ============
			c.stats.messagesSent.Add(1)
			c.stats.bytesSent.Add(int64(len(msg)))
			c.stats.lastActivity.Store(util.CurrentTime())
		}
	}
}

// writePing 发送一次 ping 控制帧（带写超时）
func (c *Client) writePing(conn *websocket.Conn) error {
	if err := conn.SetWriteDeadline(util.CurrentTime().Add(currentWriteTimeout())); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.PingMessage, nil)
}

func (c *Client) readLoop() {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("客户端readLoop发生panic错误: %v, 客户端地址: %s", err, c.remoteAddr)
		}
		c.Close("")
		c.finishLoop()
	}()

	conn := c.wsConnect
	if conn == nil {
		return
	}

	// 读超时 + pong 续期：修复前 ReadMessage 完全没有 deadline，对端拔网线或
	// 留下半开连接时本协程永久阻塞，Client 实例与底层 socket 一起泄漏到进程退出。
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(util.CurrentTime().Add(currentReadTimeout()))
	})

	for c.Connected() {
		if err := conn.SetReadDeadline(util.CurrentTime().Add(currentReadTimeout())); err != nil {
			vars.Error("设置读超时失败: %v, 客户端地址: %s", err, c.remoteAddr)
			return
		}
		if _, data, err := conn.ReadMessage(); err == nil {
			if c.Connected() {
				item := &msgQueueType{uid: c.UID, data: data}
				if dropMessageOnFull {
					select {
					case msgQueue <- item:
						c.stats.messagesReceived.Add(1)
						c.stats.bytesReceived.Add(int64(len(data)))
						c.stats.lastActivity.Store(util.CurrentTime())
					case <-c.closeCh:
						return
					default:
						UpdateErrorStats()
						metrics.WS.IncErrors("queue_full")
						vars.Warning("WebSocket 接收队列已满，丢弃消息 uid=%d", c.UID)
					}
				} else {
					select {
					case msgQueue <- item:
						c.stats.messagesReceived.Add(1)
						c.stats.bytesReceived.Add(int64(len(data)))
						c.stats.lastActivity.Store(util.CurrentTime())
					case <-c.closeCh:
						return
					}
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

// Close 关闭连接。只负责「通知关闭」，绝不释放仍被协程使用的资源：
// msgChan 永不 close，字段清空与回池延后到 readLoop/handleLoop 全部退出之后。
func (c *Client) Close(reason string) {
	// 使用原子操作确保只关闭一次
	if !c.closed.CompareAndSwap(false, true) {
		return
	}

	c.closeOnce.Do(func() {
		// 先从映射中移除，防止新消息到达
		if clientMap != nil && c.UID != 0 {
			clientMap.Delete(c.UID)
		}

		// 调用 OnClose 回调
		if c.ICall != nil {
			c.OnClose(c)
		}

		// 关闭通知通道并断开底层连接；msgChan 保持开放，写入方靠 closed 判断丢弃
		if c.closeCh != nil {
			close(c.closeCh)
		}
		if c.wsConnect != nil {
			c.wsConnect.Close()
		}

		vars.Info("%s 连接关闭，原因：%s", c.remoteAddr, reason)
	})

	// 无常驻协程在跑（连接建立失败等场景）时立即回收
	if c.liveLoops.Load() == 0 {
		c.recycle()
	}
}

// finishLoop 常驻协程退出时调用，最后一个退出的协程负责回收
func (c *Client) finishLoop() {
	if c.liveLoops.Add(-1) <= 0 {
		c.recycle()
	}
}

// recycle 清空在用字段并归还对象池，仅在全部协程退出后执行一次
func (c *Client) recycle() {
	c.recycleOnce.Do(func() {
		// 清理客户端资源，切断对连接的引用。
		// closeCh/msgChan 保持原样：可能仍有业务协程刚通过 Connected() 检查，
		// 置 nil 会与之形成数据竞争；下一次 NewClient 会整体重建这两条通道。
		c.wsConnect = nil
		c.remoteAddr = ""
		c.UID = 0

		// 归还 ICall 到对象池
		if clientpool != nil && c.ICall != nil {
			if icallpool, ok := clientcall.Load(c.iCallName); ok {
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

		// 与 NewClient 的 +1 配对，未计入统计的失败连接不做 -1
		if c.counted.CompareAndSwap(true, false) {
			UpdateConnectionStats(false)
		}
	})
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
				metrics.WS.IncErrors("write")
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

// runConnectGate 执行业务 OnConnect 并收敛 panic：
// 外部回调 panic 冒到 HTTP 处理协程只会静默掐断连接，既没有日志也没有失败计数，
// 排查时表现为「客户端莫名连不上」。panic 与返回 false 同样按拒绝处理。
func (client *Client) runConnectGate() (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			vars.Error("WebSocket OnConnect 回调 panic, 客户端: %s: %v", client.remoteAddr, r)
			ok = false
		}
	}()
	return client.OnConnect(client)
}

// 修改InitConnection为NewClient
func NewClient(connType interface{}, remoteAddr string, className string) (*Client, error) {
	uid := nextUID()

	var client *Client = nil
	if clientpool != nil {
		client = clientpool.Get().(*Client)
		if client == nil {
			return nil, errors.New("内存池获取失败")
		}
	} else {
		client = &Client{}
	}
	// 复用的实例必须重置一次性标志与协程计数，否则上一次的关闭/回收状态会污染本次连接
	client.closed.Store(false)
	client.counted.Store(false)
	client.closeOnce = sync.Once{}
	client.recycleOnce = sync.Once{}
	client.liveLoops.Store(0)
	client.iCallName = className

	client.UID = uid
	client.remoteAddr = remoteAddr
	client.initChannels()

	// ============ 改进：初始化统计 ============
	client.stats.connectTime = util.CurrentTime()
	client.stats.lastActivity.Store(util.CurrentTime())

	switch v := connType.(type) {
	case string: // 客户端主动连接模式
		if err := client.connectionDial(v); err != nil {
			client.Close("拨号失败")
			return nil, err
		}
	case *websocket.Conn: // 服务端接收连接模式
		client.wsConnect = v
		v.SetReadLimit(currentMaxMessageSize())
	default:
		client.Close("无效的连接类型参数")
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
				client.Close("内存池获取失败")
				return nil, errors.New("内存池获取失败")
			}
			client.ICall = icall.(ICall)
		} else {
			vars.Error("未找到类名对应的ICall接口实现: %s", className)
			client.Close("未找到类名对应的ICall接口实现")
			return nil, errors.New("未找到类名对应的ICall接口实现")
		}
	} else {
		//使用默认的
		client.ICall = &defaultCall{}
	}

	// 业务 OnConnect 是外部回调：panic 必须收敛在这里，见 runConnectGate
	if !client.runConnectGate() {
		client.Close("连接初始化失败")
		return nil, errors.New("连接回调验证失败")
	}

	// 先声明「有两个常驻协程要跑」，再发布到 clientMap，
	// 保证任何能看到该客户端的协程都不会提前把它回池。
	client.liveLoops.Store(2)
	client.counted.Store(true)
	clientMap.Store(client.UID, client)
	// vars.Info("%s 连接建立成功", client.remoteAddr)
	go client.readLoop()
	go client.handleLoop()

	// ============ 改进：更新服务器统计 ============
	// 只记连接数：totalMessages 的口径是「真正处理掉的消息条数」（见 UpdateStatsFromMessage），
	// 在建立连接时 +1 会让每条连接虚增一条消息；派发侧的丢弃/解析失败另有错误计数。
	UpdateConnectionStats(true)

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
