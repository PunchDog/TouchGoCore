package rpc

import (
	"github.com/PunchDog/TouchGoCore/touchgocore/config"
	"github.com/PunchDog/TouchGoCore/touchgocore/db"
	"github.com/PunchDog/TouchGoCore/touchgocore/jsonthr"
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
	redis, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic(err)
	}

	redis.Lock("initbus")
	defer redis.UnLock("initbus")

	var maps1 []string = nil
	//先映射函数找busid
	for szkey, val := range maps {
		redis.Get().HSet(config.Cfg_.BusId, szkey, val)
		redis.Get().HSet(szkey, "BusId", config.Cfg_.BusId)
		redis.Get().HSet(szkey, "ProtocolServerType", config.Cfg_.ServerType)
		redis.Get().HSet(szkey, "ServerName", config.ServerName_)
		maps1 = append(maps1, szkey)
	}
	//busid对应的消息列表
	if d, err := jsonthr.Json.Marshal(maps1); err == nil {
		redis.Get().HSet(config.Cfg_.BusId, "keylist", string(d))
	}

	//映射连接信息
	list := map[int]*connectData{}
	if cmd := redis.Get().HGet(config.Cfg_.BusId, config.Cfg_.ServerType); cmd.Err() == nil {
		jsonthr.Json.Unmarshal([]byte(cmd.Val()), list)
	}

	list[httpserver_.port] = &connectData{
		Ip:         config.Cfg_.Ip,
		Port:       httpserver_.port,
		Num:        0,
		ServerName: config.ServerName_,
	}
	d, _ := jsonthr.Json.Marshal(list)
	redis.Get().HSet(config.Cfg_.BusId, config.Cfg_.ServerType, string(d))
}

//删除通道数据
func removeBus() {
	//1、先把映射关系存到redis里
	redis, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic(err)
	}

	redis.Lock("removebus")
	defer redis.UnLock("removebus")

	list := map[int]*connectData{}
	if cmd := redis.Get().HGet(config.Cfg_.BusId, config.Cfg_.ServerType); cmd.Err() == nil {
		jsonthr.Json.Unmarshal([]byte(cmd.Val()), list)
	} else {
		panic("未正确取BusId对应的端口信息")
	}
	delete(list, httpserver_.port)
	if len(list) == 0 {
		if cmd := redis.Get().HGet(config.Cfg_.BusId, "keylist"); cmd.Err() == nil {
			var maps1 []string = nil
			jsonthr.Json.Unmarshal([]byte(cmd.Val()), maps1)
			for _, szkey := range maps1 {
				redis.Get().Del(szkey)
			}
			redis.Get().Del(config.Cfg_.BusId)
		} else {
			panic("未正确读取BusId对应协议列表:" + cmd.Err().Error())
		}
	} else {
		d, _ := jsonthr.Json.Marshal(list)
		redis.Get().HSet(config.Cfg_.BusId, config.Cfg_.ServerType, string(d))
	}
}

//获取一个有效的人少的端口(协议号/当前服务器的BusId)
func getConnectInfo(szKey string) (mindata *connectData, sztype string, keyValue string) {
	//1、先把映射关系存到redis里
	redis, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic(err)
	}

	redis.Lock("minbusid")
	defer redis.UnLock("minbusid")

	if cmd := redis.Get().HGet(szKey, "BusId"); cmd.Err() == nil {
		szbusid := cmd.Val()
		//查询exec和dll列表
		//busid不同，取exec列表ip:port,真实服务器类型；budis相同，取与自身真实服务器类型相反的类型列表
		list := map[int]*connectData{}
		types := "dll"
		if szbusid != config.Cfg_.BusId || config.Cfg_.ServerType == "dll" {
			types = "exec"
		}
		if jsonthr.Json.Unmarshal([]byte(redis.Get().HGet(szbusid, types).Val()), &list) == nil {
			mindata = nil
			//查询一个合适的连接信息
			for _, data := range list {
				if mindata == nil || mindata.Num < data.Num {
					mindata = data
				}
			}

			//赋予服务器类型
			keyValue = redis.Get().HGet(szbusid, szKey).Val()
			sztype = redis.Get().HGet(szKey, "ProtocolServerType").Val()

			if szbusid != config.Cfg_.BusId || config.Cfg_.ServerType == "dll" {
				//如果发送消息的是插件服务器，在查询出来的结果后，需要取反
				if config.Cfg_.ServerType == "dll" {
					if sztype == "dll" {
						sztype = "exec"
					} else {
						sztype = "dll"
					}
				}
			}
			return
		} else {
			panic("获取bus数据错误")
		}
	} else {
		panic("未正确读取协议对应BusId:" + cmd.Err().Error())
	}
	return
}

//获取发送协议
func getMsgKey(szKey string) string {
	redis, err := db.NewRedis(config.Cfg_.Redis)
	if err != nil {
		panic(err)
	}

	if cmd := redis.Get().HGet(szKey, "BusId"); cmd.Err() == nil {
		szbusid := cmd.Val()
		return redis.Get().HGet(szbusid, szKey).Val()
	}
	return ""
}
