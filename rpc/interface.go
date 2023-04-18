package rpc

import "github.com/golang/protobuf/proto"

type RpcRequest struct {
	Websocket      bool          //最后一层消息发向websocket发出去
	ConnUid        int64         //来的时候的连接ID
	RemoteServerId []int         //远端服务器ID，默认为1
	ForwardingIdx  int8          //转发消息的层次
	Params         []interface{} //附加数据，比如玩家ID之类的
	protocol1      int32         //协议号1
	protocol2      int32         //协议号2
	Request        proto.Message //req数据
}

type RpcResponse struct {
	Error    int32         //错误判断
	Response proto.Message //res数据
}

// 注册client用
type registerClient struct {
	ServerId   string
	ServerName string
}

// rpc连接协议
const (
	REGISTER_SERVER = "RpcServer.RegisterClient" //注册连接
	DISPATCH        = "RpcServer.MsgDispatch"    //消息派发处理逻辑
)
