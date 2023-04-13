package rpc

import (
	"net/rpc"
	"strconv"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

type RpcClient struct {
	Addr       string
	client     *rpc.Client
	ServerName string //连接的服务器名字
	ServerId   string //服务器ID
}

func (this *RpcClient) Connect() error {
	// 释放旧数据
	if this.client != nil {
		this.client.Close()
	}
	// 建立新连接
	client, err := rpc.Dial("tcp", this.Addr)

	vars.Debug("this = %+v", *this)

	if err == nil {
		this.client = client
	}

	return err
}

func (this *RpcClient) Init(addr string, servername, serverid string, connect *rpc.Client) error {
	this.Addr = addr
	this.ServerName = servername
	this.ServerId = serverid
	vars.Debug("start RpcClient.Init(), value of this = %+v", *this)

	connectok := true
	var err error = nil
	if connect == nil {
		err = this.Connect()
		if err != nil {
			connectok = false
		}
	} else {
		this.client = connect
	}

	if connectok {
		//保存连接
		rpcclientmap.LoadAndFunction(servername, func(v interface{}, storefn func(v1 interface{}), delfn func()) {
			var mp *syncmap.Map
			if v != nil {
				mp = v.(*syncmap.Map)
			} else {
				mp = new(syncmap.Map)
			}

			mp.Store(serverid, this)
			storefn(mp)
		})
	}
	return err
}

func (this *RpcClient) Go(api string, args interface{}, reply interface{}) {
	go func() {
		done := this.client.Go(api, args, reply, nil)
		if done.Error == nil { //正常消息
			// requestMsg <- done
		} else { //断线了，先重连试试，不行就删除
			if err := this.Connect(); err == nil {
				this.Go(api, args, reply) //客户端重连一次，还不行就删除
				return
			}
			this.Close()
		}
	}()
}

func (this *RpcClient) Call(api string, args interface{}, reply interface{}) error {
	err := this.client.Call(api, args, reply)
	if err != nil {
		this.Close()
	}
	return err
}

func (this *RpcClient) Close() {
	this.client.Close()
	rpcclientmap.LoadAndFunction(this.ServerName, func(v interface{}, storefn func(v1 interface{}), delfn func()) {
		if v != nil {
			mp := v.(*syncmap.Map)
			mp.Delete(this.ServerId)
			if mp.Length() == 0 {
				delfn()
			}
		}
	})
}

// 获取或者创建客户端连接
func GetConn(servername string, serverid int) *RpcClient {
	szServerID := strconv.Itoa(serverid)
	//不能连接自己
	if servername == config.ServerName_ && serverid == config.GetServerID() {
		vars.Error("rpc不能连接自己:servername-" + servername + " serverid-" + szServerID)
		// <-time.After(time.Nanosecond * 10)
		// panic("rpc不能连接自己:servername-" + servername + " serverid-" + szServerID)
		return nil
	}

	//先查询有没有，没有才创建
	if conn := getConn(servername, serverid); conn != nil {
		return conn
	}

	//没有就创建
	cmd := redis_.Get().HGet(servername, szServerID)
	if addr, err := cmd.Result(); err == nil {
		conn := new(RpcClient)
		if err1 := conn.Init(addr, servername, szServerID, nil); err1 == nil {
			return conn
		}
	}
	return nil
}

// 获取客户端连接
func getConn(servername string, serverid int) (conn *RpcClient) {
	conn = nil
	//保存连接
	rpcclientmap.LoadAndFunction(servername, func(v interface{}, storefn func(v1 interface{}), delfn func()) {
		if v != nil {
			mp := v.(*syncmap.Map)
			c, ok := mp.Load(serverid)
			if ok {
				conn = c.(*RpcClient)
			}
		}
	})
	return
}
