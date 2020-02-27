package rpc

//注册rpc功能接口
type IRpcClass interface {
	Run()
}

//服务器回调注册函数类型
type IRpcCallFunctionClass interface {
	//服务器类型的的函数需要能返回所有的协议号对应RPC的函数的名字:[proto.query]rpcfunctionname
	MsgMap() map[int]string
	//类名字
	ClassName() string
}
