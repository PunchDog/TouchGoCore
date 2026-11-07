package taskengine

import (
	"context"
	"errors"
	"testing"
)

// rewardLog 记录 rewardFn 每次收到的任务快照与调用时进度行是否已存在。
type rewardLog struct {
	// rows 每次回调拿到的任务对象副本。
	rows []TaskProgress
	// existedAtCall 回调发生时该进度行是否已写入，用于验证「先发奖后落库」。
	existedAtCall []bool
}

// record 记下一次发奖回调。
func (l *rewardLog) record() func(*TaskProgress) {
	return func(p *TaskProgress) {
		cp := *p
		l.rows = append(l.rows, cp)
		row, err := loadProgressRow(context.Background(), p.MemberID, p.TaskID, p.CycleKey)
		l.existedAtCall = append(l.existedAtCall, err == nil && row != nil)
	}
}

// newRewardEngine 构造带发奖回调的引擎，并写入给定定义。
func newRewardEngine(t *testing.T, defs ...*TaskDef) (*Engine, *rewardLog) {
	t.Helper()
	setupStorage(t)
	seedDefs(t, defs...)
	var rl rewardLog
	e, err := New(Options{memberID: 1, rewardFn: rl.record()})
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
	})
}

// TestRewardNilSafe 验证未注入 rewardFn 时 AUTO 仍正常结算。
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
