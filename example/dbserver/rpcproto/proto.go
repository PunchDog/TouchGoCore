package rpcproto

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/rpc"
)

type RegisterFunc struct {
	p DBProto
}

//二级协议对应rpc传输函数
func (this *RegisterFunc) MsgMap() map[int]string {
	return map[int]string{
		1: "DBProto.Query",
		2: "DBProto.Write",
	}
}

//rpc协议具体实现的类
func (this *RegisterFunc) RpcFunc() interface{} {
	return &this.p
}

//主协议号
func (this *RegisterFunc) Protocol1() int {
	return 1
}

type DBProto struct {
}

//查询数据
func (this *DBProto) Query(req rpc.ReqBuffer, res *rpc.RetBuffer) error {
	return nil
}

//写数据
func (this *DBProto) Write(req rpc.ReqBuffer, res *rpc.RetBuffer) error {
	return nil
}
