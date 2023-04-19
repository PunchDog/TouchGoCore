package rpc

import (
	"fmt"
	"net/rpc"
	"strconv"
	"touchgocore/config"
	"touchgocore/network"
	"touchgocore/util"
	"touchgocore/vars"

	"github.com/golang/protobuf/proto"
)

const (
	MAX_QUEUE_SIZE = 100000
)

var requestMsg chan *rpc.Call
var server *RpcServer

func init() {
	server = new(RpcServer)
	util.DefaultCallFunc.Register(util.CallRpcMsg, MsgDispatch)
}

func MsgDispatch(uid int64, protocol1 int32, protocol2 int32, pb proto.Message, params []interface{}, remoteServerId []int) {
	server.MsgDispatch(&RpcRequest{
		Websocket:      false,
		ConnUid:        uid,
		RemoteServerId: remoteServerId,
		ForwardingIdx:  0,
		Params:         params,
		protocol1:      protocol1,
		protocol2:      protocol2,
		Request:        pb,
	}, nil)
}

// 判断是否是自己服务器用的协议,ForwardingIdx==-1获取最后一个
func serverMsgToServerName(protocol1, protocol2 int32, ForwardingIdx int8) string {
	return network.ServerMsgToServerName(protocol1, protocol2, ForwardingIdx)
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
			//广播给websocket模块去发消息
			util.DefaultCallFunc.Do(util.CallWebSocketMsg, args.ConnUid, args.protocol1, args.protocol2, args.Request)
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
