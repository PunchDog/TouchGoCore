package rpc

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/config"
	"github.com/TouchGoCore/touchgocore/util"
	"github.com/TouchGoCore/touchgocore/vars"
	"net"
	"net/rpc"
	"strconv"
)

//服务器链接注册
var httpserver_ *HttpServer = &HttpServer{
	msgClassMap_: make(map[string]IRpcCallFunctionClass),
}

type HttpServer struct {
	msgClassMap_ map[string]IRpcCallFunctionClass //需要注册的消息结构体
	port         int
}

func (this *HttpServer) run() {
	//注册函数
	rpc.Register(new(DefaultMsg)) //每个服务器都有个默认转发协议使用
	//其他功能消息
	for _, c := range httpserver_.msgClassMap_ {
		rpc.Register(c.RpcFunc())
	}

	this.port = config.Cfg_.ListenPort
	//查询可以用的端口
	for {
		if util.CheckPort(strconv.FormatInt(int64(this.port), 10)) != nil {
			this.port++
		}
		break
	}

	//添加监听
	listener, err := net.Listen("tcp", ":"+strconv.FormatInt(int64(this.port), 10))
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
			go rpc.ServeConn(conn)
		}
	}()
}

func (this *HttpServer) setBus() {
	maps := map[string]string{}
	for _, regMsgClass := range this.msgClassMap_ {
		list := regMsgClass.MsgMap() //生成协议对应值
		for protocol2, strVal := range list {
			key := fmt.Sprintf("%d-%d", regMsgClass.Protocol1(), protocol2)
			maps[key] = strVal
		}
	}
	createBus(maps)
}

//注册服务器监听函数
func AddServerListen(class IRpcCallFunctionClass) {
	//插入一个准备注册的消息协议函数
	httpserver_.msgClassMap_[util.GetClassName(class.RpcFunc())] = class
}

//启动监控(协议一级协议,通道ID)
func Run() {
	//开启监听
	httpserver_.run()

	//开始写BUS数据
	httpserver_.setBus()

	vars.Info("初始化RPC模块完成!")
}

func Stop() {
	removeBus()
}
