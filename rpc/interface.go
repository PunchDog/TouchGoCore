package rpc

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
	Ip      string      //IP
	Port    int         //要转发的端口号
	Mark    interface{} //标记位，用于服务器内操作，通常是玩家ID
	ReqData interface{} //数据
}

type ResBuffer struct {
	Port    int         //要转发的端口号
	RetData interface{} //数据
}
