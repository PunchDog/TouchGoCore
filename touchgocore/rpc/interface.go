package rpc

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
	port      int
	data      interface{} //数据
}

type SQRegister struct {
	Ip         string
	Port       int
	ServerType string //对方的类型
}

type ReqBuffer struct {
	port    int         //要转发的端口号
	ReqData interface{} //数据
}

type RetBuffer struct {
	port    int         //要转发的端口号
	RetData interface{} //数据
}
