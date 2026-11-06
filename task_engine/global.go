package taskengine

import (
	"touchgocore/list"
	"touchgocore/syncmap"
)

// NodeList 任务节点链表。
type NodeList = *list.List

// Trigger 事件触发点。
type Trigger = string

// Golbal 进程级共享状态：任务定义索引、引擎注册表与完成通知配置。
//
// 任务定义是静态配置，每会员一个 Engine 时共享同一份；Engine 用内嵌的 list.Node
// 挂进 engineList，便于统一遍历/回收。字段全部不导出，统一经 taskglobal 读写。
type Golbal struct {
	// defs 任务定义索引：taskID → 定义（只存启用行）。
	defs syncmap.Map[int64, *TaskDef]
	// engineList 已创建的引擎节点链，节点数据是 *Engine 本身。
	engineList NodeList
}

// taskglobal 进程级共享状态实例；链表必须在这里初始化，零值链表不可用。
var taskglobal = Golbal{engineList: list.NewUnindexedList()}

// setDefs 用一次 Reload 的结果整体替换定义索引。
//
// 先清后写：清空窗口内 getDef 会 miss，调用方按 id 回源 DB，语义不变。
func (g *Golbal) setDefs(rows []TaskDef) {
	g.defs.Clear()
	for _, d := range rows {
		d := d
		g.defs.Store(d.ID, &d)
	}
}

// getDef 按 id 取定义；第二个返回值表示是否命中。
func (g *Golbal) getDef(id int64) (*TaskDef, bool) {
	return g.defs.Load(id)
}

// storeDef 回填单个定义，供 DB 回源后复用。
func (g *Golbal) storeDef(d *TaskDef) {
	if d == nil {
		return
	}
	g.defs.Store(d.ID, d)
}

// addEngine 把引擎挂进注册表；Engine 内嵌 list.Node，本身就是链表节点。
func (g *Golbal) addEngine(e *Engine) {
	if e == nil || g.engineList == nil {
		return
	}
	g.engineList.Add(e)
}
