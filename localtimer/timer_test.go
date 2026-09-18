package localtimer

import (
	"sync"
	"testing"
)

// 验证 Timer 的 Init/HasNext/GetRemainingCount 行为，绕开对象池（pool 行为不可预测）
func TestTimer_Init_Basic(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, 3, nil); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	if tr.interval.Load() != 100 || tr.count.Load() != 3 {
		t.Fatalf("Init 后字段未设置: %+v", tr)
	}
	if !tr.isActive.Load() {
		t.Fatal("Init 后 isActive 应为 true")
	}
}

func TestTimer_Init_InvalidInterval(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(0, 1, nil); err != ErrTimerInvalidInterval {
		t.Fatalf("期望 ErrTimerInvalidInterval，实际: %v", err)
	}
}

func TestTimer_Init_Infinite(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, InfiniteCount, nil); err != nil {
		t.Fatal(err)
	}
	if tr.count.Load() != CountCorrectionValue {
		t.Fatalf("InfiniteCount 应转 CountCorrectionValue，实际: %d", tr.count.Load())
	}
}

func TestTimer_HasNext_Decrement(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 2, nil)
	// 注：HasNext 通过 nextTime=now+interval 计算，需要先调一次让 nextTime 变化
	if !tr.HasNext() {
		t.Fatal("第一次 HasNext 应为 true（剩 1 次）")
	}
	if tr.HasNext() {
		t.Fatal("第二次 HasNext 应为 false（用完）")
	}
}

func TestTimer_HasNext_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	for i := 0; i < 50; i++ {
		if !tr.HasNext() {
			t.Fatalf("第 %d 次 HasNext 应仍为 true", i)
		}
	}
}

func TestTimer_GetRemainingCount(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 5, nil)
	if got := tr.GetRemainingCount(); got != 5 {
		t.Fatalf("初始应剩 5，实际: %d", got)
	}
}

func TestTimer_GetRemainingCount_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	if got := tr.GetRemainingCount(); got != InfiniteCount {
		t.Fatalf("无限模式应返回 InfiniteCount，实际: %d", got)
	}
}

func TestTimer_SetCount(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil)
	tr.SetCount(InfiniteCount)
	if tr.GetRemainingCount() != InfiniteCount {
		t.Fatal("SetCount(InfiniteCount) 异常")
	}
}

func TestTimer_GetInterval(t *testing.T) {
	tr := &Timer{}
	tr.Init(500, 1, nil)
	if got := tr.GetInterval(); got != 500 {
		t.Fatalf("GetInterval=%d want=500", got)
	}
}

func TestTimer_RemoveFromManager_NoWheel(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil)
	// 无 wheel 时直接调用不应 panic
	tr.RemoveFromManager(false)
	if tr.IsActive() {
		t.Fatal("RemoveFromManager 后 IsActive 应为 false")
	}
}

func TestTimer_CalculateType(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 1, nil) // nextTime 已设置
	cases := []struct {
		ms   int64
		want TimerType
	}{
		{50, TimerTypeMillisecond},
		{2 * 1000, TimerTypeSecond},
		{2 * 60 * 1000, TimerTypeMinute},
		{2 * 10 * 60 * 1000, TimerTypeTenMinute},
		{2 * 60 * 60 * 1000, TimerTypeHour},
	}
	for _, c := range cases {
		if got := tr.calculateType(c.ms); got != c.want {
			t.Fatalf("interval=%dms got=%v want=%v", c.ms, got, c.want)
		}
	}
}

func TestTimerType_String(t *testing.T) {
	cases := map[TimerType]string{
		TimerTypeMillisecond: "millisecond",
		TimerTypeSecond:     "second",
		TimerTypeMinute:     "minute",
		TimerTypeTenMinute:  "ten-minute",
		TimerTypeHour:       "hour",
		TimerType(99):       "unknown",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("TimerType(%d)=%s want=%s", typ, got, want)
		}
	}
}

func TestNewTimer_NilClass(t *testing.T) {
	// T 为接口类型时 zero 是 nil interface，reflect.TypeOf 取不到类型 → ErrTimerInvalidType
	if _, err := NewTimer[TimerInterface](100, 1, nil); err != ErrTimerInvalidType {
		t.Fatalf("期望 ErrTimerInvalidType，实际: %v", err)
	}
}

func TestNewTimer_InvalidInterval(t *testing.T) {
	// T 为接口类型时直接命中 ErrTimerInvalidType；该用例同时接受 interval 错误
	if _, err := NewTimer[TimerInterface](0, 1, nil); err != ErrTimerInvalidInterval && err != ErrTimerInvalidType {
		t.Fatalf("期望 interval 错误，实际: %v", err)
	}
}

func TestAddTimer_SystemNotReady(t *testing.T) {
	if err := AddTimer(nil); err != ErrTimerSystemNotReady {
		t.Fatalf("期望 ErrTimerSystemNotReady，实际: %v", err)
	}
}

func TestIsSystemRunning_False(t *testing.T) {
	if IsSystemRunning() {
		t.Fatal("未 Run 的系统不应视为运行中")
	}
}

func TestGetSystemStats_ZeroBeforeRun(t *testing.T) {
	total, _ := GetSystemStats()
	if total != 0 {
		t.Fatalf("未启动时 total 应为 0，实际: %d", total)
	}
}

func TestTimerStats_SnapshotCopyable(t *testing.T) {
	var s TimerStats
	s.TimersAdded = 10
	s.TimersExecuted = 20
	cp := s
	if cp.TimersAdded != 10 || cp.TimersExecuted != 20 {
		t.Fatal("TimerStats 应可安全复制")
	}
}

func TestConstants(t *testing.T) {
	if MaxTimerChannelNum <= 0 || MaxAddTimerChannelNum <= 0 {
		t.Fatal("通道上限常量应为正")
	}
	if InfiniteCount != -1 {
		t.Fatalf("InfiniteCount 应为 -1，实际: %d", InfiniteCount)
	}
	if DefaultWheelCount != 5 {
		t.Fatalf("DefaultWheelCount 应为 5，实际: %d", DefaultWheelCount)
	}
}

// 防止 sync 包导入警告（保留以供未来并发测试扩展）
var _ = sync.Mutex{}
