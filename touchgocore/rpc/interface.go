package rpc

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

func (this *defaultMsg) Proxy(req sqsproxy, res interface{}) error {
	return SendMsg(req.protocol1, req.protocol2, req.data, res)
}
