package taskengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"touchgocore/db"
	"touchgocore/list"
	"touchgocore/util"
)

// Options Engine 构造参数。
type Options struct {
	// Store 任务定义读取与进度读写存储（必填）。
	Store Store
	// Cache 可选：进度写线走两级缓存（Redis 一级 + 定时回写 DB）。
	// 为 nil 时走 Store 事务路径。
	Cache *db.Cache[ProgressKey, TaskProgress]
	// memberID 该引擎绑定的玩家 ID；一个会员一个 Engine。
	memberID int64
	// rewardFn AUTO 达标时的发奖回调；为 nil 则不发奖。
	rewardFn func(*TaskProgress)
}

// Engine 任务引擎（单会员视图）。
//
// 会员的每个启用任务在内存里常驻一个 *TaskProgress，并用四个节点挂进四条链：
//
//	tasklist         全部可见任务（claimed 除外）
//	triggerlist      按触发点分组，只挂「进行中」任务
//	completetasklist 已达成待领取（completed）
//	claimedtasklist  已结算（claimed）
//
// 链表的归属就是任务的状态：达标时把节点从 triggerlist/tasklist 摘下来，
// 换挂到 complete 或 claimed 链；Reload 重新加载时按库中状态重挂。
type Engine struct {
	list.Node
	store Store                                // 定义读取与进度回退存储
	cache *db.Cache[ProgressKey, TaskProgress] // 可选进度缓存

	mu               sync.Mutex              // 保护下面的索引表与任务节点摘挂
	tasklist         NodeList                // 全部任务节点链
	completetasklist NodeList                // 全部已完成任务节点链
	claimedtasklist  NodeList                // 已经领取的任务
	membertasks      map[int64]*TaskProgress // taskID → 任务
	triggerlist      map[Trigger]NodeList    // 触发点 → 任务节点链
	memberID         int64                   // 绑定的玩家 ID
	rewardFn         func(*TaskProgress)     // AUTO 发奖回调
}

// progressRowKey 会员进度行在周期内的键。
type progressRowKey struct {
	// TaskID 任务定义 ID。
	TaskID int64
	// CycleKey 周期片。
	CycleKey string
}

// New 构造引擎，并立即 Reload 任务定义与会员进度。
func New(opts Options) (*Engine, error) {
	// Store 是唯一必填依赖。
	if opts.Store == nil {
		return nil, ErrStoreNotConfigured
	}

	// 创建新任务引擎
	e, ok := list.NewNode(nil, &Engine{}).(*Engine)
	if !ok {
		return nil, ErrCreateEngineFail
	}
	e.SetData(e)

	e.store = opts.Store
	e.cache = opts.Cache
	e.memberID = opts.memberID
	e.rewardFn = opts.rewardFn

	// 建立定义索引与任务链；失败则构造失败。
	if err := e.Reload(context.Background()); err != nil {
		return nil, err
	}

	// 挂进全局引擎注册表。
	taskglobal.addEngine(e)
	return e, nil
}

// newTaskNode 建一个挂任务对象的链表节点。
//
// nodeType 必须传非 nil：传 nil 时 list 走对象池分配，节点 Remove 后会被回收复用，
// 而任务节点是常驻持有、在四条链之间来回摘挂的，被复用改写 data 会踩坏任务对象。
func newTaskNode(t *TaskProgress) list.INode {
	return list.NewNode(t, &list.Node{})
}

// Reload 重新加载启用定义与会员进度，按状态重建四条链。
//
// 任务定义变更后应调用本方法；建议在低峰调用。
func (e *Engine) Reload(ctx context.Context) error {
	rows, err := e.store.ListEnabledDefs(ctx)
	if err != nil {
		return err
	}
	// 先清后写；清空窗口内 defOf 会回源 DB，语义不变。
	taskglobal.setDefs(rows)
	now := util.CurrentTime()
	// 会员当前周期的进度：加载时按状态决定节点挂到哪条链。
	progs, err := e.loadProgressMap(ctx, rows, now)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasklist = list.NewUnindexedList()
	e.completetasklist = list.NewUnindexedList()
	e.claimedtasklist = list.NewUnindexedList()
	e.membertasks = make(map[int64]*TaskProgress, len(rows))
	e.triggerlist = make(map[Trigger]NodeList)
	// 按 ListEnabledDefs 的返回顺序（id 升序）建任务，保证 GetTask 顺序稳定。
	for _, def := range rows {
		t := &TaskProgress{
			MemberID:     e.memberID,
			TaskID:       def.ID,
			CycleKey:     TaskCycleKey(def.Cycle, now, def.ID),
			SnapshotJSON: "{}",
		}
		// 四条链各用一个独立节点。
		t.tasknode = newTaskNode(t)
		t.triggernode = newTaskNode(t)
		t.completenode = newTaskNode(t)
		t.claimednode = newTaskNode(t)
		// 合并已有进度行；无行则保持 active 零值。
		if p := progs[def.ID]; p != nil {
			t.ID = p.ID
			t.CycleKey = p.CycleKey
			t.Progress = p.Progress
			t.Status = p.Status
			t.SnapshotJSON = p.SnapshotJSON
			t.CreatedAt = p.CreatedAt
			t.UpdatedAt = p.UpdatedAt
		}
		e.membertasks[def.ID] = t
		e.attachLocked(t)
	}
	return nil
}

// loadProgressMap 取该会员本轮期的进度行，按 taskID 归档。
func (e *Engine) loadProgressMap(ctx context.Context, rows []TaskDef, now time.Time) (map[int64]*TaskProgress, error) {
	out := make(map[int64]*TaskProgress, len(rows))
	if e.cache != nil {
		// 缓存路径：逐任务 GetOrLoad，回源由缓存层负责。
		for _, def := range rows {
			p, err := e.loadCached(ctx, ProgressKey{MemberID: e.memberID, TaskID: def.ID, CycleKey: TaskCycleKey(def.Cycle, now, def.ID)})
			if err != nil {
				return nil, err
			}
			if p != nil {
				out[def.ID] = p
			}
		}
		return out, nil
	}
	progRows, err := e.store.ListProgressByMember(ctx, e.memberID)
	if err != nil {
		return nil, err
	}
	// 只认本周期行，旧周期进度不得复活。
	byKey := make(map[progressRowKey]*TaskProgress, len(progRows))
	for i := range progRows {
		p := progRows[i]
		byKey[progressRowKey{TaskID: p.TaskID, CycleKey: p.CycleKey}] = &p
	}
	for _, def := range rows {
		key := progressRowKey{TaskID: def.ID, CycleKey: TaskCycleKey(def.Cycle, now, def.ID)}
		if p := byKey[key]; p != nil {
			out[def.ID] = p
		}
	}
	return out, nil
}

// nodeFor 取某条链上该任务的节点，必要时重建。
//
// list.Node 在 Remove 时会清空 data（对象池语义），所以摘过链的节点不能再直接挂回去，
// 否则链表里就是一个 data 为空的幽灵节点。
func nodeFor(t *TaskProgress, cur list.INode) list.INode {
	if cur == nil || cur.GetData() == nil {
		return newTaskNode(t)
	}
	return cur
}

// attachLocked 按当前状态把任务节点挂入链表（须持有 e.mu）。
//
// claimed   → 只挂 claimedtasklist，另外三条链一律不挂；
// completed → 挂 tasklist + completetasklist，不挂 triggerlist；
// active    → 挂 tasklist + triggerlist。
func (e *Engine) attachLocked(t *TaskProgress) {
	switch t.Status {
	case ProgressClaimed:
		t.claimednode = nodeFor(t, t.claimednode)
		e.claimedtasklist.Add(t.claimednode)
	case ProgressCompleted:
		t.tasknode = nodeFor(t, t.tasknode)
		t.completenode = nodeFor(t, t.completenode)
		e.tasklist.Add(t.tasknode)
		e.completetasklist.Add(t.completenode)
	default:
		t.tasknode = nodeFor(t, t.tasknode)
		t.triggernode = nodeFor(t, t.triggernode)
		e.tasklist.Add(t.tasknode)
		def := t.GetDef()
		if def == nil {
			return
		}
		tl := e.triggerlist[def.Trigger]
		if tl == nil {
			tl = list.NewUnindexedList()
			e.triggerlist[def.Trigger] = tl
		}
		tl.Add(t.triggernode)
	}
}

// detachLocked 摘掉任务在四条链上的全部节点（须持有 e.mu）。
//
// 临时对象没有节点，逐个判空；摘链会清掉节点 data，入链时由 nodeFor 重建。
func (e *Engine) detachLocked(t *TaskProgress) {
	if t.tasknode != nil {
		t.tasknode.Remove()
	}
	if t.triggernode != nil {
		t.triggernode.Remove()
	}
	if t.completenode != nil {
		t.completenode.Remove()
	}
	if t.claimednode != nil {
		t.claimednode.Remove()
	}
}

// relocate 按当前状态重挂任务节点，用于达标或周期切换后的状态迁移。
//
// 只服务常驻对象：临时对象（Reload 后新增的定义）挂进链表会污染索引。
func (e *Engine) relocate(t *TaskProgress) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.membertasks[t.TaskID] != t {
		return
	}
	e.detachLocked(t)
	e.attachLocked(t)
}

// defOf 按 id 取任务定义：优先全局内存索引，未命中回源 DB 并回填。
func (e *Engine) defOf(ctx context.Context, id int64) (*TaskDef, error) {
	// 命中索引直接返回指针。
	if d, ok := taskglobal.getDef(id); ok {
		return d, nil
	}
	d, err := e.store.FindDefByID(ctx, id)
	if err != nil || d == nil {
		return nil, err
	}
	// 回填索引，后续同 id 不再回源。
	taskglobal.storeDef(d)
	return d, nil
}

// teak_progress 取任务进度；不存在返回 nil。
func (e *Engine) teak_progress(taskID int64) *TaskProgress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.membertasks[taskID]
}

// all_task_progress 取全部任务链。
func (e *Engine) all_task_progress() NodeList {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tasklist
}

// trigger_task_progress 取触发点对应的任务链（只含进行中任务）。
func (e *Engine) trigger_task_progress(trigger Trigger) NodeList {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.triggerlist[trigger]
}

// tasksOf 拷贝取出某条链上的任务对象，避免在 Range 回调里改链。
func tasksOf(tl NodeList) []*TaskProgress {
	out := make([]*TaskProgress, 0, 8)
	if tl == nil {
		return out
	}
	tl.Range(func(n list.INode) bool {
		if n == nil || n.GetData() == nil {
			return true
		}
		if t, ok := n.GetData().(*TaskProgress); ok {
			out = append(out, t)
		}
		return true
	})
	return out
}

// rollCycle 把周期已切换的终态任务复位回「进行中」并重挂节点。
//
// 跨日后 claimed/completed 任务不会永远留在终态链上；DAILY/WEEKLY 靠这里复活。
func (e *Engine) rollCycle(now time.Time) {
	e.mu.Lock()
	// 只扫终态两条链：active 任务的 CycleKey 由 advance 就地更新。
	done := append(tasksOf(e.completetasklist), tasksOf(e.claimedtasklist)...)
	changed := make([]*TaskProgress, 0, len(done))
	for _, t := range done {
		def := t.GetDef()
		if def == nil {
			continue
		}
		if t.CycleKey == TaskCycleKey(def.Cycle, now, def.ID) {
			continue
		}
		// 新周期从零开始。
		t.CycleKey = TaskCycleKey(def.Cycle, now, def.ID)
		t.Progress = 0
		t.Status = ProgressActive
		t.SnapshotJSON = "{}"
		changed = append(changed, t)
	}
	for _, t := range changed {
		e.detachLocked(t)
		e.attachLocked(t)
	}
	e.mu.Unlock()
}

// OnEvent 按 parm.Trigger 推进该会员的所有任务。
//
// 读取来自 triggerlist —— 里面只剩进行中任务；两阶段：先按本次事件之前的
// 状态确定候选，再统一推进并把达标任务的节点切换到 complete/claimed 链。
func (e *Engine) OnEvent(ctx context.Context, parm *ProgressParm) error {
	// 空参数或空触发点直接忽略。
	if parm == nil || strings.TrimSpace(parm.Trigger) == "" {
		return nil
	}
	now := util.CurrentTime()
	e.rollCycle(now)
	tl := e.trigger_task_progress(parm.Trigger)
	if tl == nil {
		return nil
	}

	// 第一段：收集该触发点下的进行中任务（不做 I/O）。
	cands := tasksOf(tl)

	// 第二段：窗口与前置判定，确定本次可推进的任务。
	var first error
	ready := make([]*TaskProgress, 0, len(cands))
	for _, t := range cands {
		def := t.GetDef()
		if def == nil || def.Enabled != 1 || !defInWindow(def, now) {
			continue
		}
		ok, err := e.prerequisiteReady(ctx, def)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if ok {
			ready = append(ready, t)
		}
	}

	// 第三段：统一推进；单任务未达标/已结算不阻断其它任务。
	for _, t := range ready {
		err := e.advance(ctx, t, parm, now)
		switch {
		case err == nil:
		case errors.Is(err, ErrNotComplete), errors.Is(err, ErrNotEligible),
			errors.Is(err, ErrAlreadyClaimed), errors.Is(err, ErrDisabled), errors.Is(err, ErrDefMissing):
			// 单任务未推进，继续扇出。
		default:
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// UpdateTask 推进指定任务；task 取 GetTask 返回的常驻对象。
//
// 传副本时按 TaskID 换回常驻对象，否则节点摘挂作用不到引擎索引上。
func (e *Engine) UpdateTask(ctx context.Context, task *TaskProgress, parm *ProgressParm) error {
	// 无任务 ID 不足以定位任务。
	if task == nil || task.TaskID <= 0 {
		return ErrDefMissing
	}
	t := task
	if live := e.teak_progress(task.TaskID); live != nil {
		t = live
	}
	def := t.GetDef()
	// 不在定义索引里（停用或 Reload 后新增）：回源 DB 才能区分缺失与停用。
	if def == nil {
		d, err := e.defOf(ctx, task.TaskID)
		if err != nil {
			return err
		}
		if d == nil {
			return ErrDefMissing
		}
		def = d
	}
	// 定义必须处于启用状态。
	if def.Enabled != 1 {
		return ErrDisabled
	}
	now := util.CurrentTime()
	e.rollCycle(now)
	// 投放窗口外不推进。
	if !defInWindow(def, now) {
		return ErrNotEligible
	}
	// 前置任务本周期未完成时不放行。
	ready, err := e.prerequisiteReady(ctx, def)
	if err != nil {
		return err
	}
	if !ready {
		return ErrNotEligible
	}
	return e.advance(ctx, t, parm, now)
}

// advance 就地推进一个常驻任务：算进度 → 判达标 →（AUTO 发奖）→ 落库 → 切链。
//
// 状态由节点归属表达：达标后任务离开 triggerlist，claimed 同时离开 tasklist。
func (e *Engine) advance(ctx context.Context, t *TaskProgress, parm *ProgressParm, now time.Time) error {
	def := t.GetDef()
	if def == nil {
		return ErrDefMissing
	}
	// 已是终态：不降级也不重复发奖。
	if TaskProgressDone(t.Status) {
		return ErrAlreadyClaimed
	}
	achieved := applyProgress(t, def, parm, now)
	// 达标先发奖，再落库、切链。
	if achieved && def.GrantMode == GrantAuto {
		e.reward(t)
	}
	if err := e.persist(ctx, t, now); err != nil {
		return err
	}
	if !achieved {
		return ErrNotComplete
	}
	e.relocate(t)
	return e.notifyCompleted(ctx)
}

// applyProgress 按进度口径把一次上报合入常驻任务对象，返回是否达成。
//
//	INVITE → 用 ProgressParmInvite 覆盖
//	ADD    → 在当前进度上累加 ProgressParmAdd
//	未配置 → Invite 非零则覆盖，否则累加 Add
func applyProgress(t *TaskProgress, def *TaskDef, parm *ProgressParm, now time.Time) bool {
	// 防御 nil 参数，按零值处理。
	if parm == nil {
		return false
	}
	// 周期切换后从零起算。
	cycle := TaskCycleKey(def.Cycle, now, def.ID)
	progress := t.Progress
	if t.CycleKey != cycle {
		t.CycleKey = cycle
		progress = 0
	}
	// 按任务配置的进度口径计算。
	switch strings.ToUpper(strings.TrimSpace(def.ProgressType)) {
	case ProgressTypeInvite:
		// 覆盖：直接采用上报的全局数据。
		progress = parm.ProgressParmInvite
	case ProgressTypeAdd:
		// 累加：在当前进度上加增量。
		progress += parm.ProgressParmAdd
	default:
		// 未配置：Invite 非零视为覆盖，否则按增量累加。
		if parm.ProgressParmInvite != 0 {
			progress = parm.ProgressParmInvite
		} else {
			progress += parm.ProgressParmAdd
		}
	}
	t.Progress = progress
	// 与目标值按 > / = / < 比较。
	achieved := TargetReached(progress, def.TargetCount, def.TargetCountType)
	t.Status = ProgressActive
	if achieved {
		// AUTO 直接结算；CLAIM 等宿主发放。
		if def.GrantMode == GrantAuto {
			t.Status = ProgressClaimed
		} else {
			t.Status = ProgressCompleted
		}
	}
	// 快照只用于排查，不参与判定。
	js, _ := json.Marshal(parm)
	t.SnapshotJSON = string(js)
	t.UpdatedAt = now
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	return achieved
}

// persist 落库一次进度；缓存路径同步写 Redis 并定时回写，回退路径走事务 upsert。
func (e *Engine) persist(ctx context.Context, t *TaskProgress, now time.Time) error {
	if e.cache != nil {
		key := ProgressKey{MemberID: e.memberID, TaskID: t.TaskID, CycleKey: t.CycleKey}
		return e.cache.Write(ctx, key, t)
	}
	return e.store.WithTx(ctx, func(tx Tx) error {
		return tx.UpsertProgress(ctx, t, now)
	})
}

// reward 触发宿主发奖回调；回调没有返回值，失败无法回滚本次写入。
func (e *Engine) reward(p *TaskProgress) {
	if e.rewardFn != nil {
		e.rewardFn(p)
	}
}

// loadCached 从两级缓存读进度；回源确认不存在时返回 (nil, nil)。
func (e *Engine) loadCached(ctx context.Context, key ProgressKey) (*TaskProgress, error) {
	p, err := e.cache.GetOrLoad(ctx, key)
	if err != nil {
		// 源确认不存在：视为没有进度行。
		if errors.Is(err, db.ErrCacheNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// notifyDepthKey 完成通知的递归深度键。
type notifyDepthKey struct{}

// maxNotifyDepth 完成通知最大递归深度，防止任务链互相触发成环。
const maxNotifyDepth = 8

// notifyCompleted 扇出「任务完成」通知；未配置 CompletedNotifyTrigger 则关闭。
func (e *Engine) notifyCompleted(ctx context.Context) error {
	// 递归深度保护，避免任务链成环。
	depth, _ := ctx.Value(notifyDepthKey{}).(int)
	if depth >= maxNotifyDepth {
		return nil
	}
	next := context.WithValue(ctx, notifyDepthKey{}, depth+1)
	// 完成通知统一按增量 1 扇出。
	return e.OnEvent(next, &ProgressParm{Trigger: string(CompletedNotifyTrigger), ProgressParmAdd: 1})
}

// prerequisiteReady 前置任务是否已在本周期完成。
func (e *Engine) prerequisiteReady(ctx context.Context, def *TaskDef) (bool, error) {
	// 无前置视为已就绪。
	if def == nil || def.PreTaskID <= 0 {
		return true, nil
	}
	// 前置任务已常驻：终态链的归属即答案，免去一次库查询。
	if pre := e.teak_progress(def.PreTaskID); pre != nil {
		preDef := pre.GetDef()
		if preDef == nil {
			return false, nil
		}
		if pre.CycleKey != TaskCycleKey(preDef.Cycle, util.CurrentTime(), preDef.ID) {
			return false, nil
		}
		return TaskProgressDone(pre.Status), nil
	}
	pre, err := e.defOf(ctx, def.PreTaskID)
	if err != nil {
		return false, err
	}
	// 前置定义缺失：保持锁定。
	if pre == nil {
		return false, nil
	}
	// 前置按自己的周期计算当前 cycle_key。
	cycle := TaskCycleKey(pre.Cycle, util.CurrentTime(), pre.ID)
	return e.store.HasDoneProgress(ctx, e.memberID, pre.ID, cycle)
}

// defInWindow 投放窗口判定；now == start/end 视为在窗内。
func defInWindow(def *TaskDef, now time.Time) bool {
	// 早于开始时间不展示也不推进。
	if def.StartAt != nil && now.Before(def.StartAt.UTC()) {
		return false
	}
	// 晚于结束时间不展示也不推进。
	if def.EndAt != nil && now.After(def.EndAt.UTC()) {
		return false
	}
	return true
}

// tempTask 给未常驻（停用/缺失回源）的定义合成一个临时任务对象。
func tempTask(def *TaskDef, memberID int64, cur *TaskProgress, now time.Time) *TaskProgress {
	p := &TaskProgress{
		MemberID:     memberID,
		TaskID:       def.ID,
		CycleKey:     TaskCycleKey(def.Cycle, now, def.ID),
		SnapshotJSON: "{}",
	}
	// 有进度行则合并进度字段。
	if cur != nil {
		p.ID = cur.ID
		p.CycleKey = cur.CycleKey
		p.Progress = cur.Progress
		p.Status = cur.Status
		p.SnapshotJSON = cur.SnapshotJSON
		p.CreatedAt = cur.CreatedAt
		p.UpdatedAt = cur.UpdatedAt
	}
	return p
}
