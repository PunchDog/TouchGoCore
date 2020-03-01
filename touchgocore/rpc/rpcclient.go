package rpc

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/syncmap"
	"github.com/TouchGoCore/touchgocore/util"
	"net/rpc"
	"strconv"
	"time"
)

type Client struct {
	client     *rpc.Client
	serverType string
	keyValue   map[string]*string
	registerCh chan bool //注册阻断
}

//所有的连接信息(map[port](client))
var rpcClientMap_ *syncmap.Map = &syncmap.Map{}

//发送消息(负载低服务器)
func SendMsgByBurdenMin(protocol1 int, protocol2 int, req interface{}, res interface{}) (port1 int, err error) {
	szkey := fmt.Sprintf("%d-%d", protocol1, protocol2)
	ip, port, types, keyValue := getConnectInfo(szkey)
	if port == 0 {
		err = &util.Error{ErrMsg: "查询Bus映射端口出错，没有对应的bus数据"}
		return
	}
	var client *Client = nil
	if c, ok := rpcClientMap_.Load(port); !ok {
		client = &Client{serverType: types, keyValue: make(map[string]*string), registerCh: make(chan bool)}
		client.client, err = rpc.Dial("tcp", ip+":"+strconv.FormatInt(int64(port), 10))
		if err != nil {
			return
		}
		rpcClientMap_.Store(port, client) //先放入临时空间
		go func() {                       //创建个定时器，超时删除连接
			time.Sleep(time.Second * 2)
			client.registerCh <- false
		}()
		b := <-client.registerCh
		if !b {
			rpcClientMap_.Delete(port)
			return 0, &util.Error{ErrMsg: "注册超时，创建连接失败"}
		}
	} else {
		client = c.(*Client)
	}
	client.keyValue[szkey] = new(string)
	*client.keyValue[szkey] = keyValue
	port1 = port

	err = send(protocol1, protocol2, req, res, client)
	return
}

//定向发送消息
func SendMsg(port int, protocol1 int, protocol2 int, req interface{}, res interface{}) (port1 int, err error) {
	if c, ok := rpcClientMap_.Load(port); ok {
		client := c.(*Client)
		port1 = port
		szkey := fmt.Sprintf("%d-%d", protocol1, protocol2)
		if client.keyValue[szkey] == nil {
			client.keyValue[szkey] = new(string)
			*client.keyValue[szkey] = getMsgKey(szkey)
		}
		err = send(protocol1, protocol2, req, res, client)
		return
	}
	return SendMsgByBurdenMin(protocol1, protocol2, req, res)
}

func send(protocol1 int, protocol2 int, req interface{}, res interface{}, client *Client) (err error) {
	szkey := fmt.Sprintf("%d-%d", protocol1, protocol2)
	if protocol1 != rpcCfg_.Protocol1 {
		//发送给其他主要服务器的消息
		switch client.serverType {
		case "exec":
			call := client.client.Go(*client.keyValue[szkey], req, res, nil)
			call = <-call.Done
			err = call.Error
			return
		case "dll":
			proxyreq := &sqsproxy{
				protocol1: protocol1,
				protocol2: protocol2,
				data:      req,
			}
			call := client.client.Go("defaultMsg.Proxy", proxyreq, res, nil)
			call = <-call.Done
			err = call.Error
			return
		}
	} else {
		//发给功能插件服务器的消息
		switch client.serverType {
		case "exec":
			proxyreq := &sqsproxy{
				protocol1: protocol1,
				protocol2: protocol2,
				data:      req,
			}
			call := client.client.Go("defaultMsg.Proxy", proxyreq, res, nil)
			call = <-call.Done
			err = call.Error
			return
		case "dll":
			call := client.client.Go(*client.keyValue[szkey], req, res, nil)
			call = <-call.Done
			err = call.Error
			return
		}
	}
	return
}
