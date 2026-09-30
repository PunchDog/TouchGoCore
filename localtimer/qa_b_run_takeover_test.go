package localtimer

// ============================================================================
// B-F1（槽位B）：Run 接管上一轮生命周期时，等待旧 tick 协程必须带预算。
//
// 缺陷形态：消费协程卡死在业务 Tick 里时直接再次 Run，旧实现在
// old.tickWG.Wait() 上永久挂死——与紧随其后的 waitConsumersDrain
// 「超时只报错、不阻塞本轮启动」的意图矛盾（TimeStop 同路径早有预算写法）。
// 修复后 Run 在 DefaultCloseBudget 内放弃等待并照常启动新一轮；孤儿协程醒来
// 会因 timeTick 循环顶的生命周期检查自行退出。
// ============================================================================

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// qaBBlockTimer arm 之后第一次 Tick 阻塞到 release 放行，用于占住消费协程。
type qaBBlockTimer struct {
	Timer
	arm     chan struct{}
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (b *qaBBlockTimer) Tick() {
	select {
	case <-b.arm:
		if b.blocked.CompareAndSwap(false, true) {
			close(b.entered)
			<-b.release
		}
	default:
	}
}

// qaBBeatTimer 只累加执行次数的观测探针
type qaBBeatTimer struct {
	Timer
	n atomic.Int64
}

func (c *qaBBeatTimer) Tick() { c.n.Add(1) }

// qaBWaitFor 轮询断言：在 d 内满足 cond 才算通过（自足实现，不依赖其它测试文件）
func qaBWaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("✘ %s（等待 %v 超时）", what, d)
}

// TestQaBRunTakeoverHasBudget 消费协程卡死在业务 Tick 时再次 Run：
//  1. Run 必须在预算内返回（旧实现永久挂死 → 本用例红）；
//  2. 新一轮必须正常工作（探针定时器照常 Tick）；
//  3. 放行后孤儿协程自行退出（liveConsumers 收敛到新轮协程数）。
func TestQaBRunTakeoverHasBudget(t *testing.T) {
	// 清场：确保从已知状态出发
	stopCtx, stopCancel := context.WithTimeout(context.Background(), DefaultCloseBudget)
	TimeStop(stopCtx)
	stopCancel()

	Run(context.Background())

	blocked, err := NewTimer[*qaBBlockTimer](5, InfiniteCount, func(p *qaBBlockTimer) {
		p.arm = make(chan struct{})
		p.entered = make(chan struct{})
		p.release = make(chan struct{})
		p.blocked.Store(false)
	})
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	if err := AddTimer(blocked); err != nil {
		t.Fatalf("✘ AddTimer 失败: %v", err)
	}

	// 武装并等它真的把消费协程占住
	close(blocked.arm)
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("✘ 前置条件：阻塞型定时器一直没被派发，占不住消费协程，本用例失去意义")
	}

	// 再次 Run：旧实现会永久挂在 old.tickWG.Wait() 上。
	// 修复后的耗时上界 = tickWG 预算 + waitConsumersDrain 预算 + 收尾余量。
	start := time.Now()
	runDone := make(chan struct{})
	go func() {
		Run(context.Background())
		close(runDone)
	}()
	select {
	case <-runDone:
		elapsed := time.Since(start)
		if limit := 2*DefaultCloseBudget + 5*time.Second; elapsed > limit {
			t.Fatalf("✘ Run 返回了但耗时 %v 超出预算上界 %v", elapsed, limit)
		}
		t.Logf("✔ 卡在业务 Tick 时重复 Run 于 %v 内返回（预算等待生效）", elapsed)
	case <-time.After(2*DefaultCloseBudget + 10*time.Second):
		close(blocked.release) // 解卡，别让泄漏的 Run 协程挂到进程结束
		t.Fatal("✘ 消费协程卡死在业务 Tick 时，重复 Run 被 old.tickWG.Wait() 永久挂死")
	}

	// 新一轮必须真的在工作
	probe, err := NewTimer[*qaBBeatTimer](5, InfiniteCount, func(p *qaBBeatTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	if err := AddTimer(probe); err != nil {
		t.Fatalf("✘ AddTimer 失败: %v", err)
	}
	qaBWaitFor(t, 5*time.Second, "新一轮 Run 之后探针定时器没有跑起来", func() bool {
		return probe.n.Load() > 0
	})
	probe.Pause()

	// 放行孤儿协程：它醒来后必须因生命周期检查自行退出，不消费新一轮调度项
	close(blocked.release)
	shards := scheduleShardCount()
	qaBWaitFor(t, 5*time.Second, "放行后孤儿消费协程未自行退出", func() bool {
		return liveConsumers.Load() == int64(shards)
	})

	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), DefaultCloseBudget)
	defer cleanCancel()
	TimeStop(cleanCtx)
}
