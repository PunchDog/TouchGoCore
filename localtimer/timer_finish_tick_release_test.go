package localtimer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// reentrantHasNextTimer 在 HasNext 里复刻「Tick 收尾与业务 Pause+AddTimer 重连交错」的窗口。
//
// 真实竞态：finishTimerTick 以锁外方式先读 HasNext、再读 IsActive；业务若在两次读
// 之间完成 Pause+AddTimer，会留下「HasNext 看到不活跃、IsActive 看到活跃、代次已
// 推进」的组合。旧实现把它当作自然耗尽并把实例归还对象池，重连后的定时器就此丢失。
//
// hook 用真实 Pause/AddTimer 制造该状态；HasNext 返回 false 只是复刻「锁外那次读取
// 落在不活跃窗口里」的观测结果，使窗口确定化。
type reentrantHasNextTimer struct {
	// Timer 提供其余接口实现。
	Timer
	// hook 在 HasNext 返回前执行，模拟业务重连。
	hook func()
	// hookErr 记录 hook 内 AddTimer 的错误文本。
	hookErr atomic.Value
}

// Tick 本用例不触发回调。
func (p *reentrantHasNextTimer) Tick() {}

// HasNext 先执行重连 hook，再返回 false 表示「锁外这次读取看到不活跃」。
func (p *reentrantHasNextTimer) HasNext() bool {
	if p.hook != nil {
		p.hook()
	}
	return false
}

// TestFinishTimerTick_ReconnectDuringFinishNotReleased 回归：Tick 收尾瞬间业务执行
// Pause+AddTimer 重连时，旧代次的调度项不得把已重挂的实例归还对象池。
func TestFinishTimerTick_ReconnectDuringFinishNotReleased(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	tm, err := NewTimer[*reentrantHasNextTimer](60_000, InfiniteCount, func(p *reentrantHasNextTimer) {
		p.hook = func() {
			// 业务重连的真实写法：先停调度，再按当前状态重新挂回。
			p.Pause()
			if err := AddTimer(p); err != nil {
				p.hookErr.Store(err.Error())
			}
		}
	})
	if err != nil {
		t.Fatalf("new timer: %v", err)
	}

	parent := tm.GetParent()
	task := timerTask{timer: tm, gen: parent.gen.Load(), mgr: m}
	// 模拟 executeTimer：先登记在飞，再走收尾路径。
	if !parent.beginTick(task.gen) {
		t.Fatal("beginTick 应成功")
	}

	before := GetTimerPoolStats().Puts
	m.finishTimerTick(task, parent)

	if v := tm.hookErr.Load(); v != nil {
		t.Fatalf("重连 AddTimer 失败: %v", v)
	}
	if parent.released.Load() || parent.inPool.Load() {
		t.Fatalf("并发重连后不得归还对象池: released=%v inPool=%v", parent.released.Load(), parent.inPool.Load())
	}
	if delta := GetTimerPoolStats().Puts - before; delta != 0 {
		t.Fatalf("不应发生任何归还，Puts 增量=%d", delta)
	}
	// 实例仍在调用方手里，正常作废收尾应能回池。
	tm.Remove()
	if delta := GetTimerPoolStats().Puts - before; delta != 1 {
		t.Fatalf("收尾 Remove 应恰好归还一次，Puts 增量=%d", delta)
	}
}

// reconnectTickTimer 每次 Tick 都执行一次 Pause+AddTimer，模拟断线重连。
type reconnectTickTimer struct {
	// Timer 提供其余接口实现。
	Timer
	// n 已执行的回调次数。
	n atomic.Int64
	// errText 记录 AddTimer 的错误文本。
	errText atomic.Value
}

// Tick 先计数，再按业务重连写法重挂自身。
func (p *reconnectTickTimer) Tick() {
	p.n.Add(1)
	p.Pause()
	if err := AddTimer(p); err != nil {
		p.errText.Store(err.Error())
	}
}

// TestTickReconnectKeepsScheduling 验证 Tick 内 Pause+AddTimer 的重连路径不会
// 让定时器停摆，也不会返回 ErrTimerReleased。
func TestTickReconnectKeepsScheduling(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*reconnectTickTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatalf("new timer: %v", err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatalf("AddTimer: %v", err)
	}

	waitUntil(t, 3*time.Second, "Tick 内 Pause+AddTimer 后定时器应继续触发", func() bool {
		return tm.n.Load() >= 10
	})
	if v := tm.errText.Load(); v != nil {
		t.Fatalf("重连 AddTimer 失败: %v", v)
	}
	tm.Remove()
}
