package localtimer

import (
	"errors"
	"testing"
)

// ========== TryAdvance 推进与改期语义 ==========

func TestTryAdvance_DecrementFinite(t *testing.T) {
	tr := &Timer{}
	if err := tr.Init(100, 3, nil); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// count=3: 第一次推进 → true (剩2)
	if !tr.TryAdvance() {
		t.Fatal("TryAdvance #1 应为 true")
	}
	if tr.count.Load() != 2 {
		t.Fatalf("TryAdvance #1 后 count 应为 2, got %d", tr.count.Load())
	}

	// count=2: 第二次推进 → true (剩1)
	if !tr.TryAdvance() {
		t.Fatal("TryAdvance #2 应为 true")
	}
	if tr.count.Load() != 1 {
		t.Fatalf("TryAdvance #2 后 count 应为 1, got %d", tr.count.Load())
	}

	// count=1: 第三次推进 → false (最后一次，扣到0后返回false)
	if tr.TryAdvance() {
		t.Fatal("TryAdvance #3 应为 false (最后一次)")
	}
	if tr.count.Load() != 0 {
		t.Fatalf("TryAdvance #3 后 count 应为 0, got %d", tr.count.Load())
	}

	// 再调用 → false，不会变为负数
	if tr.TryAdvance() {
		t.Fatal("TryAdvance #4 应为 false")
	}
	if tr.count.Load() != 0 {
		t.Fatalf("count 不应为负, got %d", tr.count.Load())
	}
}

func TestTryAdvance_Infinite(t *testing.T) {
	tr := &Timer{}
	tr.Init(50, InfiniteCount, nil)

	for i := 0; i < 100; i++ {
		if !tr.TryAdvance() {
			t.Fatalf("TryAdvance 第 %d 次应为 true (无限次)", i)
		}
	}
	// count 不变（仍为 CountCorrectionValue）
	if tr.count.Load() != CountCorrectionValue {
		t.Fatalf("无限次 count 应保持 CountCorrectionValue, got %d", tr.count.Load())
	}
}

func TestTryAdvance_UpdatesNextTime(t *testing.T) {
	tr := &Timer{}
	tr.Init(200, InfiniteCount, nil)

	// 显式设置一个过期的 nextTime，确保 TryAdvance 后值被推进
	tr.nextTime.Store(1)

	tr.TryAdvance()
	after := tr.nextTime.Load()

	// nextTime 应被推进到当前时间+interval 的附近
	if after <= 1 {
		t.Fatalf("nextTime 应被推进: got %d", after)
	}
}

func TestTryAdvance_InactiveReturnsFalse(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	tr.isActive.Store(false) // 模拟已暂停

	if tr.TryAdvance() {
		t.Fatal("isActive=false 时 TryAdvance 应返回 false")
	}
}

// ========== HasNext 旧行为不回归 ==========

func TestHasNext_DelegatesToTryAdvance(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 2, nil)

	// HasNext 应与 TryAdvance 行为一致
	if !tr.HasNext() {
		t.Fatal("HasNext #1 应为 true")
	}
	if tr.HasNext() {
		t.Fatal("HasNext #2 应为 false (count耗尽)")
	}
}

func TestHasNext_Infinite_NoRegression(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)

	for i := 0; i < 50; i++ {
		if !tr.HasNext() {
			t.Fatalf("HasNext 第 %d 次应为 true", i)
		}
	}
}

func TestHasNext_Inactive_NoRegression(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 5, nil)
	tr.isActive.Store(false)

	if tr.HasNext() {
		t.Fatal("isActive=false 时 HasNext 应返回 false")
	}
}

// ========== SafeHandle 作废后操作返回明确错误 ==========

func TestSafeHandle_NilTimer(t *testing.T) {
	h := NewSafeHandle(nil)

	if h.IsValid() {
		t.Fatal("nil Timer 的 SafeHandle 应无效")
	}
	if !errors.Is(h.Err(), ErrTimerInvalidated) {
		t.Fatal("Err() 应返回 ErrTimerInvalidated")
	}

	ok, err := h.TryAdvance()
	if ok || !errors.Is(err, ErrTimerInvalidated) {
		t.Fatal("TryAdvance on nil handle 应返回 error")
	}
	if err := h.Pause(); !errors.Is(err, ErrTimerInvalidated) {
		t.Fatal("Pause on nil handle 应返回 error")
	}
	if err := h.Remove(); !errors.Is(err, ErrTimerInvalidated) {
		t.Fatal("Remove on nil handle 应返回 error")
	}
	if _, err := h.IsActive(); !errors.Is(err, ErrTimerInvalidated) {
		t.Fatal("IsActive on nil handle 应返回 error")
	}
	if _, err := h.GetUID(); !errors.Is(err, ErrTimerInvalidated) {
		t.Fatal("GetUID on nil handle 应返回 error")
	}
}

func TestSafeHandle_ActiveTimer(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	tr.uid.Store(42)

	h := NewSafeHandle(tr)

	if !h.IsValid() {
		t.Fatal("活跃 Timer 的 SafeHandle 应有效")
	}
	if h.Err() != nil {
		t.Fatal("Err() 应为 nil")
	}

	ok, err := h.TryAdvance()
	if err != nil {
		t.Fatalf("TryAdvance: %v", err)
	}
	if !ok {
		t.Fatal("TryAdvance 应返回 true")
	}

	active, err := h.IsActive()
	if err != nil || !active {
		t.Fatal("IsActive 应返回 (true, nil)")
	}

	uid, err := h.GetUID()
	if err != nil || uid != 42 {
		t.Fatalf("GetUID 应返回 (42, nil), got (%d, %v)", uid, err)
	}
}

func TestSafeHandle_AfterPause(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)

	h := NewSafeHandle(tr)
	if !h.IsValid() {
		t.Fatal("初始应有效")
	}

	// Pause 通过 SafeHandle
	if err := h.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Pause 后 SafeHandle 应报告无效
	if h.IsValid() {
		t.Fatal("Pause 后 SafeHandle 应无效")
	}

	_, err := h.TryAdvance()
	if !errors.Is(err, ErrTimerInvalidated) {
		t.Fatalf("Pause 后 TryAdvance 应返回 ErrTimerInvalidated, got %v", err)
	}

	err = h.Pause()
	if !errors.Is(err, ErrTimerInvalidated) {
		t.Fatalf("Pause 后再次 Pause 应返回 ErrTimerInvalidated, got %v", err)
	}
}

func TestSafeHandle_AfterRemove(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, 5, nil)
	tr.fromPool.Store(true) // Remove 需要 fromPool=true 才真正走 Put

	h := NewSafeHandle(tr)
	if !h.IsValid() {
		t.Fatal("初始应有效")
	}

	// 通过 SafeHandle Remove
	if err := h.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Remove 后 SafeHandle 应报告无效
	if h.IsValid() {
		t.Fatal("Remove 后 SafeHandle 应无效")
	}

	_, err := h.TryAdvance()
	if !errors.Is(err, ErrTimerInvalidated) {
		t.Fatalf("Remove 后 TryAdvance 应返回 ErrTimerInvalidated, got %v", err)
	}
}

func TestSafeHandle_DoubleRemove(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)
	tr.fromPool.Store(true)

	h := NewSafeHandle(tr)

	// 第一次 Remove 成功
	if err := h.Remove(); err != nil {
		t.Fatalf("第一次 Remove: %v", err)
	}

	// 第二次 Remove 应返回 ErrTimerInvalidated（released 粘性标记）
	err := h.Remove()
	if !errors.Is(err, ErrTimerInvalidated) {
		t.Fatalf("第二次 Remove 应返回 ErrTimerInvalidated, got %v", err)
	}
}

func TestSafeHandle_TimerAccessor(t *testing.T) {
	tr := &Timer{}
	tr.Init(100, InfiniteCount, nil)

	h := NewSafeHandle(tr)
	if h.Timer() != tr {
		t.Fatal("Timer() 应返回底层指针")
	}
}
