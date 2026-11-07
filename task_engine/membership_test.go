package taskengine

import (
	"context"
	"errors"
	"testing"

	"touchgocore/util"
)

// TestReloadAttachesByStatus 验证加载数据时四条链的归属规则：
// claimed 只进 claimedtasklist；completed 进 tasklist+completetasklist 但不进 triggerlist；
// active 进 tasklist+triggerlist。
func TestReloadAttachesByStatus(t *testing.T) {
	ctx := context.Background()
	active := sampleDef(1)
	active.ID = 1
	done := sampleDef(1)
	done.ID = 2
	claimed := sampleDef(1)
	claimed.ID = 3
	claimed.GrantMode = GrantAuto

	now := util.CurrentTime()
	setupStorage(t)
	seedDefs(t, active, done, claimed)
	seedProgress(t,
		TaskProgress{MemberID: 1, TaskID: 2, CycleKey: TaskCycleKey(done.Cycle, now, 2), Progress: 1, Status: ProgressCompleted, SnapshotJSON: "{}"},
		TaskProgress{MemberID: 1, TaskID: 3, CycleKey: TaskCycleKey(claimed.Cycle, now, 3), Progress: 1, Status: ProgressClaimed, SnapshotJSON: "{}"},
	)
	e := newEngineFor(t, 1)

	tasks := e.teak_progress(1)
	dones := e.teak_progress(2)
	off := e.teak_progress(3)
	trig := e.triggerlist["CHECKIN"]

	// active：tasklist + triggerlist。
	if !inList(e.tasklist, tasks) || !inList(trig, tasks) {
		t.Fatalf("进行中任务应挂 tasklist+triggerlist: %+v", tasks)
	}
	if inList(e.completetasklist, tasks) || inList(e.claimedtasklist, tasks) {
		t.Fatal("进行中任务不得进终态链")
	}
	// completed：tasklist + completetasklist，且不得进 triggerlist。
	if !inList(e.tasklist, dones) || !inList(e.completetasklist, dones) {
		t.Fatalf("completed 应挂 tasklist+completetasklist: %+v", dones)
	}
	if inList(trig, dones) || inList(e.claimedtasklist, dones) {
		t.Fatal("completed 不得进 triggerlist/claimedtasklist")
	}
	// claimed：只进 claimedtasklist，其余三条链都不进。
	if !inList(e.claimedtasklist, off) {
		t.Fatal("claimed 应挂 claimedtasklist")
	}
	if inList(e.tasklist, off) || inList(e.completetasklist, off) || inList(trig, off) {
		t.Fatal("claimed 不得进另外三条链")
	}

	// 列表查询走 tasklist：进行中 + completed 可见，claimed 不在此列。
	list, err := e.GetTask(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].TaskID != 1 || list[1].TaskID != 2 {
		t.Fatalf("可见任务=%+v", list)
	}
	// 单条查询走 membertasks，claimed 仍可定位。
	single, err := e.GetTask(ctx, 3, "")
	if err != nil || len(single) != 1 || single[0].Status != ProgressClaimed {
		t.Fatalf("claimed 单条=%v err=%v", single, err)
	}
}

// TestOnEventSwitchesToCompleteList 验证 CLAIM 达标后节点从 triggerlist 切到 completetasklist，
// 且后续事件不再推进它。
func TestOnEventSwitchesToCompleteList(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.TargetCount = 2
	e := newTestEngine(t, def)
	task := e.teak_progress(def.ID)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("第一次事件: %v", err)
	}
	// 未达标：仍留在 triggerlist。
	if !inList(e.triggerlist["CHECKIN"], task) || task.Progress != 1 {
		t.Fatalf("未达标应保持进行中: %+v", task)
	}

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("第二次事件: %v", err)
	}
	if task.Status != ProgressCompleted {
		t.Fatalf("status=%s", task.Status)
	}
	// 匹配切换：离开 triggerlist，挂上 completetasklist，tasklist 保留。
	if inList(e.triggerlist["CHECKIN"], task) {
		t.Fatal("completed 任务不得留在 triggerlist")
	}
	if !inList(e.completetasklist, task) || !inList(e.tasklist, task) {
		t.Fatal("completed 应挂 completetasklist+tasklist")
	}
	// 事件再来得不到该任务（triggerlist 已无节点），进度不再变化。
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("第三次事件: %v", err)
	}
	if task.Progress != 2 || progressOf(t, def).Status != ProgressCompleted {
		t.Fatalf("completed 不应被再次推进: %+v", task)
	}
}

// TestOnEventSwitchesToClaimedList 验证 AUTO 达标后节点只留在 claimedtasklist。
func TestOnEventSwitchesToClaimedList(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	e := newTestEngine(t, def)
	task := e.teak_progress(def.ID)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("event: %v", err)
	}
	if task.Status != ProgressClaimed {
		t.Fatalf("status=%s", task.Status)
	}
	if !inList(e.claimedtasklist, task) {
		t.Fatal("claimed 应挂 claimedtasklist")
	}
	if inList(e.tasklist, task) || inList(e.completetasklist, task) || inList(e.triggerlist["CHECKIN"], task) {
		t.Fatal("claimed 不得留在另外三条链")
	}
	// UpdateTask 对已结算任务只能拿到 ErrAlreadyClaimed。
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("重复结算: %v", err)
	}
}

// TestUpdateTaskSwitchesLists 验证 UpdateTask 路径同样完成节点切换。
func TestUpdateTaskSwitchesLists(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 1
	e := newTestEngine(t, def)
	task := fetchTask(t, e, def.ID)

	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !inList(e.claimedtasklist, task) || inList(e.triggerlist["CHECKIN"], task) {
		t.Fatalf("UpdateTask 达标后应切到 claimed 链: %+v", task)
	}
}

// TestRollCycleReactivatesTerminalTasks 验证周期切换把终态任务复位回进行中并重挂节点。
func TestRollCycleReactivatesTerminalTasks(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	e := newTestEngine(t, def)
	task := e.teak_progress(def.ID)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("event: %v", err)
	}
	// 手工把任务退回上一个周期，模拟跨日后内存里的旧状态。
	task.CycleKey = "2000-01-01"
	if _, err := e.GetTask(ctx, 0, ""); err != nil {
		t.Fatal(err)
	}
	// GetTask 内的 rollCycle 已把它复活并重新入链。
	if task.Status != ProgressActive || task.Progress != 0 {
		t.Fatalf("跨周期应复位: %+v", task)
	}
	if !inList(e.tasklist, task) || !inList(e.triggerlist["CHECKIN"], task) {
		t.Fatal("复活后应回到 tasklist+triggerlist")
	}
	if inList(e.claimedtasklist, task) {
		t.Fatal("复活后不得留在 claimedtasklist")
	}
	// 新周期可以被事件重新推进。
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("新周期事件: %v", err)
	}
	if task.Progress != 1 || task.Status != ProgressClaimed {
		t.Fatalf("新周期应重新结算: %+v", task)
	}
}

// TestUpdateTaskTempObjectNotAttached 验证未常驻的定义（Reload 后新增）只落库，
// 其临时对象不会被挂进索引链。
func TestUpdateTaskTempObjectNotAttached(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	e := newTestEngine(t, def)
	before := e.tasklist.Length()

	late := sampleDef(1)
	late.ID = 42
	late.Trigger = "LATE"
	seedDefs(t, late)

	if err := e.UpdateTask(ctx, &TaskProgress{TaskID: late.ID}, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("回源定义应可推进: %v", err)
	}
	if e.tasklist.Length() != before {
		t.Fatalf("临时对象不得挂进 tasklist，长度 %d → %d", before, e.tasklist.Length())
	}
	if e.teak_progress(late.ID) != nil {
		t.Fatal("临时对象不得进常驻索引")
	}
	if tl := e.triggerlist[late.Trigger]; tl != nil && tl.Length() != 0 {
		t.Fatalf("临时对象不得进 triggerlist，长度=%d", tl.Length())
	}
	// 落库仍然发生。
	if progressOf(t, late) == nil {
		t.Fatalf("应落一行进度: %+v", progressOf(t, late))
	}
}
