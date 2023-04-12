package rpc

import (
	"net/rpc"
	"strconv"
	"sync"
	"touchgocore/config"
)

const (
	MAX_QUEUE_SIZE = 100000
)

var requestMsg chan *rpc.Call

type RpcServer struct {
	sync.RWMutex
}

// 主要是用来处理MsgQ的
func Tick() chan bool {
	// //这里是用来监听rpc应答
	// call := <-requestMsg
	// req := call.Args.(*RpcRequest)
	// if req.Ntf {//因为这里是处理回执的，所以这里不作处理
	// 	return
	// }

	// switch req.ConnType {
	// case "websocket":
	// 	//把数据转发到websocket模块去
	// 	// util.DefaultCallFunc.Do()
	// case "rpc":
	// }
	return nil
}

// 注册用的
func (self *RpcServer) RegisterClient(args *RpcRequest, reply *registerClient) error {
	reply.ServerName = config.ServerName_
	reply.ServerId = strconv.Itoa(config.GetServerID())
	return nil
}

// 消息广播
func (self *RpcServer) MsgDispatch(args *RpcRequest, reply *RpcResponse) error {
	// //查询协议所在的服务器，判断转发
	// if !args.Ntf {
	// 	if condition { //如果是本地处理的协议
	// 		//把数据转发到初始化模块去
	// 		util.DefaultCallFunc.Do(util.Dispatch, args.Params, args.protocol1, args.protocol2, args.Request)
	// 	} else {

	// 	}
	// } else {

	// }
	return nil
}
