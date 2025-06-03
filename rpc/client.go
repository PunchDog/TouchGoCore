package rpc

import (
	"context"
	"reflect"
	"touchgocore/network/message"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

var (
	// RpcClient rpc客户端
	rpcClient_ map[string]*RpcClient
)

type RpcClient struct {
	client message.GrpcClient
	// 连接
	addr string
	//对应的服务器名
	serverName string
	// 连接状态
	connStatus bool
	//
	conn *grpc.ClientConn
}

func (c *RpcClient) SendMsg(protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	msg, err := c.client.Msg(context.Background())
	if err != nil {
		c.connStatus = false
		return
	}

	req := util.NewFSMessage(protocol1, protocol2, pb)
	err = msg.Send(req)
	if err != nil {
		//连接断开
		c.connStatus = false
		return
	}

	recv, err := msg.Recv()
	if err != nil {
		vars.Error("接收数据失败:", err)
		return
	}
	if callfunc != nil {
		res := util.PasreFSMessage(recv)
		if res != nil && callfunc != nil {
			//判断res和pb1是否相同类型
			//通过反射获取callfunc形参PB1的类型
			reflectType := reflect.TypeOf(callfunc)
			//获取callfunc形参PB1的类型
			pb1Type := reflectType.In(0)
			//判断res和pb1是否相同类型
			if reflect.TypeOf(res) == pb1Type {
				callfunc(res)
			} else {
				vars.Error("callfunc形参PB1的类型和res的类型不相同")
			}
		}
	}
}

func NewRpcClient(servername, addr string) *RpcClient {
	if rpcClient_ == nil {
		rpcClient_ = make(map[string]*RpcClient)
	}

	conn, err := grpc.NewClient(addr)
	if err != nil {
		return nil
	}

	client := &RpcClient{
		client:     message.NewGrpcClient(conn),
		addr:       addr,
		serverName: servername,
		connStatus: true,
		conn:       conn,
	}

	rpcClient_[servername] = client
	return rpcClient_[servername]
}
