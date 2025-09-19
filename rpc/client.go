package rpc

import (
	"context"
	"reflect"
	"strconv"
	"touchgocore/network/message"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var (
	// RpcClient rpc客户端
	rpcClient_ map[string]*RpcClient
)

type RpcClient struct {
	util.Timer
	// 连接
	addr string
	//端口
	port int
	//对应的服务器名
	serverName string
	// 连接状态
	connStatus bool
	//
	conn *grpc.ClientConn
}

func (c *RpcClient) Tick() {
	//断线重连，链接上了就从计时器里移除
	conn, err := newClient(c.addr)
	if err != nil {
		return
	}
	c.conn = conn
	c.connStatus = true
	c.Remove()
}

func (c *RpcClient) SendMsg(protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	//客户端context创建
	// 客户端发送流式请求时附加元数据
	md := metadata.Pairs("client-name", c.serverName)
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	client := message.NewGrpcClient(c.conn)
	msg, err := client.Msg(ctx)
	if err != nil {
		c.connStatus = false
		util.AddTimer(c)
		return
	}

	req := util.NewFSMessage(protocol1, protocol2, pb)
	err = msg.Send(req)
	if err != nil {
		//连接断开
		c.connStatus = false
		util.AddTimer(c)
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

func newClient(addr string) (*grpc.ClientConn, error) {
	opt := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MAX_MSG_SIZE)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(MAX_MSG_SIZE)),
	}

	conn, err := grpc.NewClient(addr, opt...)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func NewRpcClient(servername, addr string, port int) *RpcClient {
	if rpcClient_ == nil {
		rpcClient_ = make(map[string]*RpcClient)
	}

	//创建一个带计时器的客户端指针
	client := util.NewTimer(1000, -1, &RpcClient{}).(*RpcClient)
	client.addr = addr
	client.port = port
	client.serverName = servername
	conn, err := newClient(addr + ":" + strconv.Itoa(port))
	if err == nil {
		client.connStatus = true
		client.conn = conn
	} else { //一直保持监听保证连接
		client.connStatus = false
		util.AddTimer(client)
	}

	rpcClient_[servername] = client
	return rpcClient_[servername]
}
