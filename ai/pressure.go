package ai

import (
	"sync"
	"time"
)

// rpmWindow RPM 滑动窗口长度
const rpmWindow = time.Minute

// pressureKey 模型压力统计键
type pressureKey struct {
	provider string
	model    string
}

// modelPressure 单个模型的压力计数：并发在途请求数 + RPM 滑动窗口
type modelPressure struct {
	mu       sync.Mutex
	inflight int       // 当前并发在途请求数
	window   []time.Time // 最近一分钟内的请求时间戳
}

// over 判断是否达到任一阈值（maxConcurrency/rpmLimit 任一 >0 才启用对应维度）
func (mp *modelPressure) over(maxConcurrency, rpmLimit int) bool {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.pruneLocked()
	if maxConcurrency > 0 && mp.inflight >= maxConcurrency {
		return true
	}
	if rpmLimit > 0 && len(mp.window) >= rpmLimit {
		return true
	}
	return false
}

// acquire 记录一次调用开始（并发 +1、记入 RPM 窗口），返回释放函数
func (mp *modelPressure) acquire() func() {
	mp.mu.Lock()
	mp.inflight++
	mp.window = append(mp.window, time.Now())
	mp.mu.Unlock()
	return func() {
		mp.mu.Lock()
		mp.inflight--
		mp.mu.Unlock()
	}
}

// pruneLocked 清理窗口外的过期时间戳。调用方需持有 mp.mu。
func (mp *modelPressure) pruneLocked() {
	cutoff := time.Now().Add(-rpmWindow)
	i := 0
	for i < len(mp.window) && mp.window[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		mp.window = append([]time.Time(nil), mp.window[i:]...)
	}
}

// PressureTracker 模型调用压力跟踪器（按 提供方+模型 维度统计）。
// 仅用于 cheapest 自动选型的溢出判断与调用计数；显式指定的调用只计数、不受限。
type PressureTracker struct {
	mu    sync.RWMutex
	items map[pressureKey]*modelPressure
}

// NewPressureTracker 创建压力跟踪器
func NewPressureTracker() *PressureTracker {
	return &PressureTracker{items: make(map[pressureKey]*modelPressure)}
}

// Reset 清空所有统计（Stop 时调用，避免残留状态影响下次启动）
func (t *PressureTracker) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.items = make(map[pressureKey]*modelPressure)
	t.mu.Unlock()
}

// item 取（或惰性创建）模型的压力计数器
func (t *PressureTracker) item(provider, model string) *modelPressure {
	k := pressureKey{provider, model}
	t.mu.RLock()
	mp := t.items[k]
	t.mu.RUnlock()
	if mp != nil {
		return mp
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if mp = t.items[k]; mp == nil {
		mp = &modelPressure{}
		t.items[k] = mp
	}
	return mp
}

// Overloaded 判断模型是否达到调用压力阈值（并发或 RPM 任一达到即超限）。
// 阈值均 <=0 时恒为 false（未启用压力控制）。
func (t *PressureTracker) Overloaded(provider, model string, maxConcurrency, rpmLimit int) bool {
	if t == nil || (maxConcurrency <= 0 && rpmLimit <= 0) {
		return false
	}
	return t.item(provider, model).over(maxConcurrency, rpmLimit)
}

// Acquire 记录一次调用开始（计入并发与 RPM 窗口），返回释放函数（调用结束必须执行）。
func (t *PressureTracker) Acquire(provider, model string) func() {
	if t == nil {
		return func() {}
	}
	return t.item(provider, model).acquire()
}
