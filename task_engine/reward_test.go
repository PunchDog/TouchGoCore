package taskengine

import (
	"context"
	"errors"
	"testing"
)

// rewardLog 记录 rewardFn 每次收到的任务快照与调用时库内已有行数。
type rewardLog struct {
	// rows 每次回调拿到的任务对象副本。
	rows []TaskProgress
	// rowsAtCall 回调发生时 store 里的进度行数，用于验证「先发奖后落库」。
	rowsAtCall []int
}

// record 记下一次发奖回调。
func (l *rewardLog) record(store *fakeStore) func(*TaskProgress) {
	return func(p *TaskProgress) {
		cp := *p
		l.rows = append(l.rows, cp)
		l.rowsAtCall = append(l.rowsAtCall, len(store.rows))
	}
}

// newRewardEngine 构造带发奖回调的引擎。
func newRewardEngine(t *testing.T, store *fakeStore) (*Engine, *rewardLog) {
	t.Helper()
	var rl rewardLog
	e, err := New(Options{Store: store, memberID: 1, rewardFn: rl.record(store)})
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
	store := newFakeStore(def)
	e, rl := newRewardEngine(t, store)

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
	if len(rl.rowsAtCall) != 1 || rl.rowsAtCall[0] != 0 {
		t.Fatalf("发奖应先于落库，库内行数=%v", rl.rowsAtCall)
	}
	rows := rowsFor(store, def.ID)
	if len(rows) != 1 || rows[0].Status != ProgressClaimed {
		t.Fatalf("应落 claimed: %+v", rows)
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
		store := newFakeStore(def)
		e, rl := newRewardEngine(t, store)
		task := fetchTask(t, e, def.ID)
		if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
			t.Fatalf("claim update: %v", err)
		}
		if len(rl.rows) != 0 {
			t.Fatalf("CLAIM 不应发奖，calls=%d", len(rl.rows))
		}
		if got := rowsFor(store, def.ID)[0].Status; got != ProgressCompleted {
			t.Fatalf("CLAIM 应落 completed，实际 %s", got)
		}
	})

	t.Run("on event auto", func(t *testing.T) {
		def := sampleDef(1)
		def.GrantMode = GrantAuto
		def.TargetCount = 1
		store := newFakeStore(def)
		e, rl := newRewardEngine(t, store)
		if err := e.OnEvent(ctx, addParm("CHECKIN")); err != nil {
			t.Fatalf("on event: %v", err)
		}
		if got := rowsFor(store, def.ID)[0].Status; got != ProgressClaimed {
			t.Fatalf("OnEvent AUTO 应落 claimed，实际 %s", got)
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
	store := newFakeStore(def)
	e := newTestEngine(t, store)

	task := fetchTask(t, e, def.ID)
	if err := e.UpdateTask(ctx, task, &ProgressParm{ProgressParmAdd: 1}); err != nil {
		t.Fatalf("无回调不应报错: %v", err)
	}
	if got := rowsFor(store, def.ID)[0].Status; got != ProgressClaimed {
		t.Fatalf("应落 claimed，实际 %s", got)
	}
}
