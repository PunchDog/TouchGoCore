package rpc

type ReqBuffer struct {
	Ip      string      //IP
	Port    int         //要转发的端口号
	Mark    interface{} //标记位，用于服务器内操作，通常是玩家ID
	Request interface{} //数据
}

type ResBuffer struct {
	Port     int         //要转发的端口号
	Response interface{} //数据
}
