package rpc

import (
	"net/rpc"
	"touchgocore/vars"
)

const (
	MAX_QUEUE_SIZE        = 100000
	MAX_RPC_MSG_CHAN_SIZE = 128
)

type RpcClient struct {
	Addr    string
	client  *rpc.Client
	msgQ    chan *rpc.Call
	OutMsgQ chan *rpc.Call
	err     chan error
	die     chan struct{}
	IsClose bool
}

func (this *RpcClient) Connect() error {
	// if this.client != nil && this.Heartbeat != nil {
	// 	err := this.Heartbeat(this.client)
	// 	if err == nil {
	// 		return nil
	// 	}
	// }

	// 释放旧数据
	if this.client != nil {
		this.client.Close()
	}
	// 建立新连接
	client, err := rpc.DialHTTP("tcp", this.Addr)

	vars.Debug("this = %+v", *this)

	if err == nil {
		this.client = client
		this.IsClose = false
		// if this.InitFunc != nil {
		// 	logger.LogDebug("start RpcClient.InitFunc()")
		// 	tempInfos := make([]*network_message.HD_BriefInfo, 0)
		// 	this.InitFunc(true, tempInfos)
		// }
	}

	return err
}

func catchError() {
	if x := recover(); x != nil {
		vars.Error("panic, err:%v", x)
		// helper.BackTrace("rpc client")
	}
}

func (this *RpcClient) Init(addr string) error {
	this.Addr = addr
	this.msgQ = make(chan *rpc.Call, MAX_RPC_MSG_CHAN_SIZE)
	this.err = make(chan error, MAX_RPC_MSG_CHAN_SIZE)
	this.OutMsgQ = make(chan *rpc.Call, MAX_QUEUE_SIZE)
	this.die = make(chan struct{})
	this.IsClose = true
	vars.Debug("start RpcClient.Init(), value of this = %+v", *this)
	// 转发消息和收集错误
	go func(grpc *RpcClient) {
		defer catchError()
		for {
			select {
			case call, _ := <-grpc.msgQ:
				if call.Error != nil { // 转发错误
					grpc.err <- call.Error
				} // 转发消息
				grpc.OutMsgQ <- call
			case err := <-grpc.err:
				if err != nil {
					grpc.Connect()
					vars.Error("rpc err :%v", err)
				}
			case <-grpc.die:
				close(grpc.err)
				close(grpc.OutMsgQ)
				grpc.client.Close()
				close(grpc.msgQ)
				grpc.IsClose = true
				return
			}
		}
	}(this)
	// try connect
	return this.Connect()
}

func (this *RpcClient) Go(api string, args interface{}, reply interface{}) {
	if this.client == nil || this.IsClose {
		err := this.Connect()
		if err != nil {
			// 拨号失败也要返回一个数据防止客户端卡死
			call := new(rpc.Call)
			call.ServiceMethod = api
			call.Args = args
			call.Reply = reply
			this.msgQ <- call
			vars.Error("connect server error, err:%v", err)
			return
		}
	}
	// 收集错误数据
	this.client.Go(api, args, reply, this.msgQ)
}

func (this *RpcClient) Call(api string, args interface{}, reply interface{}) error {
	if this.client == nil || this.IsClose {
		err := this.Connect()
		if err != nil {
			// 拨号失败也要返回一个数据防止客户端卡死
			call := new(rpc.Call)
			call.ServiceMethod = api
			call.Args = args
			call.Reply = reply
			this.msgQ <- call
			vars.Error("connect server error, err:%v", err)
			return err
		}
	}

	// 收集错误数据
	err := this.client.Call(api, args, reply)
	this.err <- err
	return err
}

func (this *RpcClient) Close() {
	close(this.die)
}
