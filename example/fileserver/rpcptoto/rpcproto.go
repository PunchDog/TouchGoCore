package rpcptoto

import (
	"github.com/TouchGoCore/touchgocore/rpc"
	"github.com/TouchGoCore/touchgocore/vars"
)

type RegisterFunc struct {
	p TestProto
}

//二级协议对应rpc传输函数
func (this *RegisterFunc) MsgMap() map[int]string {
	return map[int]string{
		1: "TestProto.Test",
	}
}

//rpc协议具体实现的类
func (this *RegisterFunc) RpcFunc() interface{} {
	return &this.p
}

//rpc协议对应的主协议号
func Protocol1() int {
	return 1
}

type TestProto struct {
}

func (this *TestProto) Test(req int, res *rpc.RetBuffer) error {
	str := "test msg"
	res.RetData = str
	vars.Info("测试收发消息")
	return nil
}
