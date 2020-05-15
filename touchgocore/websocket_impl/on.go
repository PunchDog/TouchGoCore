package impl

import (
	"strconv"
	"strings"

	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/util"
	"github.com/PunchDog/TouchGoCore/touchgocore/vars"
	"github.com/gorilla/websocket"
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
	//封号地区或者封号连接处理
	//if util.IsPublicIP(net.ParseIP(conn.RemoteAddr())) {
	//	redis, _ := model.NewRedis(&util.LobbyConfig_.IM.RedisConfig)
	//	key1 := strconv.Itoa(util.LobbyConfig_.IM.Config_id)
	//	ForbiddenIp := redis.Get().HGet(key1, "ForbiddenIp").Val() //获取黑名单IP和地址
	//	forbiddenIpList := strings.Split(ForbiddenIp, ",")
	//	for _, ip := range forbiddenIpList {
	//		//直接就是被封的IP
	//		if ip == conn.RemoteAddr() {
	//			return false
	//		} else {
	//			info := util.TabaoIpAPI(conn.RemoteAddr())
	//			if info != nil && info.Data.City == ip {
	//				return false
	//			}
	//		}
	//	}
	//}
	return true
}

func (this *defaultCallBack) OnMessage(conn *Connection, data interface{}) bool {
	echoPacket := data.(*EchoPacket)
	body := echoPacket.GetBody()
	protocol2 := echoPacket.GetProtocol2()
	protocol1 := echoPacket.GetProtocol1()

	SMsgDispatch_.Do(protocol1, protocol2, conn, body)
	return true
}

func (this *defaultCallBack) OnClose(conn *Connection) {
}

type rwData struct {
	data []byte
	conn *Connection
}

//消息处理
type WsOnMessage struct {
	readChan  chan *rwData //
	writeChan chan *rwData //
}

var wsOnMessage_ *WsOnMessage = nil

var clientmap_ *map[string]*Client = &map[string]*Client{}

//获取所有的ws连接
func GetWsClientMap() *map[string]*Client {
	return clientmap_
}

var redis_ *db.Redis = nil

//启动ws
func Run() {
	if config.Cfg_.Ws == "off" && config.Cfg_.Http == "off" {
		vars.Info("不启动websocket和http服务")
		return
	}

	//加载redis
	redis_, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic("加载redis错误:" + err.Error())
	}

	//查询启动或者连接
	if config.Cfg_.Ws != "off" {
		szinfo := strings.Split(config.Cfg_.Ws, "|")
		for _, c := range szinfo {
			if c[0] == ':' { //这个是创建连接
				for {
					//查询端口占用
					szPort := c[1:]
					if util.CheckPort(szPort) != nil {
						continue
					}
					port, _ := strconv.Atoi(szPort)
					WsListenAndServe(port)
					//设置服务器连接数
					redis_.Get().HSet("wsListen", port, 0)
					break
				}
			} else if strings.Index(c, "http") == 0 {
				client := &Client{}
				if err := client.Connection1(c); err == nil {
					(*clientmap_)[c] = client
				} else {
					vars.Error("无效的链接地址:", c)
				}
			}
		}

		wsOnMessage_ = &WsOnMessage{
			readChan:  make(chan *rwData, 100000), //10W读大军
			writeChan: make(chan *rwData, 100000), //10W写大军
		}
		go handleloop()
		go writeloop()

		vars.Info("WS启动完成")
	}
	if config.Cfg_.Http != "off" {
		szinfo := strings.Split(config.Cfg_.Http, "|")
		for _, c := range szinfo {
			port, _ := strconv.Atoi(c)
			HttpListenAndServe(port)
		}
		vars.Info("Http启动完成")
	}
}

func handleloop() {
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
}

func writeloop() {
	for {
		//写数据
		select {
		case data := <-wsOnMessage_.writeChan:
			if data.conn.IsClose() {
				vars.Error("socket已经关闭")
				continue
			}

			if err := data.conn.wsConnect.WriteMessage(websocket.BinaryMessage, data.data); err != nil {
				vars.Error("发送消息出错:", err)
				continue
			}
		}
	}
}
