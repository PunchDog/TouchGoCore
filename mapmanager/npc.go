package mapmanager

import (
	lua "touchgocore/gopherlua"

	"touchgocore/syncmap"
)

// 商品物品类
type ShopItem struct {
	ItemId         int    //物品ID
	CostType       int    //购买扣除
	Cost           int64  //扣除值
	MaxBuyCnt      int    //最大购买个数
	UpdateTimeType string //刷新类型:day每日;week每周;0不限购
}

type Npc struct {
	lua.ILuaClassObject
	Id        int          //NPCID
	Name      string       //名字
	Shape     string       //形象
	Direction int          //朝向
	AutoMove  bool         //自动行走
	MapPoint  [][2]int     //地图点
	Shop      *syncmap.Map //商店页面
	Dialog    *syncmap.Map //聊天数据
	MapId     int32        //地图ID
}

// 创建一个NPC容器，放入到NPC数据里
func (this *Npc) AddField(id int64, lua *lua.LuaScript) interface{} {
	npc := &Npc{
		Id: int(id),
	}

	npc.SetLuaScript(lua)
	return npc
}
func (this *Npc) SetMapId(mapId int32) {
	this.MapId = mapId
	if m, h := _maplist.Load(mapId); h {
		maps := m.(*Map)
		maps.Npc = append(maps.Npc, this)
	} else {
		panic("NPC配置在了未知的地图上")
	}
}

func (this *Npc) SetName(Name string) {
	this.Name = Name
}
func (this *Npc) SetShape(Shape string) {
	this.Shape = Shape
}
func (this *Npc) SetDirection(Direction int) {
	this.Direction = Direction
}
func (this *Npc) SetAutoMove(AutoMove bool) {
	this.AutoMove = AutoMove
}
func (this *Npc) AddMapPoint(x int, y int) {
	var point [2]int = [2]int{x, y}
	this.MapPoint = append(this.MapPoint, point)
}

func (this *Npc) AddShop(shopid int, itemid int, costtype int, cost int64, maxbuycnt int, updatetimetype string) {
	if this.Shop == nil {
		this.Shop = &syncmap.Map{}
	}
	var list []*ShopItem = nil
	if l, ok := this.Shop.Load(shopid); ok {
		list = l.([]*ShopItem)
	}
	list = append(list, &ShopItem{
		ItemId:         itemid,
		CostType:       costtype,
		Cost:           cost,
		MaxBuyCnt:      maxbuycnt,
		UpdateTimeType: updatetimetype,
	})
	this.Shop.Store(shopid, list)
}
