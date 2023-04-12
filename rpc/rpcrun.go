package rpc

import (
	"net"
	"net/http"
	"net/rpc"
	"strconv"
	"time"
	"touchgocore/config"
	"touchgocore/db"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

// 客户端连接(servername/(serverid/conn))
var rpcclientmap *syncmap.Map
var redis_ *db.Redis = nil

func init() {
	rpcclientmap = new(syncmap.Map)
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
	rpc.Register(new(RpcServer))
	rpc.HandleHTTP()
	szPort := strconv.Itoa(config.Cfg_.RpcPort)
	listen, err := net.Listen("tcp", "0.0.0.0:"+szPort)
	if nil != err {
		vars.Error("listen error:", err)
	}
	go http.Serve(listen, nil)
	//设置redis记录(服务器名字；服务器ID；IP:端口)
	redis_.Get().HSet(config.ServerName_, strconv.Itoa(config.GetServerID()), config.Cfg_.Ip+":"+szPort)
	vars.Info("启动rpc服务成功")
}

// 停止rpc
func Stop() {
	//循环关闭客户端连接
	rpcclientmap.ClearAll(func(k, v interface{}) bool {
		clientmap := v.(*syncmap.Map)
		clientmap.ClearAll(func(k, v interface{}) bool {
			client := v.(*RpcClient)
			client.Close()
			return true
		})
		return true
	})
	//删除监听映射
	redis_.Get().HDel(config.ServerName_, strconv.Itoa(config.GetServerID()))
}

// 创建客户端连接
func GetConn(servername string, serverid int) *RpcClient {
	szServerID := strconv.Itoa(serverid)
	//不能连接自己
	if servername == config.ServerName_ && serverid == config.GetServerID() {
		vars.Error("rpc不能连接自己:servername-" + servername + " serverid-" + szServerID)
		<-time.After(time.Nanosecond * 10)
		panic("rpc不能连接自己:servername-" + servername + " serverid-" + szServerID)
	}

	cmd := redis_.Get().HGet(servername, szServerID)
	if addr, err := cmd.Result(); err == nil {
		conn := new(RpcClient)
		if err1 := conn.Init(addr); err1 == nil {
			//保存连接
			rpcclientmap.LoadAndFunction(servername, func(v interface{}, storefn func(v1 interface{}), delfn func()) {
				var mp *syncmap.Map
				if v != nil {
					mp = v.(*syncmap.Map)
				} else {
					mp = new(syncmap.Map)
				}
				mp.Store(szServerID, conn)
				storefn(mp)
			})
			return conn
		}
	}
	return nil
}
