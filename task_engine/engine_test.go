package taskengine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// newTestEngine 构造会员 1 的引擎。
func newTestEngine(t *testing.T, store Store) *Engine {
	return newTestEngineFor(t, store, 1)
}

// newTestEngineFor 构造指定会员的引擎；rewardFn 由需要的用例自行注入。
func newTestEngineFor(t *testing.T, store Store, memberID int64) *Engine {
	t.Helper()
	e, err := New(Options{Store: store, memberID: memberID})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// sampleDef 一个最小可用任务定义（ADD 口径、目标 1）。
func sampleDef(enabled int8) *TaskDef {
	return &TaskDef{
		ID: 1, Title: "签到", Desc: "每日签到", Trigger: "CHECKIN",
		Cycle: CycleDaily, GrantMode: GrantClaim, ProgressType: ProgressTypeAdd,
		TargetCount: 1, Enabled: enabled,
	}
}

// rowsFor 取某任务的全部进度行。
func rowsFor(store *fakeStore, taskID int64) []TaskProgress {
	out := make([]TaskProgress, 0)
	for _, r := range store.rows {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	return out
}

// fetchTask 通过 GetTask 取单条常驻任务对象。
func fetchTask(t *testing.T, e *Engine, taskID int64) *TaskProgress {
	t.Helper()
	list, err := e.GetTask(context.Background(), taskID, "")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("get task 返回 %d 条", len(list))
	}
	return list[0]
}

// inList 判断任务对象是否挂在某条链上（按指针身份比对）。
func inList(tl NodeList, task *TaskProgress) bool {
	for _, x := range tasksOf(tl) {
		if x == task {
			return true
		}
	}
	return false
}

// addParm 一次 +1 的进度上报。
func addParm(trigger string) *ProgressParm {
	return &ProgressParm{Trigger: trigger, ProgressParmAdd: 1}
}

// TestUpdateTaskInviteOverwrites 验证 INVITE 口径：Invite 覆盖进度，达标落 completed。
func TestUpdateTaskInviteOverwrites(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.ProgressType = ProgressTypeInvite
	def.TargetCount = 5
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 2}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("2/5 未达标，got %v", err)
	}
	rows := rowsFor(store, def.ID)
	if len(rows) != 1 || rows[0].Progress != 2 || rows[0].Status != ProgressActive {
		t.Fatalf("覆盖后进度: %+v", rows)
	}

	task = fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 5}); err != nil {
		t.Fatalf("5/5 应达标: %v", err)
	}
	rows = rowsFor(store, def.ID)
	if rows[0].Progress != 5 || rows[0].Status != ProgressCompleted {
		t.Fatalf("CLAIM 达标应 completed: %+v", rows[0])
	}
	if task.Progress != 5 || task.Status != ProgressCompleted {
		t.Fatalf("常驻对象未同步: %+v", task)
	}
}

// TestUpdateTaskAddAccumulates 验证 ADD 口径：三次 +1 达标落 claimed，已结算再更新被拒。
func TestUpdateTaskAddAccumulates(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 3
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	for i := 1; i <= 2; i++ {
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
			t.Fatalf("第 %d 次未达标，got %v", i, err)
		}
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("第 3 次应达标: %v", err)
	}
	rows := rowsFor(store, def.ID)
	if len(rows) != 1 || rows[0].Progress != 3 || rows[0].Status != ProgressClaimed {
		t.Fatalf("AUTO 达标应 claimed: %+v", rows)
	}

	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("已结算应拒绝，got %v", err)
	}
	if got := rowsFor(store, def.ID)[0].Progress; got != 3 {
		t.Fatalf("已结算后进度不得变化，实际 %d", got)
	}
}

// TestDefaultProgressType 验证 ProgressType 为空：Invite 非零优先覆盖，否则累加 Add。
func TestDefaultProgressType(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.ProgressType = ""
	def.TargetCount = 3
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	for i := 1; i <= 2; i++ {
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
			t.Fatalf("add 第 %d 次: %v", i, err)
		}
	}
	if got := rowsFor(store, def.ID)[0].Progress; got != 2 {
		t.Fatalf("add 累计=%d", got)
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 3}); err != nil {
		t.Fatalf("invite 非零应覆盖并达标: %v", err)
	}
	if got := rowsFor(store, def.ID)[0].Progress; got != 3 {
		t.Fatalf("invite 覆盖=%d", got)
	}
}

// TestTargetCountFloatCompare 验证 TargetCount 支持浮点，并按 > 比较。
func TestTargetCountFloatCompare(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.ProgressType = ProgressTypeInvite
	def.TargetCount = 2.5
	def.TargetCountType = TargetCountTypeGT
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 2}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("2 > 2.5 不成立，不应达标，got %v", err)
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 3}); err != nil {
		t.Fatalf("3 > 2.5 应达标: %v", err)
	}
}

// TestTargetCountFallbackOne 验证 TargetCount<=0 按 1 处理。
func TestTargetCountFallbackOne(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.TargetCount = 0
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("TargetCount=0 应按 1，一次即达标: %v", err)
	}
}

// TestUpdateTaskGuards 覆盖 nil/缺定义/停用/窗口外/前置未完成等拒绝路径。
func TestUpdateTaskGuards(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, newFakeStore(sampleDef(1)))

	if err := e.UpdateTask(ctx, nil, nil); !errors.Is(err, ErrDefMissing) {
		t.Fatalf("nil task: %v", err)
	}
	if err := e.UpdateTask(ctx, &TaskProgress{}, nil); !errors.Is(err, ErrDefMissing) {
		t.Fatalf("无 TaskID: %v", err)
	}
	// 手搓对象：按 TaskID 换回常驻对象后推进。
	if err := e.UpdateTask(ctx, &TaskProgress{TaskID: 1}, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("非常驻对象应按 TaskID 定位: %v", err)
	}
	// 索引与库里都没有的定义。
	if err := e.UpdateTask(ctx, &TaskProgress{TaskID: 99}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrDefMissing) {
		t.Fatalf("未知 TaskID: %v", err)
	}

	// 停用定义不会进常驻索引，只能靠 DB 回源判定。
	disabled := sampleDef(0)
	de := newTestEngine(t, newFakeStore(disabled))
	if err := de.UpdateTask(ctx, &TaskProgress{TaskID: disabled.ID}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}

	// 窗口外。
	past := util.CurrentTime().Add(-time.Hour)
	expired := sampleDef(1)
	expired.EndAt = &past
	ee := newTestEngine(t, newFakeStore(expired))
	if err := ee.UpdateTask(ctx, &TaskProgress{TaskID: expired.ID}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("窗口外: %v", err)
	}

	// 前置未完成 / 已完成：以前置任务的常驻状态为准。
	pre := &TaskDef{ID: 8, Cycle: CycleOnce, Enabled: 1, TargetCount: 1}
	gated := sampleDef(1)
	gated.PreTaskID = 8
	store := newFakeStore(gated, pre)
	ge := newTestEngine(t, store)
	if err := ge.UpdateTask(ctx, &TaskProgress{TaskID: gated.ID}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("前置未完成: %v", err)
	}
	// 把前置置为 completed 后放行。
	if err := ge.UpdateTask(ctx, &TaskProgress{TaskID: pre.ID}, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("前置自身应可推进: %v", err)
	}
	if err := ge.UpdateTask(ctx, &TaskProgress{TaskID: gated.ID}, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("前置完成后应放行: %v", err)
	}
}

// TestOnEventFanout 验证同 trigger 扇出 + 前置两阶段，同事件不连锁解锁后继。
func TestOnEventFanout(t *testing.T) {
	ctx := context.Background()
	a := sampleDef(1)
	a.ID = 10
	b := sampleDef(1)
	b.ID = 11
	b.PreTaskID = 10
	store := newFakeStore(a, b)
	e := newTestEngine(t, store)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("on event: %v", err)
	}
	if len(rowsFor(store, 10)) != 1 || len(rowsFor(store, 11)) != 0 {
		t.Fatalf("前置未完成不应触发后继: %+v", store.rows)
	}
	if got := rowsFor(store, 10)[0].Status; got != ProgressCompleted {
		t.Fatalf("CLAIM 应 completed，实际 %s", got)
	}

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("second event: %v", err)
	}
	if len(rowsFor(store, 11)) != 1 {
		t.Fatalf("前置完成后应推进后继: %+v", store.rows)
	}
}

// TestOnEventSkipsWindowAndEmptyParm 验证 nil/空 trigger/窗口外均不推进。
func TestOnEventSkipsWindowAndEmptyParm(t *testing.T) {
	ctx := context.Background()
	past := util.CurrentTime().Add(-time.Hour)
	expired := sampleDef(1)
	expired.EndAt = &past
	store := newFakeStore(expired)
	e := newTestEngine(t, store)

	if err := e.OnEvent(ctx, nil); err != nil {
		t.Fatalf("nil parm: %v", err)
	}
	if err := e.OnEvent(ctx, &ProgressParm{}); err != nil {
		t.Fatalf("空 trigger: %v", err)
	}
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("窗口外不应报错: %v", err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("窗口外不得推进: %+v", store.rows)
	}
}

// TestNotifyCompletedOnce 验证完成通知链只推进一次，不重复写入。
func TestNotifyCompletedOnce(t *testing.T) {
	ctx := context.Background()
	leader := sampleDef(1)
	leader.ID = 1
	leader.GrantMode = GrantAuto
	leader.TargetCount = 1

	follower := sampleDef(1)
	follower.ID = 20
	follower.Trigger = "TASK_COMPLETED"
	follower.GrantMode = GrantAuto
	follower.TargetCount = 1

	store := newFakeStore(leader, follower)
	e := newTestEngine(t, store)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("leader event: %v", err)
	}
	rows := rowsFor(store, follower.ID)
	if len(rows) != 1 || rows[0].Status != ProgressClaimed {
		t.Fatalf("follower 应被完成通知推进: %+v", rows)
	}
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("重复事件不应报错: %v", err)
	}
	if got := len(rowsFor(store, follower.ID)); got != 1 {
		t.Fatalf("完成通知不得重复写入，rows=%d", got)
	}
}

// TestRemoveReleasesAllNodes 验证 Remove 摘掉 tasklist/claimedtasklist 上的全部任务节点、
// 丢弃链表与索引，并把引擎自身节点从全局注册链摘除；重复调用是空操作。
func TestRemoveReleasesAllNodes(t *testing.T) {
	ctx := context.Background()
	// 两条任务分别落在 completed（tasklist）与 claimed（claimedtasklist）两条链上。
	claimDef := sampleDef(1)
	claimDef.Trigger = "CLAIM_EVT"
	autoDef := sampleDef(1)
	autoDef.ID = 2
	autoDef.Trigger = "AUTO_EVT"
	autoDef.GrantMode = GrantAuto
	store := newFakeStore(claimDef, autoDef)
	e := newTestEngine(t, store)

	claimTask := fetchTask(t, e, claimDef.ID)
	if err := e.UpdateTask(ctx, claimTask, addParm(claimDef.Trigger)); err != nil {
		t.Fatalf("claim 任务达标: %v", err)
	}
	autoTask := fetchTask(t, e, autoDef.ID)
	if err := e.UpdateTask(ctx, autoTask, addParm(autoDef.Trigger)); err != nil {
		t.Fatalf("auto 任务达标: %v", err)
	}
	if !inList(e.tasklist, claimTask) || !inList(e.claimedtasklist, autoTask) {
		t.Fatal("前置状态：claim 任务应在 tasklist，auto 任务应在 claimedtasklist")
	}

	e.Remove()

	if e.tasklist != nil || e.completetasklist != nil || e.claimedtasklist != nil {
		t.Fatal("四条链应被丢弃")
	}
	if e.membertasks != nil || e.triggerlist != nil {
		t.Fatal("任务索引应被丢弃")
	}
	found := false
	taskglobal.engineList.Range(func(n list.INode) bool {
		if n.GetData() == e {
			found = true
			return false
		}
		return true
	})
	if found {
		t.Fatal("引擎应已从全局注册链摘除")
	}

	// 重复调用不得 panic。
	e.Remove()
	if got, err := e.GetTask(ctx, 0, ""); err != nil || len(got) != 0 {
		t.Fatalf("remove 后 GetTask 应为空: %v, %d", err, len(got))
	}
}

// TestAPISurface 守卫 Engine 自有的导出方法只有 OnEvent / GetTask / UpdateTask / Reload / Remove。
//
// 其余导出方法来自内嵌的 list.Node（GetId/GetData 等），是挂进全局引擎链所必需的。
func TestAPISurface(t *testing.T) {
	// 内嵌节点的导出方法名集合，随 list.Node 自动同步。
	nodeType := reflect.TypeOf((*list.Node)(nil))
	allowed := make(map[string]bool, nodeType.NumMethod())
	for i := 0; i < nodeType.NumMethod(); i++ {
		allowed[nodeType.Method(i).Name] = true
	}
	want := map[string]bool{"OnEvent": true, "GetTask": true, "UpdateTask": true, "Reload": true, "Remove": true}
	typ := reflect.TypeOf(&Engine{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if want[name] || allowed[name] {
			continue
		}
		t.Fatalf("多余的导出方法: %s", name)
	}
	// 四个入口必须都在。
	for name := range want {
		if _, ok := typ.MethodByName(name); !ok {
			t.Fatalf("缺少导出方法: %s", name)
		}
	}
}
