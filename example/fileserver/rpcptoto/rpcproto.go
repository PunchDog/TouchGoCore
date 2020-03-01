package rpcptoto

import (
	"github.com/TouchGoCore/touchgocore/rpc"
	"github.com/TouchGoCore/touchgocore/vars"
)

type RegisterFunc struct {
	p TestProto
}

func (this *RegisterFunc) MsgMap() map[int]string {
	return map[int]string{
		1: "TestProto.Test",
	}
}

func (this *RegisterFunc) RpcFunc() interface{} {
	return &this.p
}

type TestProto struct {
}

func (this *TestProto) Test(req int, res *rpc.RetBuffer) error {
	str := "test msg"
	res.RetData = str
	vars.Info("测试收发消息")
	return nil
}
