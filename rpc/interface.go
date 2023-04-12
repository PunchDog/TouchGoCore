package rpc

import "github.com/golang/protobuf/proto"

type RpcRequest struct {
	ConnType  string        //连接类型，主要是用于区分是websocket还是rpc
	Ntf       bool          //广播消息
	ConnUid   int64         //来的时候的连接ID
	Params    []interface{} //附加数据，比如玩家ID之类的
	protocol1 int32         //协议号
	protocol2 int32         //协议号
	Request   proto.Message //req数据
}

type RpcResponse struct {
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
