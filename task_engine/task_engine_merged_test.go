package taskengine

import (
	"context"
	"errors"
	"fmt"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/db"
	"touchgocore/list"
	"touchgocore/util"
)

// concurrentRewardLog 线程安全的发奖记录器。
//
// 本用例的发奖回调只在消费者 goroutine 上执行，理论上不会并发；
// 这里仍显式加锁，避免用例自身成为竞争源，也便于将来扩展到多会员并行。
type concurrentRewardLog struct {
	// mu 保护下面两个切片。
	mu sync.Mutex
	// keys 每次回调收到的框架幂等键。
	keys []RewardKey
	// rows 每次回调收到的任务对象副本。
	rows []TaskProgress
}

// record 返回一个把回调记入日志的 RewardFn。
func (l *concurrentRewardLog) record() RewardFn {
	return func(ctx context.Context, key RewardKey, p *TaskProgress) {
		if p == nil {
			return
		}
		cp := *p
		l.mu.Lock()
		defer l.mu.Unlock()
		l.keys = append(l.keys, key)
		l.rows = append(l.rows, cp)
	}
}

// snapshot 取回调日志快照（拷贝切片，调用方无需持锁）。
func (l *concurrentRewardLog) snapshot() ([]RewardKey, []TaskProgress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]RewardKey, len(l.keys))
	copy(keys, l.keys)
	rows := make([]TaskProgress, len(l.rows))
	copy(rows, l.rows)
	return keys, rows
}

// TestOnEventConcurrentSameMember 验证同一会员在「并发生产者 + 单逻辑线程投递」
// 下事件不丢失、终态正确、AUTO 发奖恰好一次（不重复发奖）。
//
// 场景：3 个任务同挂会员 1、同一触发点 CHECKIN，覆盖 AUTO / CLAIM 与不同目标值：
//
//	task 101 AUTO 目标 64（= 全部事件数）→ 最后一个事件达标，进度必须恰好 64；
//	task 102 AUTO 目标 16（早达标）      → 达标即摘出 triggerlist，进度冻结在 16；
//	task 103 CLAIM 目标 8（早达标）      → 达标即 completed，进度冻结在 8。
//
// 关键不变量：AUTO 任务每个周期只回调一次发奖；终态任务不再被后续事件推进。
func TestOnEventConcurrentSameMember(t *testing.T) {
	const (
		// producerCount 并发生产者数量（16，落在 16~32 区间）。
		producerCount = 16
		// eventsPerProducer 每个生产者投递的事件数。
		eventsPerProducer = 4
		// totalEvents 总事件数，同时是 task 101 的目标值。
		totalEvents = producerCount * eventsPerProducer
		// earlyAutoTarget task 102 的目标值。
		earlyAutoTarget = 16
		// earlyClaimTarget task 103 的目标值。
		earlyClaimTarget = 8
		// trigger 三个任务共用的触发点。
		trigger = "CHECKIN"
	)
	ctx := context.Background()

	// 全量 AUTO：目标等于总事件数，用于验证「事件一条不丢」。
	autoAll := sampleDef(1)
	autoAll.ID = 101
	autoAll.Title = "AUTO 全量"
	autoAll.Trigger = trigger
	autoAll.Cycle = CycleOnce
	autoAll.GrantMode = GrantAuto
	autoAll.ProgressType = ProgressTypeAdd
	autoAll.TargetCount = float64(totalEvents)
	autoAll.Reward = `{"item":"full"}`

	// 早达标 AUTO：达标后必须离开 triggerlist，进度不得继续上涨。
	autoEarly := sampleDef(1)
	autoEarly.ID = 102
	autoEarly.Title = "AUTO 早达标"
	autoEarly.Trigger = trigger
	autoEarly.Cycle = CycleOnce
	autoEarly.GrantMode = GrantAuto
	autoEarly.ProgressType = ProgressTypeAdd
	autoEarly.TargetCount = float64(earlyAutoTarget)
	autoEarly.Reward = `{"item":"early"}`

	// 早达标 CLAIM：引擎不发奖，只落 completed。
	claimEarly := sampleDef(1)
	claimEarly.ID = 103
	claimEarly.Title = "CLAIM 早达标"
	claimEarly.Trigger = trigger
	claimEarly.Cycle = CycleOnce
	claimEarly.GrantMode = GrantClaim
	claimEarly.ProgressType = ProgressTypeAdd
	claimEarly.TargetCount = float64(earlyClaimTarget)

	setupStorage(t)
	seedDefs(t, autoAll, autoEarly, claimEarly)

	var rewardLog concurrentRewardLog
	e, err := New(Options{MemberID: 1, RewardFn: rewardLog.record()})
	if err != nil {
		t.Fatalf("构造引擎: %v", err)
	}
	t.Cleanup(e.Remove)

	// 单逻辑线程投递器：队列容量等于总事件数，生产者不会因满队列阻塞。
	queue := make(chan *ProgressParm, totalEvents)
	errCh := make(chan error, totalEvents)

	// 唯一消费者：串行调用 OnEvent，满足「同一会员的事件必须串行投递」的契约。
	var consumer sync.WaitGroup
	consumer.Add(1)
	go func() {
		defer consumer.Done()
		for parm := range queue {
			if err := e.OnEvent(ctx, parm); err != nil {
				errCh <- err
			}
		}
	}()

	// N 个生产者并发提交事件。
	var producers sync.WaitGroup
	producers.Add(producerCount)
	for i := 0; i < producerCount; i++ {
		go func() {
			defer producers.Done()
			for j := 0; j < eventsPerProducer; j++ {
				queue <- addParm(trigger)
			}
		}()
	}

	// 先等生产者全部投递完，再关队列让消费者退出，最后收集错误。
	producers.Wait()
	close(queue)
	consumer.Wait()
	close(errCh)
	for gotErr := range errCh {
		t.Fatalf("并发投递期间 OnEvent 返回错误: %v", gotErr)
	}

	// 消费者已退出，引擎不再有并发写入，下面的字段读取与链表遍历是安全的。
	// 断言 1：满目标 AUTO 任务的进度必须等于总事件数 —— 事件一条都不能丢。
	full := e.teak_progress(autoAll.ID)
	if full == nil {
		t.Fatalf("任务 %d 未常驻", autoAll.ID)
	}
	if full.Progress != int64(totalEvents) || full.Status != ProgressClaimed {
		t.Fatalf("满目标任务 progress=%d status=%s，期望 %d/%s",
			full.Progress, full.Status, totalEvents, ProgressClaimed)
	}

	// 断言 2：早达标 AUTO 任务在达标点冻结，不得被后续事件继续推进。
	early := e.teak_progress(autoEarly.ID)
	if early == nil {
		t.Fatalf("任务 %d 未常驻", autoEarly.ID)
	}
	if early.Progress != int64(earlyAutoTarget) || early.Status != ProgressClaimed {
		t.Fatalf("早达标 AUTO progress=%d status=%s，期望 %d/%s",
			early.Progress, early.Status, earlyAutoTarget, ProgressClaimed)
	}

	// 断言 3：CLAIM 任务落 completed 并在达标点冻结。
	claim := e.teak_progress(claimEarly.ID)
	if claim == nil {
		t.Fatalf("任务 %d 未常驻", claimEarly.ID)
	}
	if claim.Progress != int64(earlyClaimTarget) || claim.Status != ProgressCompleted {
		t.Fatalf("早达标 CLAIM progress=%d status=%s，期望 %d/%s",
			claim.Progress, claim.Status, earlyClaimTarget, ProgressCompleted)
	}

	// 断言 4：AUTO 发奖恰好两次（每个 AUTO 任务一次），且幂等键互不重复。
	keys, rows := rewardLog.snapshot()
	if len(rows) != 2 || len(keys) != 2 {
		t.Fatalf("发奖回调次数=%d（keys=%d），期望 2", len(rows), len(keys))
	}
	now := util.CurrentTime()
	wantKeys := map[RewardKey]int{
		{MemberID: 1, TaskID: autoAll.ID, CycleKey: TaskCycleKey(CycleOnce, now, autoAll.ID)}:     1,
		{MemberID: 1, TaskID: autoEarly.ID, CycleKey: TaskCycleKey(CycleOnce, now, autoEarly.ID)}: 1,
	}
	gotKeys := make(map[RewardKey]int, len(keys))
	for _, k := range keys {
		gotKeys[k]++
	}
	for want, wantCount := range wantKeys {
		if gotKeys[want] != wantCount {
			t.Fatalf("幂等键 %s 回调 %d 次，期望 %d 次；全部=%v",
				want.String(), gotKeys[want], wantCount, keys)
		}
	}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("出现意外幂等键，全部=%v", keys)
	}
	// 回调入参必须已处于终态，且进度与引擎内一致（不重复发奖的前提）。
	for _, row := range rows {
		if row.Status != ProgressClaimed {
			t.Fatalf("发奖回调入参状态=%s，期望 %s", row.Status, ProgressClaimed)
		}
		if row.MemberID != 1 {
			t.Fatalf("发奖回调入参 member=%d，期望 1", row.MemberID)
		}
	}

	// 断言 5：三个任务都已终态，CHECKIN 触发链必须清空（后续事件不再扇出）。
	if left := tasksOf(e.trigger_task_progress(trigger)); len(left) != 0 {
		t.Fatalf("终态后触发链应清空，仍有 %d 个进行中任务", len(left))
	}

	// 断言 6：落库后每个任务恰好一行，进度/状态与内存一致（同周期不重复插行）。
	flushProgress(t)
	rows2 := dbProgressRows(t, 1)
	if len(rows2) != 3 {
		t.Fatalf("进度行数=%d，期望 3", len(rows2))
	}
	byTask := make(map[int64]TaskProgress, len(rows2))
	for _, row := range rows2 {
		byTask[row.TaskID] = row
	}
	for _, want := range []struct {
		taskID   int64
		progress int64
		status   string
	}{
		{autoAll.ID, int64(totalEvents), ProgressClaimed},
		{autoEarly.ID, int64(earlyAutoTarget), ProgressClaimed},
		{claimEarly.ID, int64(earlyClaimTarget), ProgressCompleted},
	} {
		row, ok := byTask[want.taskID]
		if !ok {
			t.Fatalf("任务 %d 缺少进度行", want.taskID)
		}
		if row.Progress != want.progress || row.Status != want.status {
			t.Fatalf("任务 %d 落库 progress=%d status=%s，期望 %d/%s",
				want.taskID, row.Progress, row.Status, want.progress, want.status)
		}
	}
}

// sampleDef 一个最小可用任务定义（ADD 口径、目标 1）。
func sampleDef(enabled int8) *TaskDef {
	return &TaskDef{
		ID: 1, Title: "签到", Desc: "每日签到", Trigger: "CHECKIN",
		Cycle: CycleDaily, GrantMode: GrantClaim, ProgressType: ProgressTypeAdd,
		TargetCount: 1, Enabled: enabled,
	}
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
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 2}); !errors.Is(err, ErrNotComplete) {
		t.Fatalf("2/5 未达标，got %v", err)
	}
	row := progressOf(t, def)
	if row == nil || row.Progress != 2 || row.Status != ProgressActive {
		t.Fatalf("覆盖后进度: %+v", row)
	}

	task = fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 5}); err != nil {
		t.Fatalf("5/5 应达标: %v", err)
	}
	row = progressOf(t, def)
	if row == nil || row.Progress != 5 || row.Status != ProgressCompleted {
		t.Fatalf("CLAIM 达标应 completed: %+v", row)
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
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	for i := 1; i <= 2; i++ {
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
			t.Fatalf("第 %d 次未达标，got %v", i, err)
		}
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("第 3 次应达标: %v", err)
	}
	row := progressOf(t, def)
	if row == nil || row.Progress != 3 || row.Status != ProgressClaimed {
		t.Fatalf("AUTO 达标应 claimed: %+v", row)
	}

	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("已结算应拒绝，got %v", err)
	}
	if got := progressOf(t, def).Progress; got != 3 {
		t.Fatalf("已结算后进度不得变化，实际 %d", got)
	}
}

// TestDefaultProgressType 验证 ProgressType 为空：Invite 非零优先覆盖，否则累加 Add。
func TestDefaultProgressType(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.ProgressType = ""
	def.TargetCount = 3
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	for i := 1; i <= 2; i++ {
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotComplete) {
			t.Fatalf("add 第 %d 次: %v", i, err)
		}
	}
	if got := progressOf(t, def).Progress; got != 2 {
		t.Fatalf("add 累计=%d", got)
	}
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmInvite: 3}); err != nil {
		t.Fatalf("invite 非零应覆盖并达标: %v", err)
	}
	if got := progressOf(t, def).Progress; got != 3 {
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
	e := newTestEngine(t, def)

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
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("TargetCount=0 应按 1，一次即达标: %v", err)
	}
}

// TestUpdateTaskGuards 覆盖 nil/缺定义/停用/窗口外/前置未完成等拒绝路径。
func TestUpdateTaskGuards(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t, sampleDef(1))

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
	resetStorage(t)
	de := newTestEngine(t, disabled)
	if err := de.UpdateTask(ctx, &TaskProgress{TaskID: disabled.ID}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}

	// 窗口外。
	past := util.CurrentTime().Add(-time.Hour)
	expired := sampleDef(1)
	expired.EndAt = &past
	resetStorage(t)
	ee := newTestEngine(t, expired)
	if err := ee.UpdateTask(ctx, &TaskProgress{TaskID: expired.ID}, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("窗口外: %v", err)
	}

	// 前置未完成 / 已完成：以前置任务的常驻状态为准。
	pre := &TaskDef{ID: 8, Cycle: CycleOnce, Enabled: 1, TargetCount: 1}
	gated := sampleDef(1)
	gated.PreTaskID = 8
	resetStorage(t)
	ge := newTestEngine(t, gated, pre)
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
	e := newTestEngine(t, a, b)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("on event: %v", err)
	}
	if progressOf(t, a) == nil || progressOf(t, b) != nil {
		t.Fatalf("前置未完成不应触发后继: a=%+v b=%+v", progressOf(t, a), progressOf(t, b))
	}
	if got := progressOf(t, a).Status; got != ProgressCompleted {
		t.Fatalf("CLAIM 应 completed，实际 %s", got)
	}

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("second event: %v", err)
	}
	if progressOf(t, b) == nil {
		t.Fatalf("前置完成后应推进后继: %+v", progressOf(t, b))
	}
}

// TestOnEventSkipsWindowAndEmptyParm 验证 nil/空 trigger/窗口外均不推进。
func TestOnEventSkipsWindowAndEmptyParm(t *testing.T) {
	ctx := context.Background()
	past := util.CurrentTime().Add(-time.Hour)
	expired := sampleDef(1)
	expired.EndAt = &past
	e := newTestEngine(t, expired)

	if err := e.OnEvent(ctx, nil); err != nil {
		t.Fatalf("nil parm: %v", err)
	}
	if err := e.OnEvent(ctx, &ProgressParm{}); err != nil {
		t.Fatalf("空 trigger: %v", err)
	}
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("窗口外不应报错: %v", err)
	}
	if progressOf(t, expired) != nil {
		t.Fatalf("窗口外不得推进: %+v", progressOf(t, expired))
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
	follower.Trigger = string(CompletedNotifyTrigger)
	follower.GrantMode = GrantAuto
	follower.TargetCount = 1

	e := newTestEngine(t, leader, follower)

	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("leader event: %v", err)
	}
	row := progressOf(t, follower)
	if row == nil || row.Status != ProgressClaimed {
		t.Fatalf("follower 应被完成通知推进: %+v", row)
	}
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("重复事件不应报错: %v", err)
	}
	if row := progressOf(t, follower); row == nil || row.Status != ProgressClaimed {
		t.Fatalf("完成通知不得改变终态，row=%+v", row)
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
	e := newTestEngine(t, claimDef, autoDef)

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

// TestGlobalDefsIndex 验证全局定义索引的写入、整体替换与单条回填。
func TestGlobalDefsIndex(t *testing.T) {
	a := &TaskDef{ID: 9001, Title: "A", Enabled: 1}
	b := &TaskDef{ID: 9002, Title: "B", Enabled: 1}

	// 整体替换：写入两条。
	taskglobal.setDefs([]TaskDef{*a, *b})
	if got, ok := taskglobal.getDef(9001); !ok || got.Title != "A" {
		t.Fatalf("getDef(9001)=%+v ok=%v", got, ok)
	}

	// 再替换一次：不在新切片里的旧 id 必须消失。
	taskglobal.setDefs([]TaskDef{*b})
	if _, ok := taskglobal.getDef(9001); ok {
		t.Fatal("setDefs 必须整体替换，旧定义不应残留")
	}

	// 单条回填；nil 是空操作。
	taskglobal.storeDef(a)
	if got, ok := taskglobal.getDef(9001); !ok || got.ID != 9001 {
		t.Fatalf("storeDef 后应可读: %+v ok=%v", got, ok)
	}
	taskglobal.storeDef(nil)
}

// TestNewRegistersEngine 验证 New 把引擎挂进全局引擎链。
func TestNewRegistersEngine(t *testing.T) {
	before := taskglobal.engineList.Length()
	e := newTestEngine(t, sampleDef(1))

	if got := taskglobal.engineList.Length(); got != before+1 {
		t.Fatalf("引擎注册数 %d → %d，应 +1", before, got)
	}
	found := false
	taskglobal.engineList.Range(func(n list.INode) bool {
		if n.GetData() == e {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatal("全局引擎链中应能按数据找到该引擎")
	}
}

// TestGetTaskReturnsResident 验证 GetTask 返回的就是引擎索引里的常驻对象。
func TestGetTaskReturnsResident(t *testing.T) {
	def := sampleDef(1)
	def.TargetCount = 3
	e := newTestEngine(t, def)

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
	e1 := newTestEngineFor(t, 1, def)
	e2 := newTestEngineFor(t, 2, def)

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
	setupStorage(t)
	seedProgress(t, TaskProgress{
		MemberID: 1, TaskID: def.ID, CycleKey: "2000-01-01",
		Progress: 2, Status: ProgressClaimed, SnapshotJSON: "{}",
	})
	e := newTestEngine(t, def)

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
	e := newTestEngine(t, def)

	if _, err := e.GetTask(ctx, 0, ""); err != nil {
		t.Fatal(err)
	}

	added := sampleDef(1)
	added.ID = 2
	// 换一个触发点，保证后面的 CHECKIN 事件只针对被停用的定义。
	added.Trigger = "OTHER"
	seedDefs(t, added)
	if err := taskglobal.Reload(ctx); err != nil {
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

	setDefEnabled(t, def, 0)
	if err := taskglobal.Reload(ctx); err != nil {
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
	if progressOf(t, def) != nil {
		t.Fatalf("停用定义不得推进: %+v", progressOf(t, def))
	}
}

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

// rewardLog 记录 RewardFn 每次收到的幂等键、任务快照与调用时进度行是否已存在。
type rewardLog struct {
	// keys 每次回调拿到的框架盖章幂等键。
	keys []RewardKey
	// rows 每次回调拿到的任务对象副本。
	rows []TaskProgress
	// existedAtCall 回调发生时该进度行是否已写入，用于验证「先发奖后落库」。
	existedAtCall []bool
}

// record 记下一次发奖回调。
func (l *rewardLog) record() RewardFn {
	return func(ctx context.Context, key RewardKey, p *TaskProgress) {
		cp := *p
		l.keys = append(l.keys, key)
		l.rows = append(l.rows, cp)
		row, err := loadProgressRow(ctx, p.MemberID, p.TaskID, p.CycleKey)
		l.existedAtCall = append(l.existedAtCall, err == nil && row != nil)
	}
}

// newRewardEngine 构造带发奖回调的引擎，并写入给定定义。
func newRewardEngine(t *testing.T, defs ...*TaskDef) (*Engine, *rewardLog) {
	t.Helper()
	setupStorage(t)
	seedDefs(t, defs...)
	var rl rewardLog
	e, err := New(Options{MemberID: 1, RewardFn: rl.record()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e, &rl
}

// TestUpdateTaskAutoCallsReward 验证 AUTO 达标先发奖后落库，已结算不再发奖。
func TestUpdateTaskAutoCallsReward(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 1
	def.Reward = `{"item":1}`
	e, rl := newRewardEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("auto update: %v", err)
	}
	if len(rl.rows) != 1 {
		t.Fatalf("Reward 调用 %d 次，应为 1", len(rl.rows))
	}
	got := rl.rows[0]
	if got.Progress != 1 || got.Status != ProgressClaimed {
		t.Fatalf("回调入参=%+v", got)
	}
	if got.GetDef() == nil || got.GetDef().Reward != def.Reward {
		t.Fatal("回调应能经定义索引读到奖励配置")
	}
	// 框架必须显式构造幂等键，且与任务对象的三列一致。
	wantKey := RewardKey{MemberID: got.MemberID, TaskID: got.TaskID, CycleKey: got.CycleKey}
	if len(rl.keys) != 1 || rl.keys[0] != wantKey {
		t.Fatalf("RewardKey 幂等键=%v，期望 %v", rl.keys, wantKey)
	}
	if rl.keys[0].String() != fmt.Sprintf("%d:%d:%s", wantKey.MemberID, wantKey.TaskID, wantKey.CycleKey) {
		t.Fatalf("幂等键字符串=%q", rl.keys[0].String())
	}
	if len(rl.existedAtCall) != 1 || rl.existedAtCall[0] {
		t.Fatalf("发奖应先于落库，调用时已有行=%v", rl.existedAtCall)
	}
	row := progressOf(t, def)
	if row == nil || row.Status != ProgressClaimed {
		t.Fatalf("应落 claimed: %+v", row)
	}

	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("已结算应拒绝，got %v", err)
	}
	if len(rl.rows) != 1 {
		t.Fatalf("已结算不得再次发奖，calls=%d", len(rl.rows))
	}
}

// TestClaimAndOnEventReward 验证 CLAIM 不发奖，OnEvent 的 AUTO 达标同样发奖。
func TestClaimAndOnEventReward(t *testing.T) {
	ctx := context.Background()

	t.Run("claim mode", func(t *testing.T) {
		def := sampleDef(1)
		def.TargetCount = 1
		e, rl := newRewardEngine(t, def)
		task := fetchTask(t, e, def.ID)
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
			t.Fatalf("claim update: %v", err)
		}
		if len(rl.rows) != 0 {
			t.Fatalf("CLAIM 不应发奖，calls=%d", len(rl.rows))
		}
		if row := progressOf(t, def); row == nil || row.Status != ProgressCompleted {
			t.Fatalf("CLAIM 应落 completed: %+v", row)
		}
	})

	t.Run("on event auto", func(t *testing.T) {
		def := sampleDef(1)
		def.GrantMode = GrantAuto
		def.TargetCount = 1
		e, rl := newRewardEngine(t, def)
		if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
			t.Fatalf("on event: %v", err)
		}
		if row := progressOf(t, def); row == nil || row.Status != ProgressClaimed {
			t.Fatalf("OnEvent AUTO 应落 claimed: %+v", row)
		}
		if len(rl.rows) != 1 {
			t.Fatalf("OnEvent AUTO 应发奖一次，calls=%d", len(rl.rows))
		}
		if len(rl.keys) != 1 || rl.keys[0].MemberID != 1 || rl.keys[0].TaskID != def.ID {
			t.Fatalf("OnEvent AUTO 应带幂等键，keys=%v", rl.keys)
		}
	})
}

// TestRewardNilSafe 验证未注入 RewardFn 时 AUTO 仍正常结算。
func TestRewardNilSafe(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 1
	e := newTestEngine(t, def)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("无回调不应报错: %v", err)
	}
	if row := progressOf(t, def); row == nil || row.Status != ProgressClaimed {
		t.Fatalf("应落 claimed: %+v", row)
	}
}

// 集成测试环境变量：全部提供才会执行依赖存储的用例，否则整组 Skip。
// MySQL 库名必须含 "test"，避免误连业务库。
//
// 这 7 个变量由 bench/run.ps1 -Integration 或 CI 在运行时注入；脚本只校验变量
// 是否存在，值不写入仓库任何文件。缺项时门禁应显式报错退出，不接受全 SKIP 绿灯。
// 完整说明见 task_engine/README.md「集成测试门禁」。
const (
	// envMySQLHost MySQL 地址（host:port）。
	envMySQLHost = "TASKENGINE_TEST_MYSQL_HOST"
	// envMySQLUser MySQL 用户名。
	envMySQLUser = "TASKENGINE_TEST_MYSQL_USER"
	// envMySQLPassword MySQL 密码。
	envMySQLPassword = "TASKENGINE_TEST_MYSQL_PASSWORD"
	// envMySQLDB MySQL 库名，必须含 "test"。
	envMySQLDB = "TASKENGINE_TEST_MYSQL_DB"
	// envRedisAddr Redis 地址。
	envRedisAddr = "TASKENGINE_TEST_REDIS_ADDR"
	// envRedisPassword Redis 密码，可空。
	envRedisPassword = "TASKENGINE_TEST_REDIS_PASSWORD"
	// envRedisDB Redis 库编号，缺省 0。
	envRedisDB = "TASKENGINE_TEST_REDIS_DB"
	// testKeyPrefix 测试用缓存键前缀，清理时按 *task_* 删除。
	testKeyPrefix = "tgtest"
)

// testEnv 集成测试连接参数。
type testEnv struct {
	// mysqlHost MySQL 地址。
	mysqlHost string
	// mysqlUser MySQL 用户名。
	mysqlUser string
	// mysqlPassword MySQL 密码。
	mysqlPassword string
	// mysqlDB MySQL 库名。
	mysqlDB string
	// redisAddr Redis 地址。
	redisAddr string
	// redisPassword Redis 密码。
	redisPassword string
	// redisDB Redis 库编号。
	redisDB int
}

// 集成测试基座状态：整个进程只初始化一次。
var (
	// setupOnce 保证缓存层与表绑定只做一次。
	setupOnce sync.Once
	// setupErr 首次初始化错误。
	setupErr error
	// resetTests 记录本次 testing.T 是否已重置过存储。
	resetTests sync.Map
	// testClient 测试共享的 MySQL 客户端。
	testClient *db.Client
	// testRedis 测试共享的 Redis 客户端。
	testRedis *redis.Client
)

// TestMain 在所有用例结束后卸载缓存层。
func TestMain(m *testing.M) {
	code := m.Run()
	db.CloseCache()
	os.Exit(code)
}

// loadTestEnv 读取集成测试环境变量；缺任一 MySQL 必填项时返回 false。
//
// 环境变量由 bench/run.ps1 -Integration / CI 在运行时注入，本文件不保存任何凭据值。
// 注意：密码允许空串、Redis DB 缺省 0，所以「变量是否已定义」与「值是否非空」是两回事，
// 门禁脚本按前者校验；这里按后者决定 Skip 是测试侧的既定行为，逻辑保持不变。
func loadTestEnv() (testEnv, bool) {
	env := testEnv{
		mysqlHost:     os.Getenv(envMySQLHost),
		mysqlUser:     os.Getenv(envMySQLUser),
		mysqlPassword: os.Getenv(envMySQLPassword),
		mysqlDB:       os.Getenv(envMySQLDB),
		redisAddr:     os.Getenv(envRedisAddr),
		redisPassword: os.Getenv(envRedisPassword),
	}
	if env.mysqlHost == "" || env.mysqlUser == "" || env.mysqlDB == "" || env.redisAddr == "" {
		return testEnv{}, false
	}
	if raw := os.Getenv(envRedisDB); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return testEnv{}, false
		}
		env.redisDB = n
	}
	return env, true
}

// setupStorage 初始化集成存储基座，并在每个用例首次调用时重置数据。
//
// 破坏性提示：首次初始化的 initStorage → recreateSchema 会 DROP TABLE 后重建
// task_progress / task_def（不是 TRUNCATE），随后每个用例首次进入的 resetStorage
// 会 TRUNCATE 两张表并清空 Redis 键。因此这组变量必须指向专用隔离库，库名护栏
// 要求含 "test"。完整门控说明见 task_engine/README.md「集成测试门禁」。
func setupStorage(t *testing.T) {
	t.Helper()
	env, ok := loadTestEnv()
	if !ok {
		t.Skipf("集成测试未配置：需要 %s/%s/%s/%s/%s", envMySQLHost, envMySQLUser, envMySQLDB, envRedisAddr, envRedisPassword)
	}
	// 库名护栏：只允许在测试库上跑，避免误删业务数据。
	if !strings.Contains(strings.ToLower(env.mysqlDB), "test") {
		t.Fatalf("%s 必须包含 test，当前 %q", envMySQLDB, env.mysqlDB)
	}
	setupOnce.Do(func() { setupErr = initStorage(env) })
	if setupErr != nil {
		t.Fatalf("初始化集成测试存储失败: %v", setupErr)
	}
	if _, done := resetTests.LoadOrStore(t, struct{}{}); done {
		return
	}
	resetStorage(t)
}

// initStorage 建客户端、重建表、初始化缓存层并绑定两张表。
func initStorage(env testEnv) error {
	client, err := db.NewMySql(&config.MySqlDBConfig{
		Host:     env.mysqlHost,
		Username: env.mysqlUser,
		Password: env.mysqlPassword,
		DBName:   env.mysqlDB,
	})
	if err != nil {
		return fmt.Errorf("连接 MySQL: %w", err)
	}
	if err := recreateSchema(client); err != nil {
		return err
	}
	rdb := redis.NewClient(&redis.Options{
		Addr:     env.redisAddr,
		Password: env.redisPassword,
		DB:       env.redisDB,
	})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return fmt.Errorf("连接 Redis: %w", err)
	}
	// 落库定时器拉长到 1 小时：用例内不自动 flush，断言优先读 Redis；
	// 需要 DB 真相时显式 db.FlushCache，避免定时器与断言竞争。
	cfg := db.CacheConfigFrom(&config.CacheConfig{
		Enabled:         true,
		KeyPrefix:       testKeyPrefix,
		FlushIntervalMS: 3600 * 1000,
	})
	if err := db.InitCache(cfg, rdb); err != nil {
		return fmt.Errorf("初始化缓存层: %w", err)
	}
	if err := OpenStorage(client); err != nil {
		return fmt.Errorf("绑定存储表: %w", err)
	}
	testClient = client
	testRedis = rdb
	return nil
}

// recreateSchema 删表后按 schema.sql 重建。
func recreateSchema(client *db.Client) error {
	ctx := context.Background()
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS task_progress",
		"DROP TABLE IF EXISTS task_def",
	} {
		if err := client.Raw(ctx, func(gx *gorm.DB) error { return gx.Exec(stmt).Error }); err != nil {
			return err
		}
	}
	data, err := os.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	for _, part := range strings.Split(string(data), ";") {
		stmt := stripSQLComments(part)
		if stmt == "" {
			continue
		}
		if err := client.Raw(ctx, func(gx *gorm.DB) error { return gx.Exec(stmt).Error }); err != nil {
			return fmt.Errorf("执行 %q: %w", stmt, err)
		}
	}
	return nil
}

// stripSQLComments 去掉整行注释并裁剪空白；纯注释块返回空串。
func stripSQLComments(stmt string) string {
	lines := strings.Split(stmt, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// resetStorage 清 Redis 键、清两张表、重置进程级定义索引与引擎链。
func resetStorage(t *testing.T) {
	t.Helper()
	clearRedisKeys(t)
	execSQL(t, "TRUNCATE TABLE task_progress")
	execSQL(t, "TRUNCATE TABLE task_def")
	taskglobal.defs.Clear()
	taskglobal.engineList = list.NewUnindexedList()
}

// clearRedisKeys 删除本包两张表相关的全部缓存键。
func clearRedisKeys(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var cursor uint64
	for {
		keys, next, err := testRedis.Scan(ctx, cursor, "*task_*", 200).Result()
		if err != nil {
			t.Fatalf("扫描 Redis 键: %v", err)
		}
		if len(keys) > 0 {
			if err := testRedis.Del(ctx, keys...).Err(); err != nil {
				t.Fatalf("删除 Redis 键: %v", err)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
}

// execSQL 执行一条测试 SQL。
func execSQL(t *testing.T, sql string) {
	t.Helper()
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Exec(sql).Error
	}); err != nil {
		t.Fatalf("执行 SQL %q: %v", sql, err)
	}
}

// seedDefs 写入任务定义（幂等：同 id 覆盖）。
func seedDefs(t *testing.T, defs ...*TaskDef) {
	t.Helper()
	setupStorage(t)
	now := time.Now().UTC()
	for _, d := range defs {
		if d == nil {
			continue
		}
		row := *d
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now
		}
		if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
		}); err != nil {
			t.Fatalf("写入任务定义 %d: %v", row.ID, err)
		}
	}
}

// seedProgress 直接写入进度行（幂等：同三列键覆盖）。
func seedProgress(t *testing.T, rows ...TaskProgress) {
	t.Helper()
	setupStorage(t)
	now := time.Now().UTC()
	for i := range rows {
		row := rows[i]
		if row.SnapshotJSON == "" {
			row.SnapshotJSON = "{}"
		}
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if row.UpdatedAt.IsZero() {
			row.UpdatedAt = now
		}
		if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
			return gx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
		}); err != nil {
			t.Fatalf("写入进度 member=%d task=%d: %v", row.MemberID, row.TaskID, err)
		}
	}
}

// newTestEngine 构造会员 1 的引擎，并写入给定定义。
func newTestEngine(t *testing.T, defs ...*TaskDef) *Engine {
	return newTestEngineFor(t, 1, defs...)
}

// newTestEngineFor 构造指定会员的引擎，并写入给定定义。
func newTestEngineFor(t *testing.T, memberID int64, defs ...*TaskDef) *Engine {
	t.Helper()
	setupStorage(t)
	seedDefs(t, defs...)
	return newEngineFor(t, memberID)
}

// newEngineFor 只构造引擎，不再重置或写定义。
func newEngineFor(t *testing.T, memberID int64) *Engine {
	t.Helper()
	e, err := New(Options{MemberID: memberID})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// progressAt 按三列键读进度行（Redis 优先）。
func progressAt(t *testing.T, memberID, taskID int64, cycleKey string) *TaskProgress {
	t.Helper()
	p, err := loadProgressRow(context.Background(), memberID, taskID, cycleKey)
	if err != nil {
		t.Fatalf("读进度 member=%d task=%d: %v", memberID, taskID, err)
	}
	return p
}

// progressOf 读会员 1 在给定定义当前周期的进度行。
func progressOf(t *testing.T, def *TaskDef) *TaskProgress {
	t.Helper()
	return progressAt(t, 1, def.ID, TaskCycleKey(def.Cycle, util.CurrentTime(), def.ID))
}

// flushProgress 把进度表脏键落库。
func flushProgress(t *testing.T) {
	t.Helper()
	if err := db.FlushCache(context.Background(), tableTaskProgress); err != nil {
		t.Fatalf("flush 进度: %v", err)
	}
}

// dbProgressRows 直读数据库中的进度行，供持久化用例断言。
func dbProgressRows(t *testing.T, memberID int64) []TaskProgress {
	t.Helper()
	var rows []TaskProgress
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Where("member_id = ?", memberID).Order("id ASC").Find(&rows).Error
	}); err != nil {
		t.Fatalf("查询进度行: %v", err)
	}
	return rows
}

// setDefEnabled 通过 UpdateDef（CacheQuery+CacheSave）更新启用状态。
func setDefEnabled(t *testing.T, def *TaskDef, enabled int8) {
	t.Helper()
	clone := *def
	clone.Enabled = enabled
	ok, err := UpdateDef(context.Background(), &clone, util.CurrentTime())
	if err != nil || !ok {
		t.Fatalf("更新定义 %d 失败: ok=%v err=%v", def.ID, ok, err)
	}
	def.Enabled = enabled
}

// dbDef 直读数据库中的定义行。
func dbDef(t *testing.T, id int64) *TaskDef {
	t.Helper()
	var rows []TaskDef
	if err := testClient.Raw(context.Background(), func(gx *gorm.DB) error {
		return gx.Where("id = ?", id).Find(&rows).Error
	}); err != nil {
		t.Fatalf("查询定义 %d: %v", id, err)
	}
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// TestProgressPersistsOnce 验证延迟写落库走唯一键 Upsert，同周期始终一行。
func TestProgressPersistsOnce(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	def.GrantMode = GrantAuto
	def.TargetCount = 3
	e := newTestEngine(t, def)

	for i := 1; i <= 3; i++ {
		if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
			t.Fatalf("第 %d 次事件: %v", i, err)
		}
	}
	flushProgress(t)
	rows := dbProgressRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("同周期应只有一行，实际 %d", len(rows))
	}
	if rows[0].Progress != 3 || rows[0].Status != ProgressClaimed || rows[0].ID == 0 {
		t.Fatalf("落库行=%+v", rows[0])
	}

	// 终态后不再推进，再次 flush 仍是一行。
	if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
		t.Fatalf("终态事件: %v", err)
	}
	flushProgress(t)
	if rows := dbProgressRows(t, 1); len(rows) != 1 {
		t.Fatalf("终态后不得新增行，实际 %d", len(rows))
	}
}

// TestCreateDefAndRead 验证 CreateDef 写入后可由 CacheQuery 回源读到。
func TestCreateDefAndRead(t *testing.T) {
	ctx := context.Background()
	newTestEngine(t, sampleDef(1))

	def := sampleDef(1)
	def.ID = 500
	def.Title = "新任务"
	def.Trigger = "OTHER"
	id, err := CreateDef(ctx, def, util.CurrentTime())
	if err != nil || id != 500 {
		t.Fatalf("CreateDef: id=%d err=%v", id, err)
	}
	got, err := loadDef(ctx, id)
	if err != nil || got == nil || got.Title != "新任务" {
		t.Fatalf("回读定义=%+v err=%v", got, err)
	}
	if dbDef(t, id) == nil {
		t.Fatal("定义应已落库")
	}

	// 行键依赖主键：缺 id 直接拒绝。
	noID := sampleDef(1)
	noID.ID = 0
	if _, err := CreateDef(ctx, noID, util.CurrentTime()); !errors.Is(err, db.ErrCacheKeyMissing) {
		t.Fatalf("缺主键应返回 ErrCacheKeyMissing，实际 %v", err)
	}
}

// TestUpdateDefWritesThroughCache 验证 UpdateDef 用 CacheSave 同步缓存与数据库。
func TestUpdateDefWritesThroughCache(t *testing.T) {
	ctx := context.Background()
	def := sampleDef(1)
	_ = newTestEngine(t, def)

	updated := *def
	updated.Title = "改过"
	updated.Enabled = 0
	ok, err := UpdateDef(ctx, &updated, util.CurrentTime())
	if err != nil || !ok {
		t.Fatalf("UpdateDef: ok=%v err=%v", ok, err)
	}
	got, err := loadDef(ctx, def.ID)
	if err != nil || got == nil || got.Title != "改过" || got.Enabled != 0 {
		t.Fatalf("缓存读回=%+v err=%v", got, err)
	}
	row := dbDef(t, def.ID)
	if row == nil || row.Title != "改过" || row.Enabled != 0 {
		t.Fatalf("数据库行=%+v", row)
	}
	// 不存在的 id 返回未命中。
	missing := *def
	missing.ID = 99999
	if ok, err := UpdateDef(ctx, &missing, util.CurrentTime()); err != nil || ok {
		t.Fatalf("缺失定义应返回 false: ok=%v err=%v", ok, err)
	}
}

// TestTaskCycleKey 覆盖 ONCE/DAILY/WEEKLY/SCHEDULED 四种周期键。
func TestTaskCycleKey(t *testing.T) {
	d := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) // Thursday
	if got := TaskCycleKey(CycleOnce, d, 9); got != "once" {
		t.Fatalf("ONCE %s", got)
	}
	if got := TaskCycleKey(CycleDaily, d, 1); got != "2026-10-15" {
		t.Fatalf("DAILY %s", got)
	}
	if got := TaskCycleKey(CycleWeekly, d, 1); got != "2026-W42" {
		t.Fatalf("WEEKLY %s", got)
	}
	if got := TaskCycleKey(CycleScheduled, d, 7); got != "sched:7" {
		t.Fatalf("SCHEDULED %s", got)
	}
}

// TestTargetReached 覆盖 > / = / <、空值、未知值与零阈值回退。
func TestTargetReached(t *testing.T) {
	cases := []struct {
		name     string
		progress int64
		target   float64
		op       string
		want     bool
	}{
		{"gt true", 3, 2, TargetCountTypeGT, true},
		{"gt false", 2, 2, TargetCountTypeGT, false},
		{"eq true", 2, 2, TargetCountTypeEQ, true},
		{"eq false", 3, 2, TargetCountTypeEQ, false},
		{"lt true", 1, 2, TargetCountTypeLT, true},
		{"lt false", 2, 2, TargetCountTypeLT, false},
		{"empty defaults eq", 2, 2, "", true},
		{"unknown defaults eq", 2, 2, "?", true},
		{"zero target falls back to 1", 1, 0, TargetCountTypeEQ, true},
	}
	for _, c := range cases {
		if got := TargetReached(c.progress, c.target, c.op); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestEffectiveTargetCount 验证 <=0 的阈值按 1 处理。
func TestEffectiveTargetCount(t *testing.T) {
	if got := EffectiveTargetCount(0); got != 1 {
		t.Fatalf("0 应按 1，实际 %v", got)
	}
	if got := EffectiveTargetCount(-3); got != 1 {
		t.Fatalf("负数应按 1，实际 %v", got)
	}
	if got := EffectiveTargetCount(2.5); got != 2.5 {
		t.Fatalf("正数应原样，实际 %v", got)
	}
}
