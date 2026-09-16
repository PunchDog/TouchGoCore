package ai

import (
	"testing"
	"time"
)

func TestPressureConcurrency(t *testing.T) {
	tr := NewPressureTracker()
	release := tr.Acquire("a", "m")
	if !tr.Overloaded("a", "m", 1, 0) {
		t.Error("并发达到阈值应判定超限")
	}
	if tr.Overloaded("a", "m", 2, 0) {
		t.Error("并发未达阈值不应判定超限")
	}
	release()
	if tr.Overloaded("a", "m", 1, 0) {
		t.Error("释放后不应超限")
	}
}

func TestPressureRPMWindow(t *testing.T) {
	tr := NewPressureTracker()
	// 阈值均未启用时恒不超限
	if tr.Overloaded("a", "m", 0, 0) {
		t.Error("未启用压力控制不应超限")
	}
	r1 := tr.Acquire("a", "m")
	r2 := tr.Acquire("a", "m")
	defer r2()
	if tr.Overloaded("a", "m", 0, 3) {
		t.Error("RPM 未达阈值不应超限")
	}
	if !tr.Overloaded("a", "m", 0, 2) {
		t.Error("RPM 达到阈值应判定超限")
	}
	r1()
	// 将窗口内一个时间戳移到一分钟之前，应被清理
	tr.item("a", "m").mu.Lock()
	tr.item("a", "m").window[0] = time.Now().Add(-2 * time.Minute)
	tr.item("a", "m").mu.Unlock()
	if tr.Overloaded("a", "m", 0, 2) {
		t.Error("窗口过期条目清理后不应超限")
	}
	if !tr.Overloaded("a", "m", 0, 1) {
		t.Error("窗口内剩余 1 条请求达到 rpm_limit=1 应超限")
	}
}

func TestPressureIsolation(t *testing.T) {
	tr := NewPressureTracker()
	r := tr.Acquire("a", "m1")
	defer r()
	if !tr.Overloaded("a", "m1", 1, 0) {
		t.Error("m1 应超限")
	}
	if tr.Overloaded("a", "m2", 1, 0) || tr.Overloaded("b", "m1", 1, 0) {
		t.Error("不同模型/提供方之间压力统计不应相互影响")
	}
}

func TestPressureReset(t *testing.T) {
	tr := NewPressureTracker()
	r := tr.Acquire("a", "m")
	defer r()
	tr.Reset()
	if tr.Overloaded("a", "m", 1, 0) {
		t.Error("Reset 后不应残留超限状态")
	}
}
