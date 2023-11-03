package rpc

import (
	"net"
	"net/rpc"
	"strconv"
	"time"
	"touchgocore/config"
	"touchgocore/db"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
)

// 创建客户端连接(servername/(serverid/conn))
var rpcclientmap *syncmap.Map

var redis_ *db.Redis = nil
var szListenPort string
var lisenter net.Listener

func init() {
	rpcclientmap = new(syncmap.Map)
	requestMsg = make(chan *rpc.Call, MAX_QUEUE_SIZE)
}

// 激活Rpc服务器
func Run() {
	//服务器方面
	if config.Cfg_.RpcPort == 0 {
		vars.Info("不启动rpc服务")
		return
	}
	var err error
	redis_, err = db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		vars.Error("加载redis错误:" + err.Error())
		<-time.After(time.Nanosecond * 10)
		panic("加载redis错误:" + err.Error())
	}
	rpc.Register(server)                          //默认注册的rpc服务器操作
	util.DefaultCallFunc.Do(util.CallRegisterRpc) //告知其他自定义的rpc模块可以注册了

	//查询一个可以使用的端口生成监听
	port := config.Cfg_.RpcPort
	for {
		szPort := strconv.Itoa(port)
		if util.CheckPort(szPort) != nil {
			port++
			continue
		}
		listen, err := net.Listen("tcp", "0.0.0.0:"+szPort)
		if nil != err {
			vars.Error("listen error:", err)
			port++
			continue
		}

		lisenter = listen

		//监听
		go func() {
			for {
				conn, err := lisenter.Accept()
				if err != nil {
					//断开监听
					vars.Error("accept error:", err)
					break
				}
				//先创建模板client，获取连接数据
				clienttemp := rpc.NewClient(conn)
				reg := new(registerClient)
				if err = clienttemp.Call(REGISTER_SERVER, nil, reg); err == nil {
					//创建本地client
					client := new(RpcClient)
					client.Init(conn.RemoteAddr().String(), reg.ServerName, reg.ServerId, clienttemp)
				}
			}
		}()
		//设置redis记录(服务器名字；服务器ID；IP:端口)
		redis_.Get().HSet(config.ServerName_, strconv.Itoa(config.GetServerID()), config.Cfg_.Ip+":"+szPort)
		szListenPort = szPort
		break
	}
	vars.Info("启动rpc服务成功")
}

// 停止rpc
func Stop() {
	if config.Cfg_.RpcPort == 0 {
		return
	}

	lisenter.Close() //关闭监听
	//删除监听映射
	redis_.Get().HDel(config.ServerName_, strconv.Itoa(config.GetServerID()))

	//循环关闭客户端连接
	closefn := func(mp *syncmap.Map) {
		mp.ClearAll(func(k, v interface{}) bool {
			clientmap := v.(*syncmap.Map)
			clientmap.ClearAll(func(k, v interface{}) bool {
				client := v.(*RpcClient)
				client.client.Close()
				return true
			})
			return true
		})
	}
	closefn(rpcclientmap)
}
