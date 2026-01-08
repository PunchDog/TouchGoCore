package websocket

import (
	"fmt"
	"reflect"
	"sync"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/protobuf/proto"
)

const (
	MAX_WRITE_BUFFER_SIZE = 10240
	MAX_READ_BUFFER_SIZE  = 102400
)

var (
	closeCh    chan bool          = nil
	msgQueue   chan *msgQueueType = nil
	clientpool *sync.Pool         = nil
	clientcall syncmap.Map
)

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
	closeCh = make(chan bool)
	msgQueue = make(chan *msgQueueType, MAX_READ_BUFFER_SIZE)
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
			vars.Error(fmt.Sprintf("websocket服务启动端口%d监听失败:%s", port.Port, err.Error()))
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
			clientmap.Range(func(key, value interface{}) bool {
				client := value.(*Client)
				client.Close("")
				return true
			})

			//关闭消息队列
			close(msgQueue)
			return
		case read_msg := <-msgQueue:
			// 	处理消息队列
			if c, h := clientmap.Load(read_msg.uid); h {
				pbmsg := util.PasreFSMessage(read_msg.data)
				if pbmsg != nil {
					client := c.(*Client)
					client.OnMessage(client, pbmsg)
				}
			}
		}
	}
}
