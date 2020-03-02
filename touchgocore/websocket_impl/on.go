package impl

import (
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/vars"
	"strconv"
	"strings"
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

var clientmap_ *map[string]*Client = &map[string]*Client{}

//获取所有的ws连接
func GetWsClientMap() *map[string]*Client {
	return clientmap_
}

//启动ws
func Run() {
	if config.Cfg_.Ws == "off" && config.Cfg_.Http == "off" {
		vars.Info("不启动websocket和http服务")
		return
	}

	//查询启动或者连接
	if config.Cfg_.Ws != "off" {
		szinfo := strings.Split(config.Cfg_.Ws, "|")
		for _, c := range szinfo {
			if c[0] == ':' { //这个是创建连接
				port, _ := strconv.Atoi(c[1:])
				WsListenAndServe(port)
			} else if strings.Index(c, "http") == 0 {
				client := &Client{}
				if err := client.Connection1(c); err == nil {
					(*clientmap_)[c] = client
				} else {
					vars.Error("无效的链接地址:", c)
				}
			}
		}
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
