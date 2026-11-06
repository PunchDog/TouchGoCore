package taskengine

import (
	"testing"
	"time"
)

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
