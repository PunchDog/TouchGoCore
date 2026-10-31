package whatsapp

import (
	"testing"
	"time"
)

// TestTakeSendSlotRollingWindow 滚动窗口：窗口内第 N+1 条拒，最早一条滑出后放行。
//
// 这条闸的窗口来自配置 `send_rate`（`秒|次数`），所以窗口长度必须真的被用上 ——
// 写死 1 小时的话，「300|3」会变成 1 小时内 3 条，节奏慢 12 倍。
func TestTakeSendSlotRollingWindow(t *testing.T) {
	w := &waClientWrapper{}
	base := time.Now()

	if !w.takeSendSlot(base, 5*time.Minute, 3) {
		t.Fatal("第 1 条应放行")
	}
	if !w.takeSendSlot(base.Add(1*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 2 条应放行")
	}
	if !w.takeSendSlot(base.Add(2*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 3 条应放行")
	}
	if w.takeSendSlot(base.Add(3*time.Minute), 5*time.Minute, 3) {
		t.Fatal("第 4 条应被拒（5 分钟内已满 3 条）")
	}
	// 第 1 条（base）在 base+5min 之后滑出窗口，此时窗口内只剩 2 条。
	if !w.takeSendSlot(base.Add(5*time.Minute+time.Second), 5*time.Minute, 3) {
		t.Fatal("最早一条滑出后应放行")
	}
}

// TestTakeSendSlotDefaultRate 缺省速率 60 秒 1 条：窗口内第二条拒，满 60 秒后放行。
func TestTakeSendSlotDefaultRate(t *testing.T) {
	w := &waClientWrapper{}
	base := time.Now()
	if !w.takeSendSlot(base, time.Minute, 1) {
		t.Fatal("第 1 条应放行")
	}
	if w.takeSendSlot(base.Add(59*time.Second), time.Minute, 1) {
		t.Fatal("同一分钟内第 2 条应被拒")
	}
	if !w.takeSendSlot(base.Add(61*time.Second), time.Minute, 1) {
		t.Fatal("下一分钟应放行")
	}
}

// TestTakeSendSlotUnlimited window/max 非正读作不限（调用方已用 Effective* 归一，
// 这里只钉住「不会把所有消息都拒掉」这个安全方向）。
func TestTakeSendSlotUnlimited(t *testing.T) {
	w := &waClientWrapper{}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !w.takeSendSlot(now, 0, 0) {
			t.Fatalf("window/max 非正时应不限，第 %d 条被拒", i+1)
		}
	}
}
