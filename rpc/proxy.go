package rpc

import (
	"net/rpc"
	"strconv"
	"touchgocore/vars"
)

//原始转发协议
type DefaultMsg struct {
}

//代理转发
func (this *DefaultMsg) Proxy(req SQProxy, res *RetBuffer) error {
	port, err := SendMsg(req.port, req.protocol1, req.protocol2, ReqBuffer{ReqData: req.data, Port: httpserver_.port}, res)
	res.Port = port
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
