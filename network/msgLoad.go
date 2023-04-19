package network

import (
	"strings"
	"touchgocore/config"
	"touchgocore/ini"
	"touchgocore/syncmap"
	"touchgocore/util"
)

/*
本文件是用来加载消息协议配置的
*/

var (
	protoToMetodName  *syncmap.Map
	protoToServerList *syncmap.Map
)

func init() {
	loadMsg()
}

// /*
// 注册协议链,即协议转发几个服务器((Protocol1-Protocol2)=>[]string{server1,server2,...})，有几个服务器要转发，就要填几个
// 其实就是设置协议在哪个服务器里执行
// */
// func RegiserProtocolServerNameList(mp *syncmap.Map) {
// 	//循环插入注册好的跳转结构
// 	mp.Range(func(k, v interface{}) bool {
// 		list := v.([]string)
// 		for _, str := range list {
// 			redis_.Get().LPush(k.(string), str)
// 		}
// 		return true
// 	})
// }

func loadMsg() {
	protoToMetodName = new(syncmap.Map)
	protoToServerList = new(syncmap.Map)
	msgPath := config.GetBasePath() + "conf/MSG_CONST.ini"
	msgToServerListPath := config.GetBasePath() + "conf/MSG_JUMP_SERVER_LIST_CONST.ini"

	//读取INI
	metodNameToProtocol := make(map[string]int32)
	//协议对应函数名转换
	if p, err := ini.Load(msgPath); err == nil {
		if mp := p.GetAll("MSG_NAME_TO_PROTOCOL"); mp != nil {
			for k, v := range mp {
				protoToMetodName.Store(util.Sto32(v), k)
				metodNameToProtocol[k] = util.Sto32(v)
			}
		}
	}

	//协议在服务器之间跳转的顺序
	if p, err := ini.Load(msgToServerListPath); err == nil {
		if mp := p.GetAll("MSG_NAME_TO_JUMP_SERVER_LIST"); mp != nil {
			for k, v := range mp {
				protocol, h := metodNameToProtocol[k]
				if !h {
					continue
				}
				serverNameList := strings.Split(v, ",")
				protoToServerList.Store(protocol, serverNameList)
			}
		}
	}
}

// 获取协议对应的proto协议函数名
func GetProtoName(protocol1, protocol2 int32) string {
	if d, ok := protoToMetodName.Load(protocol1); ok {
		return d.(string)
	}
	return ""
}

// 获取协议对应的转发服务器顺序
func ServerMsgToServerName(protocol1, protocol2 int32, ForwardingIdx int8) string {
	if l, ok := protoToServerList.Load(protocol1); ok {
		serverNameList := l.([]string)
		//如果是-1，就获取最后一个键值
		if ForwardingIdx == -1 {
			ForwardingIdx = int8(len(serverNameList) - 1)
		}

		if ForwardingIdx >= 0 && len(serverNameList) > 0 {
			return serverNameList[ForwardingIdx]
		}
	}

	return ""
}
