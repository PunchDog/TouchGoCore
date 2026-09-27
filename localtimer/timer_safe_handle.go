package localtimer

import "sync/atomic"

// SafeHandle 是对 *Timer 的安全包装，在每次操作前校验定时器作废状态。
//
// 它将「作废契约」从注释约束升级为类型/返回值约束：一旦底层 Timer 被
// Pause()、Remove() 或自然耗尽，后续所有操作均返回 ErrTimerInvalidated
// 而非产生未定义行为。
//
// SafeHandle 是可选 API —— 时间轮内部派发路径不经过它，不影响热路径性能。
// 适用于业务侧持有 Timer 引用并需要安全操作的场景。
//
// 使用示例：
//
//	h := NewSafeHandle(timer)
//	if err := h.Pause(); err != nil {
//	    // timer 已作废
//	}
//	advanced, err := h.TryAdvance()
//	if err != nil { ... }
type SafeHandle struct {
	timer   *Timer
	removed atomic.Bool // 跟踪是否已调用过 Remove，防止重复归还
}

// NewSafeHandle 创建一个 SafeHandle，包装给定的 Timer。
// 如果 t 为 nil，返回的 SafeHandle 所有操作都将返回 ErrTimerInvalidated。
func NewSafeHandle(t *Timer) *SafeHandle {
	return &SafeHandle{timer: t}
}

// Timer 返回底层 *Timer 指针（只读访问，调用方应自行判断有效性）。
func (h *SafeHandle) Timer() *Timer {
	return h.timer
}

// IsValid 检查底层 Timer 是否仍有效（活跃且未归还池）。
// 注意：返回值仅为快照，不构成对后续操作的有效性保证——
// Timer 可能在 IsValid 返回 true 后立即被其他 goroutine Remove/Pause。
func (h *SafeHandle) IsValid() bool {
	if h.timer == nil {
		return false
	}
	return h.timer.isActive.Load() && !h.timer.released.Load()
}

// Err 若 Timer 已作废则返回 ErrTimerInvalidated，否则返回 nil。
func (h *SafeHandle) Err() error {
	if h.IsValid() {
		return nil
	}
	return ErrTimerInvalidated
}

// TryAdvance 安全地推进定时器到下一次执行。
// 若 Timer 已作废，返回 (false, ErrTimerInvalidated)。
func (h *SafeHandle) TryAdvance() (bool, error) {
	if !h.IsValid() {
		return false, ErrTimerInvalidated
	}
	return h.timer.TryAdvance(), nil
}

// Pause 安全地暂停定时器。
// 若 Timer 已作废，返回 ErrTimerInvalidated 而非产生副作用。
func (h *SafeHandle) Pause() error {
	if !h.IsValid() {
		return ErrTimerInvalidated
	}
	h.timer.Pause()
	return nil
}

// Remove 安全地作废并归还定时器。
// 若 Timer 已作废/已归还/已经调用过 Remove，返回 ErrTimerInvalidated（幂等安全）。
// 使用 CAS 单一裁决：即使多个 goroutine 并发调用，也只有一个能成功执行 Remove。
func (h *SafeHandle) Remove() error {
	if h.timer == nil {
		return ErrTimerInvalidated
	}
	if h.timer.released.Load() {
		return ErrTimerInvalidated
	}
	// CAS 保证 check-then-act 原子性：只有一个调用者能赢得裁决
	if !h.removed.CompareAndSwap(false, true) {
		return ErrTimerInvalidated
	}
	h.timer.Remove()
	return nil
}

// IsActive 安全地查询活跃状态。
// 若 Timer 已作废，返回 (false, ErrTimerInvalidated)。
func (h *SafeHandle) IsActive() (bool, error) {
	if !h.IsValid() {
		return false, ErrTimerInvalidated
	}
	return h.timer.IsActive(), nil
}

// GetUID 安全地获取 UID。
// 若 Timer 已作废，返回 (0, ErrTimerInvalidated)。
func (h *SafeHandle) GetUID() (int64, error) {
	if !h.IsValid() {
		return 0, ErrTimerInvalidated
	}
	return h.timer.GetUID(), nil
}
