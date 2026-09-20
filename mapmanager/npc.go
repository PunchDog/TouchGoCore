package mapmanager

import (
	"fmt"
	"sync"

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
//
// 并发约定：Lua 线程写（Set*/Add*）、游戏逻辑线程读（Get*/Has*）。
// 所有可变状态一律经 n.mu 保护并通过方法访问；直接读写字段不参与加锁。
type Npc struct {
	lua.ILuaClassObject

	mu sync.Mutex

	// attachedTo 记录已挂载的地图，用于幂等 SetMapId
	attachedTo *Map

	// 基础属性
	ID        uint32 `json:"id"`        // NPC唯一标识（业务 ID，由 SetId 或配置给出）
	Name      string `json:"name"`      // NPC名称
	Shape     string `json:"shape"`     // 外观资源ID
	Direction int8   `json:"direction"` // 朝向(0-7,代表8个方向)

	// 位置与移动
	MapID    uint32     `json:"map_id"`     // 所属地图ID
	MapPoint [][3]int32 `json:"map_points"` // 巡逻路径点（并发下请用 AddMapPoint/GetPathPoints）

	// 交互数据
	Shop   *syncmap.Map[int, []*ShopItem] `json:"-"` // 商店数据 (shopId => []*ShopItem)
	Dialog *syncmap.Map[int, *DialogItem] `json:"-"` // 对话数据 (dialogId => *DialogItem)
}

// ============================================================================
// 构造函数与初始化
// ============================================================================

// Init 由 Lua 侧对象构造器调用，补齐运行期容器。
//
// id 是 Lua 运行时的内部对象号（每个 userdata 一个），不是业务 NPC ID，
// 因此这里刻意不把它写进 n.ID：修复前正是这个赋值让同脚本内的 NPC 全部拿到
// 同一个 ID（脚本 UID），业务侧再也分不出是哪个 NPC。
// 业务 ID 请用 SetId 或配置里的 "id" 字段给出，缺失时 Validate 会报出来。
func (n *Npc) Init(id int64, luascript *lua.LuaScript) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureStoresLocked()
}

// ensureStoresLocked 调用者必须持有 n.mu
func (n *Npc) ensureStoresLocked() {
	if n.Shop == nil {
		n.Shop = syncmap.NewMap[int, []*ShopItem]()
	}
	if n.Dialog == nil {
		n.Dialog = syncmap.NewMap[int, *DialogItem]()
	}
}

// ============================================================================
// 属性设置方法
// ============================================================================

// SetId 设置业务 NPC ID
// @param id NPC唯一标识
func (n *Npc) SetId(id uint32) {
	if id == 0 {
		vars.Error("[NPC] NPC ID 不能为 0，已忽略")
		return
	}
	n.mu.Lock()
	n.ID = id
	n.mu.Unlock()
}

// GetId 读取业务 NPC ID
func (n *Npc) GetId() uint32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ID
}

// SetName 设置NPC名称
func (n *Npc) SetName(name string) {
	if name == "" {
		vars.Error("[NPC] 名字无效")
		name = "未知NPC"
	}
	n.mu.Lock()
	n.Name = name
	n.mu.Unlock()
}

// GetName 读取NPC名称
func (n *Npc) GetName() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Name
}

// SetShape 设置NPC外观
func (n *Npc) SetShape(shape string) {
	n.mu.Lock()
	n.Shape = shape
	n.mu.Unlock()
}

// SetDirection 设置NPC朝向
// @param direction 朝向值(0-7)
func (n *Npc) SetDirection(direction int8) {
	if direction < 0 || direction > 7 {
		vars.Error("[NPC] 朝向值无效: %d, 期望范围: 0-7", direction)
		direction = 0
	}
	n.mu.Lock()
	n.Direction = direction
	n.mu.Unlock()
}

// SetMapId 设置所属地图并注册NPC
// @param mapId 地图ID
//
// 幂等：重复挂到同一张地图不会 append 出两份实例（巡逻/广播会把同一 NPC 当两个
// 对象处理）；改挂到别的地图时先从旧地图上摘掉。
//
// 整段持有 n.mu：MapID、attachedTo 与两张地图的 npcs 列表必须一起改完，
// 分开加锁时两个并发的 SetMapId 会各自 append，同一 NPC 在两处都留下记录。
// 锁序固定为 n.mu -> Map.mu，Map 侧不会反向取 n.mu，不构成死锁环。
func (n *Npc) SetMapId(mapId uint32) {
	n.mu.Lock()
	defer n.mu.Unlock()

	maps, ok := _maplist.Load(mapId)
	if !ok {
		// 目标地图都不存在：MapID 与 attachedTo 一个都不能动，否则
		// Validate 的「所属地图未设置」会被这个半截状态永久掩盖
		vars.Error("[NPC] 配置在未知的地图上: mapId=%d, npcId=%d", mapId, n.ID)
		return
	}
	if n.attachedTo == maps {
		n.MapID = mapId
		return
	}

	previous := n.attachedTo
	n.MapID = mapId
	n.attachedTo = maps

	if previous != nil {
		previous.mu.Lock()
		for i, item := range previous.npcs {
			if item == n {
				previous.npcs = append(previous.npcs[:i], previous.npcs[i+1:]...)
				break
			}
		}
		previous.mu.Unlock()
	}

	maps.mu.Lock()
	maps.addNpcLocked(n)
	maps.mu.Unlock()
}

// ============================================================================
// 路径点操作
// ============================================================================

// AddMapPoint 添加巡逻路径点
// @param x X坐标
// @param y Y坐标
// @param z Z坐标
func (n *Npc) AddMapPoint(x, y, z int32) {
	point := [3]int32{x, y, z}
	n.mu.Lock()
	n.MapPoint = append(n.MapPoint, point)
	n.mu.Unlock()
}

// GetPathPoints 返回路径点快照。
// 给的是拷贝：Lua 线程随后 Append 会原地改写底层数组，
// 直接把内部切片交出去，调用方读到的是半条路径。
func (n *Npc) GetPathPoints() [][3]int32 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.MapPoint) == 0 {
		return nil
	}
	out := make([][3]int32, len(n.MapPoint))
	copy(out, n.MapPoint)
	return out
}

// ============================================================================
// 商店操作
// ============================================================================

// AddShop 添加商店商品
// @param shopId      商店ID
// @param itemId      物品ID
// @param costType    货币类型
// @param maxBuyCnt   最大购买数量
// @param cost        价格
// @param refreshType 刷新类型(day/week/0)
//
// 实参顺序以本 Go 签名为准（Lua 侧曾把 cost 与 maxBuyCnt 传反）。
// 「取列表 → 追加 → 回写」整段必须在锁内：修复前两步之间毫无互斥，
// 两个协程往同一商店加货时后者会覆盖前者，商品条数凭空变少。
func (n *Npc) AddShop(shopId, itemId, costType, maxBuyCnt int, cost int64, refreshType string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureStoresLocked()

	list, _ := n.Shop.Load(shopId)
	item := &ShopItem{
		ItemID:         itemId,
		CostType:       costType,
		Cost:           cost,
		MaxBuyCnt:      maxBuyCnt,
		UpdateTimeType: refreshType,
	}
	n.Shop.Store(shopId, append(list, item))
}

// GetShop 获取商店商品列表
// @param shopId 商店ID
// @return 商品列表
func (n *Npc) GetShop(shopId int) []*ShopItem {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Shop == nil {
		return nil
	}
	l, ok := n.Shop.Load(shopId)
	if !ok || len(l) == 0 {
		return nil
	}
	// 切片拷贝：AddShop 会替换整个列表，但调用方拿到的那份不该再被别人改
	out := make([]*ShopItem, len(l))
	copy(out, l)
	return out
}

// HasShop 检查NPC是否有商店
func (n *Npc) HasShop() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Shop != nil && n.Shop.Length() > 0
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
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ensureStoresLocked()

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
	n.mu.Lock()
	defer n.mu.Unlock()
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
// 支持 NORMAL 类型的随机文本：当 Param1 为一组字符串时随机选一条
func (d *DialogItem) GetDisplayText() string {
	// 仅 NORMAL 类型支持随机文本
	if d.Type == DialogTypeNormal {
		texts := d.randomTexts()
		if len(texts) == 0 {
			return d.Text
		}
		return texts[util.RandInt(int64(len(texts)))]
	}
	return d.Text
}

// randomTexts 取出可用于随机的文本集合。
// Lua 传过来的表在 Go 侧是 *lua.LuaTable，既不是 []string 也不是 []any：
// 修复前只断言后两种，脚本里 rand_mode="every" 的随机对话永远只显示第一条。
func (d *DialogItem) randomTexts() []string {
	switch v := d.Param1.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	case *lua.LuaTable:
		arr := v.GetArray()
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	}
	return nil
}

// GetDefaultDialog 获取默认对话项(对话ID最小的)
func (n *Npc) GetDefaultDialog() *DialogItem {
	n.mu.Lock()
	defer n.mu.Unlock()
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
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.Dialog == nil {
		return nil
	}
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
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Dialog != nil && n.Dialog.Length() > 0
}

// ============================================================================
// 验证与辅助方法
// ============================================================================

// Validate 验证NPC配置完整性
// @return 验证错误列表
func (n *Npc) Validate() []string {
	n.mu.Lock()
	defer n.mu.Unlock()

	var errors []string

	if n.ID == 0 {
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
	// if len(n.MapPoint) == 0 {
	// 	errors = append(errors, "自动移动NPC必须配置路径点")
	// }

	return errors
}

// String 返回NPC的字符串表示
func (n *Npc) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return fmt.Sprintf("Npc[id=%d, name=%s, shape=%s, map=%d]", n.ID, n.Name, n.Shape, n.MapID)
}
