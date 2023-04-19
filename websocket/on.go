package websocket

import (
	"reflect"
	"strconv"
	"strings"

	"touchgocore/config"
	"touchgocore/db"
	"touchgocore/network"
	network_message "touchgocore/network/message"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
)

// 执行创建分发消息用
var callBack_ IConnCallback = nil

// 消息数据
var wsOnMessage_ *WsOnMessage = nil
var redis_ *db.Redis = nil

const (
	MSG_PACKAGE_NAME = "network.message."
)

// 这里处理消息，把所有的消息都实行汇总处理
type IConnCallback interface {
	OnConnect(*Connection) bool
	OnMessage(*Connection, *util.EchoPacket) bool
	OnClose(*Connection)
}

// 默认的回调执行
type defaultCallBack struct {
}

func (this *defaultCallBack) OnConnect(conn *Connection) bool {
	// //封号地区或者封号连接处理
	// if util.IsPublicIP(net.ParseIP(conn.RemoteAddr())) {
	// 	redis, _ := db.NewRedis(config.Cfg_.Redis)
	// 	key1 := strconv.Itoa(util.LobbyConfig_.IM.Config_id)
	// 	ForbiddenIp := redis.Get().HGet(key1, "ForbiddenIp").Val() //获取黑名单IP和地址
	// 	forbiddenIpList := strings.Split(ForbiddenIp, ",")
	// 	for _, ip := range forbiddenIpList {
	// 		//直接就是被封的IP
	// 		if ip == conn.RemoteAddr() {
	// 			return false
	// 		} else {
	// 			info := util.TabaoIpAPI(conn.RemoteAddr())
	// 			if info != nil && info.Data.City == ip {
	// 				return false
	// 			}
	// 		}
	// 	}
	// }
	return true
}

// 这就是一个收消息的模板
func (this *defaultCallBack) OnMessage(conn *Connection, echoPacket *util.EchoPacket) bool {
	body := echoPacket.GetBody()
	protocol2 := echoPacket.GetProtocol2()
	protocol1 := echoPacket.GetProtocol1()

	all := new(network_message.FSMessage)
	err := proto.Unmarshal(body, all)
	if err != nil {
		return false
	}

	protoName := network.GetProtoName(protocol1, protocol2)
	msgType := proto.MessageType(MSG_PACKAGE_NAME + protoName)
	if msgType == nil {
		return false
	}
	msg := reflect.New(msgType.Elem()).Interface().(proto.Message)
	err = proto.Unmarshal(all.GetBody(), msg)
	if err != nil {
		return false
	}
	// uid int64, protocol1 int32, protocol2 int32, pb proto.Message, params []interface{}, remoteServerId []int
	util.DefaultCallFunc.Do(util.CallRpcMsg, conn.Uid, protocol1, 0, msg, nil, nil)
	return true
}

func (this *defaultCallBack) OnClose(conn *Connection) {
}

type rwData struct {
	data []byte
	conn *Connection
}

// 消息处理
type WsOnMessage struct {
	readChan  chan *rwData //
	writeChan chan *rwData //
}

// 启动ws
func Run() {
	if config.Cfg_.Ws == "off" || config.Cfg_.Ws == "" {
		vars.Info("不启动websocket")
		return
	}

	//加载redis
	var err error
	redis_, err = db.NewRedis(config.Cfg_.Redis)
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
					redis_.Get().HSet("wsListen", config.Cfg_.Ip+":"+szPort, 0)
					break
				}
			} else if strings.Index(c, "http") == 0 {
				if err := clientConnection(c); err != nil {
					vars.Error("无效的链接地址:", c)
				}
			}
		}

		wsOnMessage_ = &WsOnMessage{
			readChan:  make(chan *rwData, 100000),
			writeChan: make(chan *rwData, 100000),
		}

		vars.Info("WS启动完成")
	}
}

func Stop() {
	close(wsOnMessage_.readChan)
	close(wsOnMessage_.writeChan)
	wsOnMessage_ = nil
}

func Handle() chan bool {
	if wsOnMessage_ == nil {
		return nil
	}

	//数据操作
	select {
	case data := <-wsOnMessage_.readChan:
		if data.conn.IsClose() {
			return nil
		}

		//解析操作
		data1 := util.InitEchoPacket(data.data)
		if !callBack_.OnMessage(data.conn, data1) {
			return nil
		}
	case data := <-wsOnMessage_.writeChan:
		if data.conn.IsClose() {
			vars.Error("socket已经关闭")
			return nil
		}

		if err := data.conn.wsConnect.WriteMessage(websocket.BinaryMessage, data.data); err != nil {
			vars.Error("发送消息出错:", err)
			return nil
		}
	default:
	}
	return nil
}
