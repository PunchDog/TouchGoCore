package mapmanager

import (
	"fmt"

	lua "touchgocore/golua"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
)

// ============================================================================
// NPC系统 - TouchGoCore 游戏服务器
// ============================================================================
// 对话类型枚举
const (
	DialogTypeNormal  = "normal"  // 普通说话
	DialogTypeDialog  = "dialog"  // 对话框(带选项)
	DialogTypeShop    = "shop"    // 商店
	DialogTypePrivate = "private" // 密语/关键字触发
	DialogTypeSend    = "send"    // 传送
	DialogTypeClose   = "close"   // 关闭对话框
	DialogTypeTask    = "task"    // 任务
)

// 商店选择模式
const (
	ShopModeDirect = "direct" // 直接打开单个商店
	ShopModeRand   = "rand"   // 随机选择一个商店
	ShopModeFixed  = "fixed"  // 显示多个商店按钮供选择
)

// 限购刷新类型
const (
	RefreshTypeDay   = "day"  // 每日刷新
	RefreshTypeWeek  = "week" // 每周刷新
	RefreshTypeNever = "0"    // 不刷新(不限购)
)

// ============================================================================
// 数据结构定义
// ============================================================================

// 对话选项 - dialog类型使用
type DialogOption struct {
	PreDialogID    int `json:"pre_dialog_id"`    // 前置对话ID
	PreDialogCnt   int `json:"pre_dialog_cnt"`   // 前置对话执行次数
	TargetDialogID int `json:"target_dialog_id"` // 目标对话ID
}

// 对话项 - NPC交互行为配置
type DialogItem struct {
	ID     int    `json:"id"`     // 对话ID
	Text   string `json:"text"`   // 对话文本
	Type   string `json:"type"`   // 对话类型
	Param1 any    `json:"param1"` // 参数1(可选)
	Param2 any    `json:"param2"` // 参数2(可选)
	Param3 any    `json:"param3"` // 参数3(可选)
}

// 商品物品类
type ShopItem struct {
	ItemID         int    `json:"item_id"`          // 物品ID
	CostType       int    `json:"cost_type"`        // 购买扣除货币类型
	Cost           int64  `json:"cost"`             // 价格
	MaxBuyCnt      int    `json:"max_buy_cnt"`      // 最大购买个数
	UpdateTimeType string `json:"update_time_type"` // 刷新类型:day每日;week每周;0不限购
}

// NPC类 - 游戏中的非玩家角色
type Npc struct {
	lua.ILuaClassObject

	// 基础属性
	ID        uint32 `json:"id"`        // NPC唯一标识
	Name      string `json:"name"`      // NPC名称
	Shape     string `json:"shape"`     // 外观资源ID
	Direction int8   `json:"direction"` // 朝向(0-7,代表8个方向)
	AutoMove  bool   `json:"auto_move"` // 是否自动行走

	// 位置与移动
	MapID    uint32     `json:"map_id"`     // 所属地图ID
	MapPoint [][2]int16 `json:"map_points"` // 巡逻路径点

	// 交互数据
	Shop   *syncmap.Map[int, []*ShopItem] `json:"-"` // 商店数据 (shopId => []*ShopItem)
	Dialog *syncmap.Map[int, *DialogItem] `json:"-"` // 对话数据 (dialogId => *DialogItem)
}

// ============================================================================
// 构造函数与初始化
// ============================================================================

// Init 创建NPC实例
// @param id NPC唯一标识
// @param lua Lua脚本实例
func (n *Npc) Init(id int64, luascript *lua.LuaScript) {
	n.ID = uint32(id)
	n.Shop = syncmap.NewMap[int, []*ShopItem]()
	n.Dialog = syncmap.NewMap[int, *DialogItem]()
}

// ============================================================================
// 属性设置方法
// ============================================================================

// SetName 设置NPC名称
func (n *Npc) SetName(name string) {
	if name == "" {
		vars.Error("[NPC] 名字无效")
		name = "未知NPC"
	}
	n.Name = name
}

// SetShape 设置NPC外观
func (n *Npc) SetShape(shape string) {
	n.Shape = shape
}

// SetDirection 设置NPC朝向
// @param direction 朝向值(0-7)
func (n *Npc) SetDirection(direction int8) {
	if direction < 0 || direction > 7 {
		vars.Error("[NPC] 朝向值无效: %d, 期望范围: 0-7", direction)
		direction = 0
	}
	n.Direction = direction
}

// SetAutoMove 设置是否自动移动
func (n *Npc) SetAutoMove(autoMove bool) {
	n.AutoMove = autoMove
}

// SetMapId 设置所属地图并注册NPC
// @param mapId 地图ID
func (n *Npc) SetMapId(mapId uint32) {
	n.MapID = mapId
	maps, ok := _maplist.Load(mapId)
	if !ok {
		panic(fmt.Sprintf("[NPC] 配置在未知的地图上: mapId=%d, npcId=%d", mapId, n.ID))
	}
	maps.Npc = append(maps.Npc, n)
}

// ============================================================================
// 路径点操作
// ============================================================================

// AddMapPoint 添加巡逻路径点
// @param x X坐标
// @param y Y坐标
func (n *Npc) AddMapPoint(x, y int16) {
	point := [2]int16{x, y}
	n.MapPoint = append(n.MapPoint, point)
}

// GetPathPoints 获取所有路径点
func (n *Npc) GetPathPoints() [][2]int16 {
	return n.MapPoint
}

// ============================================================================
// 商店操作
// ============================================================================

// AddShop 添加商店商品
// @param shopId     商店ID
// @param itemId     物品ID
// @param costType   货币类型
// @param cost       价格
// @param maxBuyCnt  最大购买数量
// @param refreshType 刷新类型(day/week/0)
func (n *Npc) AddShop(shopId, itemId, costType, maxBuyCnt int, cost int64, refreshType string) {
	// 获取或创建商店商品列表
	var list []*ShopItem
	if l, ok := n.Shop.Load(shopId); ok {
		list = l
	} else {
		list = make([]*ShopItem, 0, 4)
	}

	// 添加商品
	item := &ShopItem{
		ItemID:         itemId,
		CostType:       costType,
		Cost:           cost,
		MaxBuyCnt:      maxBuyCnt,
		UpdateTimeType: refreshType,
	}
	list = append(list, item)
	n.Shop.Store(shopId, list)
}

// GetShop 获取商店商品列表
// @param shopId 商店ID
// @return 商品列表
func (n *Npc) GetShop(shopId int) []*ShopItem {
	if l, ok := n.Shop.Load(shopId); ok {
		return l
	}
	return nil
}

// HasShop 检查NPC是否有商店
func (n *Npc) HasShop() bool {
	if n.Shop == nil {
		return false
	}
	count := 0
	n.Shop.Range(func(key int, value []*ShopItem) bool {
		count++
		return false
	})
	return count > 0
}

// ============================================================================
// 对话操作
// ============================================================================

// AddDialog 添加对话项
// @param dialogId 对话ID
// @param text     对话文本
// @param dialogType 对话类型
// @param params   可选参数
func (n *Npc) AddDialog(dialogId int, text, dialogType string, params ...any) {
	item := &DialogItem{
		ID:   dialogId,
		Text: text,
		Type: dialogType,
	}

	// 设置可选参数
	switch len(params) {
	case 3:
		item.Param3 = params[2]
		fallthrough
	case 2:
		item.Param2 = params[1]
		fallthrough
	case 1:
		item.Param1 = params[0]
	}

	n.Dialog.Store(dialogId, item)
}

// GetDialog 获取对话项
// @param dialogId 对话ID
// @return 对话项, 是否存在
func (n *Npc) GetDialog(dialogId int) (*DialogItem, bool) {
	if n.Dialog == nil {
		return nil, false
	}
	item, ok := n.Dialog.Load(dialogId)
	if !ok {
		return nil, false
	}
	return item, true
}

// GetDialogText 获取对话文本（支持随机文本）
// 当对话类型为 normal 且 param1 为字符串数组时，每次调用随机返回一条文本
// 否则返回固定 Text 字段
func (n *Npc) GetDialogText(dialogId int) string {
	item, ok := n.GetDialog(dialogId)
	if !ok {
		return ""
	}
	return item.GetDisplayText()
}

// GetDisplayText 获取对话项的显示文本
// 支持 NORMAL 类型的随机文本：当 Param1 为 []string 时随机选一条
func (d *DialogItem) GetDisplayText() string {
	// 仅 NORMAL 类型支持随机文本
	if d.Type == DialogTypeNormal {
		if texts, ok := d.Param1.([]string); ok && len(texts) > 0 {
			return texts[util.RandInt(int64(len(texts)))]
		}
		// Lua 传递的可能是 []any 类型
		if texts, ok := d.Param1.([]any); ok && len(texts) > 0 {
			if s, ok := texts[util.RandInt(int64(len(texts)))].(string); ok {
				return s
			}
		}
	}
	return d.Text
}

// GetDefaultDialog 获取默认对话项(对话ID最小的)
func (n *Npc) GetDefaultDialog() *DialogItem {
	if n.Dialog == nil {
		return nil
	}

	var defaultDialog *DialogItem
	minId := int(^uint(0) >> 1) // 最大整数

	n.Dialog.Range(func(id int, value *DialogItem) bool {
		if id < minId {
			minId = id
			defaultDialog = value
		}
		return true
	})

	return defaultDialog
}

// GetDialogsByType 获取指定类型的所有对话
// @param dialogType 对话类型
// @return 对话列表
func (n *Npc) GetDialogsByType(dialogType string) []*DialogItem {
	var result []*DialogItem

	n.Dialog.Range(func(key int, item *DialogItem) bool {
		if item.Type == dialogType {
			result = append(result, item)
		}
		return true
	})

	return result
}

// HasDialog 检查是否有对话配置
func (n *Npc) HasDialog() bool {
	if n.Dialog == nil {
		return false
	}
	count := 0
	n.Dialog.Range(func(key int, value *DialogItem) bool {
		count++
		return false
	})
	return count > 0
}

// ============================================================================
// 验证与辅助方法
// ============================================================================

// Validate 验证NPC配置完整性
// @return 验证错误列表
func (n *Npc) Validate() []string {
	var errors []string

	if n.ID <= 0 {
		errors = append(errors, "NPC ID必须大于0")
	}
	if n.Name == "" {
		errors = append(errors, "NPC名称不能为空")
	}
	if n.Shape == "" {
		errors = append(errors, "NPC外观不能为空")
	}
	if n.MapID <= 0 {
		errors = append(errors, "NPC所属地图未设置")
	}
	if n.AutoMove && len(n.MapPoint) == 0 {
		errors = append(errors, "自动移动NPC必须配置路径点")
	}

	return errors
}

// String 返回NPC的字符串表示
func (n *Npc) String() string {
	return fmt.Sprintf("Npc[id=%d, name=%s, shape=%s, map=%d]", n.ID, n.Name, n.Shape, n.MapID)
}
