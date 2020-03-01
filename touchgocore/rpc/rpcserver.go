package rpc

import (
	"fmt"
	"net"
	"net/rpc"
	"strconv"
	"strings"

	"github.com/TouchGoCore/touchgocore/vars"
)

//服务器链接注册
var httpserver_ *HttpServer = &HttpServer{}

type HttpServer struct {
	msgClassMap_ map[string]IRpcCallFunctionClass //需要注册的消息结构体
}

func (this *HttpServer) run() {
	//注册函数
	rpc.Register(new(defaultMsg)) //每个服务器都有个默认转发协议使用
	//其他功能消息
	for _, c := range httpserver_.msgClassMap_ {
		rpc.Register(c)
	}

	//添加监听
	listener, err := net.Listen("tcp", ":"+strconv.FormatInt(int64(rpcCfg_.ListenPort), 10))
	if err != nil {
		vars.Error("ListenTCP error:", err)
		return
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				vars.Error("Accept error:", err)
				continue
			}
			client := &Client{client: rpc.NewClient(conn)}
			res := new(string)
			if client.client.Call("defaultMsg.Register", rpcCfg_.ListenPort, res) == nil { //注册消息发送
				szlist := strings.Split(*res, "-")
				if len(szlist) != 2 {
					vars.Error("注册连接失败，返回的注册信息错误")
					continue
				}

				client.serverType = szlist[1]
				port, _ := strconv.Atoi(szlist[0])
				rpcClientMap_.Store(port, client)
				go rpc.ServeConn(conn)
			}
		}
	}()
}

func (this *HttpServer) setBus() {
	maps := map[string]string{}
	for _, regMsgClass := range this.msgClassMap_ {
		list := regMsgClass.MsgMap() //生成协议对应值
		for protocol2, strVal := range list {
			key := fmt.Sprintf("%d-%d", rpcCfg_.Protocol1, protocol2)
			maps[key] = regMsgClass.ClassName() + "." + strVal
		}
	}
	createBus(maps)
}

//注册服务器监听函数
func AddServerListen(class IRpcCallFunctionClass) {
	//插入一个准备注册的消息协议函数
	httpserver_.msgClassMap_[class.ClassName()] = class
}

//启动监控(协议一级协议,通道ID)
func Run(serverName string, buscfgpath string) {
	serverName_ = serverName
	//读取bus配置
	rpcCfg_.load(buscfgpath)

	//开启监听
	httpserver_.run()

	//开始写BUS数据
	httpserver_.setBus()

	vars.Info("初始化RPC模块完成!")
}
