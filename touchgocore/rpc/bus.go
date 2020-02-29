package rpc

import (
	"github.com/TouchGoCore/touchgocore/db"
	"github.com/TouchGoCore/touchgocore/jsonthr"
	"strconv"
)

//连接信息
type connectData struct {
	Ip         string
	Port       int
	Num        int
	ServerName string
}

//生成通道数据
func createBus(maps map[string]string) {
	//1、先把映射关系存到redis里
	redis, err := db.NewRedis(&rpcCfg_.RedisConfig)
	if err != nil {
		panic(err)
	}

	szbusid := strconv.FormatInt(int64(rpcCfg_.BusId), 10)
	var maps1 []string = nil
	//先映射函数找busid
	for szkey, val := range maps {
		redis.HSet(szbusid, szkey, val)
		redis.HSet(szkey, "BusId", szbusid)
		redis.HSet(szkey, "ProtocolServerType", rpcCfg_.ServerType)
		redis.HSet(szkey, "ServerName", serverName_)
		maps1 = append(maps1, szkey)
	}
	if d, err := jsonthr.Json.Marshal(maps1); err == nil {
		redis.Set(szbusid, string(d), 0)
	}

	//映射连接信息
	list := map[int]*connectData{}
	if cmd := redis.HGet(szbusid, rpcCfg_.ServerType); cmd.Err() == nil {
		jsonthr.Json.Unmarshal([]byte(cmd.Val()), list)
	}

	list[rpcCfg_.ListenPort] = &connectData{
		Ip:         rpcCfg_.Ip,
		Port:       rpcCfg_.ListenPort,
		Num:        0,
		ServerName: serverName_,
	}
	d, _ := jsonthr.Json.Marshal(list)
	redis.HSet(szbusid, rpcCfg_.ServerType, string(d))
}

//删除通道数据
func removeBus() {
	//1、先把映射关系存到redis里
	redis, err := db.NewRedis(&rpcCfg_.RedisConfig)
	if err != nil {
		panic(err)
	}

	szbusid := strconv.FormatInt(int64(rpcCfg_.BusId), 10)

	list := map[int]*connectData{}
	if cmd := redis.HGet(szbusid, rpcCfg_.ServerType); cmd.Err() == nil {
		jsonthr.Json.Unmarshal([]byte(cmd.Val()), list)
	} else {
		panic("未正确取BusId对应的端口信息")
	}
	delete(list, rpcCfg_.ListenPort)
	if len(list) == 0 {
		if cmd := redis.Get(szbusid); cmd.Err() == nil {
			var maps1 []string = nil
			jsonthr.Json.Unmarshal([]byte(cmd.Val()), maps1)
			for _, szkey := range maps1 {
				redis.Del(szkey)
			}
			redis.Del(szbusid)
		} else {
			panic("未正确读取BusId对应协议列表:" + cmd.Err().Error())
		}
	} else {
		d, _ := jsonthr.Json.Marshal(list)
		redis.HSet(szbusid, rpcCfg_.ServerType, string(d))
	}
}

//获取一个有效的人少的端口(协议号/当前服务器的BusId)
func getConnectInfo(szKey string) (ip string, port int, sztype string, keyValue string) {
	//1、先把映射关系存到redis里
	redis, err := db.NewRedis(&rpcCfg_.RedisConfig)
	if err != nil {
		panic(err)
	}

	if cmd := redis.Get(szKey); cmd.Err() == nil {
		szbusid := cmd.Val()
		//查询exec和dll列表
		//busid不同，取exec列表ip:port,真实服务器类型；budis相同，取与自身真实服务器类型相反的类型列表
		list := map[int]*connectData{}
		if szbusid != strconv.FormatInt((int64(rpcCfg_.BusId)), 10) || rpcCfg_.ServerType == "dll" {
			if jsonthr.Json.Unmarshal([]byte(redis.HGet(szbusid, "exec").Val()), list) == nil {
				num := 0
				//查询一个合适的连接信息
				for _, data := range list {
					if port == 0 || num < data.Num {
						ip = data.Ip
						port = data.Port
						num = data.Num
					}
				}
				//赋予服务器类型
				keyValue = redis.HGet(szbusid, szKey).Val()
				sztype = redis.HGet(szKey, "ProtocolServerType").Val()
				//如果发送消息的是插件服务器，在查询出来的结果后，需要取反
				if rpcCfg_.ServerType == "dll" {
					if sztype == "dll" {
						sztype = "exec"
					} else {
						sztype = "dll"
					}
				}
				return
			}
		} else {
			if jsonthr.Json.Unmarshal([]byte(redis.HGet(szbusid, "dll").Val()), list) == nil {
				num := 0
				keyName := redis.HGet(szKey, "ServerName").Val()
				//查询一个合适的连接信息
				for _, data := range list {
					if (port == 0 || num < data.Num) && keyName == data.ServerName {
						ip = data.Ip
						port = data.Port
						num = data.Num
					}
				}

				keyValue = redis.HGet(szbusid, szKey).Val()
				sztype = redis.HGet(szKey, "ProtocolServerType").Val()
				return
			}
		}
	} else {
		panic("未正确读取协议对应BusId:" + cmd.Err().Error())
	}
	return
}
