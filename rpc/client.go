package rpc

import (
	"context"
	"reflect"
	"strconv"
	"sync/atomic"
	"touchgocore/localtimer"
	"touchgocore/network/message"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

var (
	// RpcClient rpc客户端
	rpcClient_ syncmap.Map
)

type RpcClient struct {
	localtimer.Timer
	// 连接地址（不含端口）
	addr string
	//端口
	port int
	// 完整地址 addr:port
	fullAddr string
	//对应的服务器名
	serverName string
	// 连接状态 (原子操作)
	connStatus atomic.Bool
	// 连接 (原子操作)
	conn atomic.Value // *grpc.ClientConn
}

func (c *RpcClient) Tick() {
	//断线重连，链接上了就从计时器里移除
	conn, err := newClient(c.fullAddr)
	if err != nil {
		vars.Error("RPC客户端连接失败[%s]: %v", c.fullAddr, err)
		return
	}
	c.conn.Store(conn)
	c.connStatus.Store(true)
	c.Remove()
}

// markDisconnected 标记连接断开，并启动重连定时器
func (c *RpcClient) markDisconnected() {
	c.connStatus.Store(false)
	localtimer.AddTimer(c)
}

func (c *RpcClient) SendMsg(protocol1, protocol2 int32, pb proto.Message, callfunc func(pb1 proto.Message)) {
	// 从原子值获取连接
	connVal := c.conn.Load()
	if connVal == nil {
		vars.Error("RPC客户端连接未就绪[%s]，协议:%d:%d", c.fullAddr, protocol1, protocol2)
		c.markDisconnected()
		return
	}
	conn := connVal.(*grpc.ClientConn)

	// 客户端context创建
	md := metadata.Pairs("client-name", c.serverName)
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	client := message.NewGrpcClient(conn)
	msg, err := client.Msg(ctx)
	if err != nil {
		vars.Error("RPC客户端创建流失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		c.markDisconnected()
		return
	}

	req := util.NewFSMessage(protocol1, protocol2, pb)
	err = msg.Send(req)
	if err != nil {
		vars.Error("RPC客户端发送失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		c.markDisconnected()
		return
	}

	recv, err := msg.Recv()
	if err != nil {
		vars.Error("RPC客户端接收失败[%s] 协议:%d:%d: %v", c.fullAddr, protocol1, protocol2, err)
		return
	}
	if callfunc != nil {
		res := util.PasreFSMessage(recv)
		if res != nil && callfunc != nil {
			// 判断res和pb1是否相同类型
			reflectType := reflect.TypeOf(callfunc)
			pb1Type := reflectType.In(0)
			if reflect.TypeOf(res) == pb1Type {
				callfunc(res)
			} else {
				vars.Error("RPC客户端回调类型不匹配[%s] 期望:%v 实际:%v", c.fullAddr, pb1Type, reflect.TypeOf(res))
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
	//创建一个带计时器的客户端指针
	c, err := localtimer.NewTimer(1000, -1, &RpcClient{})
	if err != nil {
		vars.Error("创建RPC客户端失败[%s:%d]: %v", addr, port, err)
		return nil
	}
	client := c.(*RpcClient)
	client.addr = addr
	client.port = port
	client.fullAddr = addr + ":" + strconv.Itoa(port)
	client.serverName = servername

	conn, err := newClient(client.fullAddr)
	if err == nil {
		client.conn.Store(conn)
		client.connStatus.Store(true)
		vars.Info("RPC客户端连接成功[%s]", client.fullAddr)
	} else { //一直保持监听保证连接
		vars.Error("RPC客户端初始连接失败[%s]: %v", client.fullAddr, err)
		client.conn.Store(nil)
		client.connStatus.Store(false)
		localtimer.AddTimer(client)
	}

	rpcClient_.Store(servername, client)
	return client
}
