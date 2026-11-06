package taskengine

import (
	"context"
	"errors"
	"testing"

	"touchgocore/util"
)

// TestGetTaskReturnsResident 验证 GetTask 返回的就是引擎索引里的常驻对象。
func TestGetTaskReturnsResident(t *testing.T) {
	def := sampleDef(1)
	def.TargetCount = 3
	e := newTestEngine(t, newFakeStore(def))

	task := fetchTask(t, e, def.ID)
	if task != e.teak_progress(def.ID) {
		t.Fatal("GetTask 必须返回常驻对象，否则节点摘挂作用不到索引上")
	}
	if !inList(e.triggerlist[def.Trigger], task) {
		t.Fatal("常驻对象应挂在触发点链上")
	}
}

// TestMemberIsolation 验证不同会员引擎的进度与链表互不影响。
func TestMemberIsolation(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.TargetCount = 3
	store := newFakeStore(def)
	e1 := newTestEngineFor(t, store, 1)
	e2 := newTestEngineFor(t, store, 2)

	task1 := fetchTask(t, e1, def.ID)
	if err := e1.UpdateTask(ctx, task1, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("member1 update: %v", err)
	}
	if got := fetchTask(t, e1, def.ID).Progress; got != 1 {
		t.Fatalf("member1 progress=%d", got)
	}
	task2 := fetchTask(t, e2, def.ID)
	if task2.Progress != 0 || task2.Status != "" {
		t.Fatalf("member2 不应受影响: %+v", task2)
	}
	// 两条引擎各自持有节点对象，会员之间不得共享链表归属。
	if task1 == task2 || task1.tasknode == task2.tasknode {
		t.Fatal("会员引擎不得共用任务节点")
	}
}

// TestStaleCycleIgnored 验证上一周期的进度行不会泄漏到当前周期。
func TestStaleCycleIgnored(t *testing.T) {
	def := sampleDef(1)
	def.TargetCount = 3
	store := newFakeStore(def)
	store.rows = []TaskProgress{{
		ID: 1, MemberID: 1, TaskID: def.ID, CycleKey: "2000-01-01",
		Progress: 2, Status: ProgressClaimed, SnapshotJSON: "{}",
	}}
	e := newTestEngine(t, store)

	got := fetchTask(t, e, def.ID)
	if got.Progress != 0 {
		t.Fatalf("旧周期进度不应出现在本周期，实际 %d", got.Progress)
	}
	if got.Status != "" {
		t.Fatalf("旧周期状态不应出现，实际 %s", got.Status)
	}
	want := TaskCycleKey(CycleDaily, util.CurrentTime(), def.ID)
	if got.CycleKey != want {
		t.Fatalf("cycleKey=%s want %s", got.CycleKey, want)
	}
	// 旧周期是 claimed，本周期必须回到进行中链。
	if !inList(e.triggerlist[def.Trigger], got) || inList(e.claimedtasklist, got) {
		t.Fatal("旧周期终态不得占用本周期的终态链")
	}
}

// TestReloadRebuildsIndex 验证 Reload 后新增/停用定义生效。
func TestReloadRebuildsIndex(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	if _, err := e.GetTask(ctx, 0, ""); err != nil {
		t.Fatal(err)
	}

	added := sampleDef(1)
	added.ID = 2
	// 换一个触发点，保证后面的 CHECKIN 事件只针对被停用的定义。
	added.Trigger = "OTHER"
	store.defs[added.ID] = added
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := e.GetTask(ctx, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("Reload 后应有 2 条，实际 %d", len(list))
	}
	if !inList(e.triggerlist[added.Trigger], e.teak_progress(added.ID)) {
		t.Fatal("新增定义应挂入触发点链")
	}

	def.Enabled = 0
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.GetTask(ctx, def.ID, ""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("停用定义应返回 ErrDisabled，实际 %v", err)
	}
	// 停用后既不在常驻索引，也不在全局定义索引里。
	if e.teak_progress(def.ID) != nil {
		t.Fatal("停用定义应移出常驻索引")
	}
	if _, ok := taskglobal.getDef(def.ID); ok {
		t.Fatal("停用定义不应留在定义索引中")
	}
	// 事件也拿不到它。
	if err := e.OnEvent(ctx, addParm(def.Trigger)); err != nil {
		t.Fatalf("停用后事件不应报错: %v", err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("停用定义不得推进: %+v", store.rows)
	}
}
