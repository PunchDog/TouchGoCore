package mapmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	lua "touchgocore/golua"
	"touchgocore/syncmap"

	"touchgocore/corectx"
	"touchgocore/util"

	"touchgocore/vars"
)

// 地图数据 id/map
var _maplist *syncmap.Map[uint32, *Map]

func init() {
	_maplist = syncmap.NewMap[uint32, *Map]()
}

// 地图坐标点类
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

// 地图类
type Map struct {
	//地图ID
	MapId uint32 `json:"mapid"`
	//地图坐标信息
	Node [][]*MapNode `json:"node"`
	// NPC列表
	Npc []*Npc
	mu  sync.Mutex
}

func (this *Map) Load(path string) error {
	file, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取启动配置出错: %w", err)
	}
	err = json.Unmarshal(file, &this)
	if err != nil {
		return fmt.Errorf("解析配置出错 %s: %w", path, err)
	}
	if _, ok := _maplist.LoadOrStore(this.MapId, this); ok {
		return fmt.Errorf("加载地图配置出错:%s:已经有相同ID的地图了", path)
	}

	vars.Info("加载地图 %s 成功!", path)
	return nil
}

func RunMap(ctx context.Context) error {
	cfg := corectx.CfgFrom(ctx)
	if cfg == nil || cfg.MapPath == "off" || cfg.MapPath == "" {
		vars.Info("不启动地图功能")
		return nil
	}

	pathlist := util.GetPathFile(cfg.MapPath, nil)

	var errs []error
	for _, filepath := range pathlist {
		maps := &Map{}
		if err := maps.Load(filepath); err != nil {
			vars.Error("加载地图失败: %v", err)
			errs = append(errs, err)
		}
	}

	lua.RegisterLuaClass(&Npc{})
	vars.Info("读取地图完成!")
	if len(errs) > 0 {
		return fmt.Errorf("部分地图加载失败: %v", errs)
	}
	return nil
}

func GetMap(id uint32) (*Map, bool) {
	return _maplist.Load(id)
}

func StopMap(_ context.Context) {
	if _maplist != nil {
		_maplist.Clear()
	}
}
