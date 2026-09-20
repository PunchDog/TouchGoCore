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

	// npcs 挂在本地图上的 NPC。非导出 + json:"-"：JSON 反序列化造出的对象
	// 不会走 Npc.Init，Shop/Dialog 会是 nil，后续 AddShop/GetShop 直接 panic。
	// 读取请用 SnapshotNpcs()（拿到的是切片拷贝，可安全遍历）。
	npcs []*Npc
	mu   sync.Mutex

	// path 记录来源文件名，仅用于重复 MapId 时指出保留了哪一份
	path string
}

// SnapshotNpcs 返回 NPC 列表的浅拷贝。
// 拷贝的是切片头：SetMapId 会 append 改写底层数组，直接把内部切片交出去，
// 遍历方可能读到重复项或半截列表。
func (m *Map) SnapshotNpcs() []*Npc {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.npcs) == 0 {
		return nil
	}
	out := make([]*Npc, len(m.npcs))
	copy(out, m.npcs)
	return out
}

// addNpcLocked 调用者必须持有 m.mu
func (m *Map) addNpcLocked(n *Npc) {
	m.npcs = append(m.npcs, n)
}

// clearNpcs 解除本地图上的 NPC 挂载，供 StopMap 使用。
// Map 上的 npcs 与 Npc.attachedTo 互为引用，不显式断开时整批 NPC 会随旧 Map
// 一起被调用方持有的快照留在内存里，热重载后仍在计数。
func (m *Map) clearNpcs() {
	m.mu.Lock()
	m.npcs = nil
	m.mu.Unlock()
}

func (this *Map) Load(path string) error {
	file, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取启动配置出错: %w", err)
	}
	// this 已是指向 Map 的指针：再取一层 &this 传成 **Map，
	// 当前靠 encoding/json 的 indirect 顺链解引用侥幸能跑，语义上并不正确
	if err := json.Unmarshal(file, this); err != nil {
		return fmt.Errorf("解析配置出错 %s: %w", path, err)
	}
	this.path = path
	if existing, ok := _maplist.LoadOrStore(this.MapId, this); ok {
		return fmt.Errorf("加载地图配置出错:%s:已经有相同ID的地图了(mapId=%d)，当前保留 %s", path, this.MapId, existing.path)
	}

	vars.Info("加载地图 %s 成功!", path)
	return nil
}

// RunMap 装载地图数据并把 Npc 类交给 Lua 运行时。
//
// 启动顺序上必须早于 luaService：Lua 脚本里 `Npc()` 造对象、`SetMapId` 挂地图，
// 两件事都依赖这里先完成类注册与地图登记。
func RunMap(ctx context.Context) error {
	cfg := corectx.CfgFrom(ctx)
	mapEnabled := cfg != nil && cfg.MapPath != "off" && cfg.MapPath != ""

	// 类注册与地图数据无关：即便 MapPath=off，Lua 侧也要能构造 NPC
	if err := lua.RegisterLuaClass(&Npc{}); err != nil {
		if !mapEnabled {
			// 没配地图目录时 NPC 本就无处可挂，不该让一个用不到的功能拖停整个进程
			vars.Error("注册 Npc 类失败（地图功能未启用，忽略）: %v", err)
			vars.Info("不启动地图功能")
			return nil
		}
		// 地图开启时这是硬前置：类没注册上，Lua 脚本里的 Npc() 全是
		// "attempt to call a nil value"，配置静默不生效比启动失败更难查
		vars.Error("注册 Npc 类失败: %v", err)
		return fmt.Errorf("注册 Npc 类失败: %w", err)
	}

	if !mapEnabled {
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

	vars.Info("读取地图完成!")
	if len(errs) > 0 {
		return fmt.Errorf("部分地图加载失败: %v", errs)
	}
	return nil
}

// ValidateNpcs 汇总校验所有已挂载 NPC 的配置完整性，只告警不阻断启动。
// 由 App.Start 在服务全部起来后调用一次：NPC 是 Lua 脚本创建并挂图的，
// 早于 luaService 调用只能看到空表。
func ValidateNpcs() []string {
	var problems []string
	_maplist.Range(func(_ uint32, m *Map) bool {
		for _, n := range m.SnapshotNpcs() {
			for _, msg := range n.Validate() {
				line := fmt.Sprintf("[NPC] mapId=%d npc=%s: %s", m.MapId, n, msg)
				vars.Warning("%s", line)
				problems = append(problems, line)
			}
		}
		return true
	})
	return problems
}

func GetMap(id uint32) (*Map, bool) {
	return _maplist.Load(id)
}

func StopMap(_ context.Context) {
	if _maplist == nil {
		return
	}
	_maplist.Range(func(_ uint32, m *Map) bool {
		m.clearNpcs()
		return true
	})
	_maplist.Clear()
}
