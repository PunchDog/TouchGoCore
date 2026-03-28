package websocket

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

// maxUID 使用原子操作保证并发安全
var maxUID atomic.Int64
var clientMap syncmap.Map

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
}

// 新增带重试机制的WebSocket连接方法
func (c *Client) connectionDial(url string) error {
	const maxRetries = 3
	retryInterval := time.Second * 2

	for i := 0; i < maxRetries; i++ {
		wsConn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			c.wsConnect = wsConn
			c.remoteAddr = url
			c.closeCh = make(chan bool, 1)
			c.msgChan = make(chan []byte, DEFAULT_WRITE_BUFFER_SIZE)

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
				if err := c.wsConnect.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
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
				msgQueue <- &msgQueueType{uid: c.UID, data: data}
			}
		} else {
			return
		}
	}
}

func (c *Client) IsClose() bool {
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
	if c.Connected() {
		// 先调用 OnClose 回调
		c.OnClose(c)

		// 关闭通道和连接
		close(c.closeCh)
		if c.wsConnect != nil {
			c.wsConnect.Close()
		}
		close(c.msgChan)

		// 从客户端映射中移除
		clientMap.Delete(c.UID)

		// 清理客户端资源
		c.wsConnect = nil
		c.remoteAddr = ""
		c.UID = 0

		// 归还 ICall 到对象池
		if clientpool != nil && c.ICall != nil {
			v, ok := clientcall.Load(c.iCallName)
			if ok {
				icallpool := v.(sync.Pool)
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

		vars.Info("%s 连接关闭，原因：%s", c.remoteAddr, reason)
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
			}
			return
		}
	} else if l == 3 {
		// 使用的是 protobuf，传入数据 cmd1, cmd2, proto message
		if v, ok := msg[2].(proto.Message); ok {
			pb := util.NewFSMessage(msg[0].(int32), msg[1].(int32), v)
			data, err := proto.Marshal(pb)
			if err != nil {
				vars.Error("打包数据失败: %v", err)
				return
			}
			select {
			case c.msgChan <- data:
			default:
				vars.Error("WebSocket 发送通道已满，丢弃消息: client=%s", c.remoteAddr)
			}
			return
		}
	}
}

// 修改InitConnection为NewClient
func NewClient(connType interface{}, remoteAddr string, className string) (*Client, error) {
	// 使用 atomic.Int64 保证 UID 唯一性，无竞态
	now := time.Now().UnixNano()
	for {
		cur := maxUID.Load()
		next := cur + 1
		if cur == 0 || next > now {
			// 初始化或防溢出：以当前纳秒为基准
			if maxUID.CompareAndSwap(cur, now+1) {
				next = now + 1
			} else {
				continue
			}
		} else {
			if !maxUID.CompareAndSwap(cur, next) {
				continue
			}
		}
		// 成功分配到 next
		_ = next
		break
	}
	uid := maxUID.Load()

	var client *Client = nil
	var err error = nil
	if clientpool != nil {
		client = clientpool.Get().(*Client)
		if client == nil {
			return nil, errors.New("内存池获取失败")
		}
	} else {
		client = &Client{}
	}

	client.UID = uid
	client.remoteAddr = remoteAddr
	client.closeCh = make(chan bool, 1)
	client.msgChan = make(chan []byte, DEFAULT_WRITE_BUFFER_SIZE)
	client.iCallName = className

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
	default:
		return nil, errors.New("无效的连接类型参数")
	}

	client.remoteAddr = remoteAddr
	//使用反射创建ICall接口
	if className != "" {
		if v, h := clientcall.Load(className); h {
			icallpool := v.(sync.Pool)
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
	return client, nil
}
