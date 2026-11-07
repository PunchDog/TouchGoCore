package taskengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"touchgocore/util"
)

// TestGetTaskListAndSingle 覆盖列表/单条/group/窗口外/缺失/停用。
func TestGetTaskListAndSingle(t *testing.T) {
	visible := sampleDef(1)
	visible.ID = 1
	hidden := sampleDef(1)
	hidden.ID = 2
	past := util.CurrentTime().Add(-time.Hour)
	hidden.EndAt = &past
	grouped := sampleDef(1)
	grouped.ID = 3
	grouped.Group = "daily"
	disabled := sampleDef(0)
	disabled.ID = 4

	cycle := TaskCycleKey(CycleDaily, util.CurrentTime(), 1)
	setupStorage(t)
	seedDefs(t, visible, hidden, grouped, disabled)
	seedProgress(t, TaskProgress{
		MemberID: 1, TaskID: 1, CycleKey: cycle, Progress: 2, Status: ProgressActive, SnapshotJSON: "{}",
	})
	e := newEngineFor(t, 1)
	ctx := context.Background()

	list, err := e.GetTask(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	// 可见 = 启用 + 窗口内：visible(1) 与 grouped(3)。
	if len(list) != 2 {
		t.Fatalf("窗口外/停用应过滤，实际 %d 条", len(list))
	}
	first := list[0]
	def := first.GetDef()
	if first.TaskID != 1 || def.Title != "签到" || def.Trigger != "CHECKIN" {
		t.Fatalf("任务字段=%+v def=%+v", first, def)
	}
	// Reload 已将本周期进度合并进常驻对象。
	if first.Progress != 2 || first.Status != ProgressActive {
		t.Fatalf("进度未合并: progress=%d status=%s", first.Progress, first.Status)
	}
	if first.CycleKey != cycle || def.TargetCount != 1 || def.TargetCountType != "" {
		t.Fatalf("cycle/target 错误: %+v", first)
	}
	// 常驻对象自带四个节点。
	if first.tasknode == nil || first.triggernode == nil || first.completenode == nil || first.claimednode == nil {
		t.Fatal("常驻任务缺少链表节点")
	}

	single, err := e.GetTask(ctx, 1, "")
	if err != nil || len(single) != 1 || single[0].TaskID != 1 {
		t.Fatalf("单条查询=%v err=%v", single, err)
	}

	group, err := e.GetTask(ctx, 0, "daily")
	if err != nil || len(group) != 1 || group[0].TaskID != grouped.ID {
		t.Fatalf("group 过滤=%v err=%v", group, err)
	}

	out, err := e.GetTask(ctx, hidden.ID, "")
	if err != nil || len(out) != 0 {
		t.Fatalf("窗口外单条应返回空切片: %v err=%v", out, err)
	}

	if _, err := e.GetTask(ctx, 99, ""); !errors.Is(err, ErrDefMissing) {
		t.Fatalf("缺失定义 err=%v", err)
	}
	if _, err := e.GetTask(ctx, disabled.ID, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("停用定义 err=%v", err)
	}
}

// TestGetTaskRoundTripUpdate 验证 GetTask → UpdateTask(parm) 的完整回写。
func TestGetTaskRoundTripUpdate(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.TargetCount = 2
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if task.Status != "" || task.Progress != 0 {
		t.Fatalf("无进度行应为零值: %+v", task)
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("1/2 未达标: %v", err)
	}

	task = fetchTask(t, e, def.ID)
	if task.Progress != 1 || task.Status != ProgressActive {
		t.Fatalf("回读进度: %+v", task)
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("2/2 应达标: %v", err)
	}
	if got := progressOf(t, def).Status; got != ProgressCompleted {
		t.Fatalf("status=%s", got)
	}
}

// TestGetTaskSnapshotRecordsParm 验证快照列记下本次上报参数。
func TestGetTaskSnapshotRecordsParm(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.TargetCount = 5
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 3}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("3/5 未达标: %v", err)
	}
	if task.SnapshotJSON == "" || task.SnapshotJSON == "{}" {
		t.Fatalf("快照应记录上报参数，实际 %q", task.SnapshotJSON)
	}
	if got := progressOf(t, def).SnapshotJSON; got != task.SnapshotJSON {
		t.Fatalf("落库快照=%q 内存快照=%q", got, task.SnapshotJSON)
	}
}
