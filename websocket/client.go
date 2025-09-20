package websocket

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

var max_uid int64 = 0
var clientmap syncmap.Map

// 客户端
// 修改Client结构体定义
type Client struct {
	ICall
	wsConnect  *websocket.Conn
	remoteAddr string
	closeCh    chan bool
	msgChan    chan []byte
	Uid        int64
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
			c.msgChan = make(chan []byte, MAX_WRITE_BUFFER_SIZE)

			return nil
		}

		vars.Error(fmt.Sprintf("连接尝试 %d/%d 失败: %v", i+1, maxRetries, err))
		time.Sleep(retryInterval)
		retryInterval *= 2 // 指数退避
	}

	return fmt.Errorf("连接失败，超过最大重试次数 (%d)", maxRetries)
}

func (c *Client) handleLoop() {
	defer func() {
		c.Close("")
		runtime.Goexit()
	}()
	for c.Connected() {
		select {
		case msg, ok := <-c.msgChan:
			if !ok {
				return
			}
			if c.Connected() {
				c.wsConnect.WriteMessage(websocket.BinaryMessage, msg)
			} else {
				return
			}
		}
	}
}

func (c *Client) readLoop() {
	defer func() {
		c.Close("")
		runtime.Goexit()
	}()

	for c.Connected() {
		if _, data, err := c.wsConnect.ReadMessage(); err == nil {
			if c.Connected() {
				msgQueue <- &msgQueueType{uid: c.Uid, data: data}
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
		c.OnClose(c)
		close(c.closeCh)
		c.wsConnect.Close()
		close(c.msgChan)
		c.wsConnect = nil
		clientmap.Delete(c.Uid)
		c.wsConnect = nil
		c.remoteAddr = ""
		c.Uid = 0
		if clientpool != nil {
			v, ok := clientcall.Load(c.iCallName)
			if ok {
				icallpool := v.(sync.Pool)
				icallpool.Put(c.ICall)
			} else {
				vars.Error("未找到类名对应的ICall接口实现: %s", c.iCallName)
			}
			c.ICall = nil
			clientpool.Put(c)
		}
		vars.Info(fmt.Sprintf("%s 连接关闭，原因：%s", c.remoteAddr, reason))
	}
}

// 发送消息
func (c *Client) SendMsg(msg ...any) {
	if c.Connected() {
		l := len(msg)
		if l == 0 {
			return
		}
		if l == 1 {
			if v, ok := msg[0].([]byte); ok {
				c.msgChan <- v
				return
			}
		} else if l == 3 {
			//使用的是protobuf,传入数据cmd1,cmd2,protomessage
			if v, ok := msg[2].(proto.Message); ok {
				pb := util.NewFSMessage(msg[0].(int32), msg[1].(int32), v)
				data, err := proto.Marshal(pb)
				if err != nil {
					vars.Error("打包数据失败:", err)
					return
				}
				c.msgChan <- data
				return
			}
		}
	}
}

// 修改InitConnection为NewClient
func NewClient(connType interface{}, remoteAddr string, className string) (*Client, error) {
	if max_uid == 0 || max_uid > time.Now().UnixNano()+1 {
		max_uid = time.Now().UnixNano() + 1
	} else {
		max_uid++
	}

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

	client.Uid = max_uid
	client.remoteAddr = remoteAddr
	client.closeCh = make(chan bool, 1)
	client.msgChan = make(chan []byte, MAX_WRITE_BUFFER_SIZE)
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

	clientmap.Store(client.Uid, client)
	vars.Info(fmt.Sprintf("%s 连接建立成功", client.remoteAddr))
	go client.readLoop()
	go client.handleLoop()
	return client, nil
}
