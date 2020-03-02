package impl

import (
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/vars"
)

//这里处理消息，把所有的消息都实行汇总处理
type ConnCallback interface {
	OnConnect(*Connection) bool
	OnMessage(*Connection, interface{}) bool
	OnClose(*Connection)
}

var callBack_ ConnCallback = nil

func RegisterCallBack(cb ConnCallback) {
	callBack_ = cb
}

//默认的回调执行
type defaultCallBack struct {
}

func (this *defaultCallBack) OnConnect(conn *Connection) bool {
	return true
}

func (this *defaultCallBack) OnMessage(conn *Connection, data interface{}) bool {
	return true
}

func (this *defaultCallBack) OnClose(conn *Connection) {
}

type readData struct {
	data []byte
	conn *Connection
}

//消息处理
type WsOnMessage struct {
	readChan chan *readData //
}

var wsOnMessage_ *WsOnMessage = nil

func init() {
	wsOnMessage_ = &WsOnMessage{
		readChan: make(chan *readData, 100000), //10W读大军
	}
	go func() {
		for {
			//读数据
			select {
			case data := <-wsOnMessage_.readChan:
				if data.conn.IsClose() {
					continue
				}

				//解析操作
				data1 := &EchoPacket{buff: data.data}
				if !callBack_.OnMessage(data.conn, data1) {
					continue
				}
			}
		}
	}()
}

//启动ws
func Run() {
	if config.Cfg_.Ws == "off" {
		vars.Info("不启动websocket服务")
		return
	}
}
