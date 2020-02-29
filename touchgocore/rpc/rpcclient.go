package rpc

import (
	"fmt"
	"github.com/TouchGoCore/touchgocore/syncmap"
	"github.com/TouchGoCore/touchgocore/util"
	"net/rpc"
	"strconv"
)

type Client struct {
	client     *rpc.Client
	serverType string
	keyValue   string
}

//所有的连接信息(map[szkey](client))
var rpcClientMap_ *syncmap.Map = &syncmap.Map{}

//发送消息
func SendMsg(protocol1 int, protocol2 int, req interface{}, res interface{}) (err error) {
	szkey := fmt.Sprintf("%d-%d", protocol1, protocol2)
	var client *Client = nil
	if c, ok := rpcClientMap_.Load(szkey); ok {
		client = c.(*Client)
	} else {
		ip, port, types, keyValue := getConnectInfo(szkey)
		if port == 0 {
			err = &util.Error{ErrMsg: "查询Bus映射端口出错，没有对应的bus数据"}
			return
		}
		client = &Client{serverType: types, keyValue: keyValue}
		client.client, err = rpc.Dial("tcp", ip+":"+strconv.FormatInt(int64(port), 10))
		if err != nil {
			return
		}
		rpcClientMap_.Store(szkey, client)
	}

	if protocol1 != rpcCfg_.Protocol1 {
		//发送给其他主要服务器的消息
		switch client.serverType {
		case "exec":
			call := client.client.Go(client.keyValue, req, res, nil)
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
			call := client.client.Go(client.keyValue, req, res, nil)
			call = <-call.Done
			err = call.Error
			return
		}
	}
	return
}
