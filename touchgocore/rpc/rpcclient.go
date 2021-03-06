package rpc

import (
	"fmt"
	"net/rpc"
	"strconv"

	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/syncmap"
	"github.com/PunchDog/TouchGoCore/touchgocore/util"
)

type Client struct {
	client     *rpc.Client
	serverType string
	keyValue   map[string]*string
}

//所有的连接信息(map[port](client))
var rpcClientMap_ *syncmap.Map = &syncmap.Map{}

//发送消息(负载低服务器)
func SendMsgByBurdenMin(protocol1 int, protocol2 int, req interface{}, res interface{}) (port1 int, err error) {
	szkey := fmt.Sprintf("%d-%d-%s", protocol1, protocol2, config.Cfg_.TeamId)
	conndata, types, keyValue := getConnectInfo(szkey)
	if conndata == nil {
		err = &util.Error{ErrMsg: "查询Bus映射端口出错，没有对应的bus数据"}
		return
	}
	var client *Client = nil
	if c, ok := rpcClientMap_.Load(conndata.Port); !ok {
		client = &Client{serverType: types, keyValue: make(map[string]*string)}
		client.client, err = rpc.Dial("tcp", conndata.Ip+":"+strconv.FormatInt(int64(conndata.Port), 10))
		if err != nil {
			return
		}
		ret := new(string)
		if err := client.client.Call("DefaultMsg.Register", SQRegister{Ip: config.Cfg_.Ip, Port: int(httpserver_.port), ServerType: config.Cfg_.ServerType}, ret); err != nil || *ret != "OK" {
			client.client.Close()
			return 0, &util.Error{ErrMsg: "注册超时，创建连接失败"}
		}
		rpcClientMap_.Store(conndata.Port, client) //注册成功的，放入map
	} else {
		client = c.(*Client)
	}
	client.keyValue[szkey] = new(string)
	*client.keyValue[szkey] = keyValue
	port1 = conndata.Port

	err = send(protocol1, protocol2, req, res, client)
	return
}

//定向发送消息
func SendMsg(port int, protocol1 int, protocol2 int, req interface{}, res interface{}) (port1 int, err error) {
	if c, ok := rpcClientMap_.Load(port); ok {
		client := c.(*Client)
		port1 = port
		szkey := fmt.Sprintf("%d-%d-%s", protocol1, protocol2, config.Cfg_.TeamId)
		if client.keyValue[szkey] == nil {
			if msgKey := getMsgKey(szkey); msgKey != "" {
				client.keyValue[szkey] = &msgKey
			} else {
				err = &util.Error{ErrMsg: "获取rpc函数错误：" + szkey}
				return
			}
		}
		err = send(protocol1, protocol2, req, res, client)
		return
	}
	return SendMsgByBurdenMin(protocol1, protocol2, req, res)
}

func send(protocol1 int, protocol2 int, req interface{}, res interface{}, client *Client) (err error) {
	szkey := fmt.Sprintf("%d-%d-%s", protocol1, protocol2, config.Cfg_.TeamId)
	redis, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic(err)
	}

	busid := redis.Get().HGet(szkey, "BusId").Val()
	if busid != config.Cfg_.BusId {
		//发送给其他主要服务器的消息
		switch client.serverType {
		case "exec":
			call := client.client.Go(*client.keyValue[szkey], req, res, nil)
			call = <-call.Done
			err = call.Error
			return
		case "dll":
			proxyreq := &SQProxy{
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
			proxyreq := &SQProxy{
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
