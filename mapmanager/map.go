package mapmanager

import (
	"io/ioutil"
	lua "touchgocore/gopherlua"
	"touchgocore/syncmap"

	"touchgocore/util"

	"touchgocore/config"
	"touchgocore/jsonthr"
	"touchgocore/vars"
)

//地图数据 id/map
var MapList_ *syncmap.Map = &syncmap.Map{}

//地图坐标点类
type MapNode struct {
	//是否阻挡
	IsBlock bool `json:"isblock"`
	//绘制ID
	ViewID int32 `json:"viewid"`
	//是否是绘制物左下角起始地
	IsViewInit bool `json:"isviewinit"`
	//怪池ID
	MonsterPoolId int `json:"monsterpoolid"`
	//传送地 mapid,x,y
	SendMapData []int `json:"sendmapdata"`
}

//地图类
type Map struct {
	//地图ID
	MapId int `json:"mapid"`
	//地图坐标信息
	Node [][]*MapNode `json:"node"`
}

func (this *Map) Load(path string) {
	file, err := ioutil.ReadFile(path)
	if err != nil {
		panic("读取启动配置出错:" + err.Error())
	}
	err = jsonthr.Json.Unmarshal(file, &this)
	if err != nil {
		panic("解析配置出错:" + path + ":" + err.Error())
	}
	if _, ok := MapList_.LoadOrStore(this.MapId, this); ok {
		panic("加载地图配置出错:" + path + ":已经有相同ID的地图了")
	}

	vars.Info("加载地图 %s 成功!", path)
}

func Run() {
	if config.Cfg_.MapPath == "off" {
		return
	}

	pathlist := util.GetPathFile(config.Cfg_.MapPath, nil)

	//加载所有地图
	for _, filepath := range pathlist {
		maps := &Map{}
		maps.Load(filepath)
	}

	//创建lua NPC类
	lua.RegisterLuaClass(&Npc{})

	vars.Info("读取地图完成!")
}
