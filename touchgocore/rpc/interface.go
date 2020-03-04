package rpc

import (
	"github.com/TouchGoCore/touchgocore/vars"
	"net/rpc"
	"strconv"
)

//服务器回调注册函数类型
type IRpcCallFunctionClass interface {
	//服务器类型的的函数需要能返回所有的协议号对应RPC的函数的名字:[protocol2:int]rpcfunctionname
	MsgMap() map[int]string
	//二级协议实现类
	RpcFunc() interface{}
	//一级协议
	Protocol1() int
}

type SQProxy struct {
	protocol1 int
	protocol2 int
	data      interface{} //数据
}

type SQRegister struct {
	Ip         string
	Port       int
	ServerType string //对方的类型
}

type RetBuffer struct {
	RetData interface{} //数据
}

//原始转发协议
type DefaultMsg struct {
}

//代理转发
func (this *DefaultMsg) Proxy(req SQProxy, res *RetBuffer) error {
	_, err := SendMsg(0, req.protocol1, req.protocol2, req.data, res)
	return err
}

//客户端创建连接后服务器取客户端取注册信息
func (this *DefaultMsg) Register(reg SQRegister, res *string) (err error) {
	//收到对方的消息，创建连接对方的指针
	client := &Client{serverType: reg.ServerType, keyValue: make(map[string]*string)}
	client.client, err = rpc.Dial("tcp", reg.Ip+":"+strconv.FormatInt(int64(reg.Port), 10))
	if err != nil {
		return
	}
	rpcClientMap_.Store(reg.Port, client) //注册成功的，放入map
	*res = "OK"
	vars.Info("注册客户端对口连接成功", reg)
	return nil
}
