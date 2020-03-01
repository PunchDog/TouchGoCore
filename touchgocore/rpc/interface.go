package rpc

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/util"
)

//服务器回调注册函数类型
type IRpcCallFunctionClass interface {
	//服务器类型的的函数需要能返回所有的协议号对应RPC的函数的名字:[protocol2]rpcfunctionname
	MsgMap() map[int]string
	//类名字
	ClassName() string
}

type sqsproxy struct {
	protocol1 int
	protocol2 int
	data      interface{} //数据
}

//原始转发协议
type defaultMsg struct {
}

//代理转发
func (this *defaultMsg) Proxy(req sqsproxy, res interface{}) error {
	_, err := SendMsg(0, req.protocol1, req.protocol2, req.data, res)
	return err
}

//客户端创建连接后服务器取客户端取注册信息
func (this *defaultMsg) Register(port int, res *string) error {
	if c, ok := rpcClientMap_.Load(port); ok {
		*res = fmt.Sprintf("%d-%s", rpcCfg_.ListenPort, rpcCfg_.ServerType)
		client := c.(*Client)
		client.registerCh <- true
		return nil
	}
	return &util.Error{ErrMsg: "注册服务器连接失败"}
}
