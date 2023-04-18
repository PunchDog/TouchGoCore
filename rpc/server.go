package rpc

import (
	"fmt"
	"net/rpc"
	"strconv"
	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
	"touchgocore/websocket"
)

const (
	MAX_QUEUE_SIZE = 100000
)

var requestMsg chan *rpc.Call

// 判断是否是自己服务器用的协议,ForwardingIdx==-1获取最后一个
func serverMsgToServerName(protocol1, protocol2 int32, ForwardingIdx int8) string {
	key := fmt.Sprintf("%d-%d", protocol1, protocol2)
	//如果是-1，就获取最后一个键值
	if ForwardingIdx == -1 {
		len := redis_.Get().LLen(key).Val()
		if len > 0 {
			ForwardingIdx = int8(len) - 1
		} else {
			//出错了
			return ""
		}
	}
	//获取协议所在的链
	d := redis_.Get().LIndex(key, int64(ForwardingIdx))
	if serverName, err := d.Result(); err == nil {
		return serverName
	}

	return ""
}

type RpcServer struct {
	// sync.RWMutex
}

// 服务器反向注册用的
func (self *RpcServer) RegisterClient(args *RpcRequest, reply *registerClient) error {
	reply.ServerName = config.ServerName_
	reply.ServerId = strconv.Itoa(config.GetServerID())
	return nil
}

// 消息广播
func (self *RpcServer) MsgDispatch(args *RpcRequest, reply *RpcResponse) error {
	serverName := serverMsgToServerName(args.protocol1, args.protocol2, -1) //获取协议所在的服务器名字
	if serverName == "" {
		vars.Error(fmt.Sprintf("服务器名字获取错误，目前没有协议%d-%d对应的服务器", args.protocol1, args.protocol2))
		return nil
	}

	if serverName == config.ServerName_ {
		if !args.Websocket {
			//是本地需要处理的消息，这里广播出去
			util.DefaultCallFunc.Do(util.CallDispatch, args.protocol1, args.protocol2, args.Params, args.Request)
		} else {
			if conn := websocket.GetConn(args.ConnUid); conn != nil {
				conn.SendMsg(args.protocol1, args.protocol2, args.Request)
			}
		}
	} else {
		args.ForwardingIdx++                                                                    //转发层级+1，看下次转发到哪,如果已经是最后一层了，那就是往客户端的websocket发展了
		serverName := serverMsgToServerName(args.protocol1, args.protocol2, args.ForwardingIdx) //获取协议所在的服务器名字
		if serverName != "" {
			//不是本服的协议，需要查询协议目的地，转过去
			conn := GetConn(serverName, args.RemoteServerId[args.ForwardingIdx])
			if conn != nil {
				conn.Go(DISPATCH, args, reply) //消息转发
			} else {
				//出错了，获取不到对应的连接
				reply.Error = -1
			}
		} else {
			//出错了，获取不到对应的连接
			reply.Error = -1
		}
	}
	return nil
}

/*
注册协议链,即协议转发几个服务器((Protocol1-Protocol2)=>[]string{server1,server2,...})，有几个服务器要转发，就要填几个
其实就是设置协议在哪个服务器里执行
*/
func RegiserProtocolServerNameList(mp *syncmap.Map) {
	//循环插入注册好的跳转结构
	mp.Range(func(k, v interface{}) bool {
		list := v.([]string)
		for _, str := range list {
			redis_.Get().LPush(k.(string), str)
		}
		return true
	})
}
