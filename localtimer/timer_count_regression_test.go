package localtimer

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// 引用计数 -1 缺陷修复的回归验证。
//
// 修复 1（timer_manager.go AddTimer）：原先 parent.gen.Add(1) 丢弃返回值、
//   入队前再 parent.gen.Load()，两个并发 AddTimer 可读到同一 gen，产生两份
//   相同 gen 的 task，handleTimerAdd 的 isValid 全部放行 → 同一节点二次入链、
//   wheel.timerCount 多加，后续配对减完出现 -1。修复后直接使用 Add 返回值。
//
// 修复 2（timer.go HasNext）：原先 count==0 时无条件 Add(-1) 并存回，
//   GetRemainingCount() 从此返回 -1（触发场景：Init(interval, 0, ...)、
//   SetCount(0)、count 耗尽后业务重新 AddTimer 复活）。修复后 CAS 扣减，
//   c <= 0 直接返回 false 不再扣；count==1 扣到 0 返回 false 的语义不变。
//
// 运行方式：
//   go test ./localtimer -run 'TestRegression_HasNext|TestRegression_ConcurrentAddRemove' -v
//   go test -race -count=1 ./localtimer/
// ============================================================================

// ----------------------------------------------------------------------------
// 测试 A（确定性，针对修复 2）：count==0 时 HasNext 不得把 -1 存回。
// ----------------------------------------------------------------------------

// TestRegression_HasNext_ZeroCountNeverGoesNegative 覆盖三类触发场景：
//  1. Init(interval, 0, ...) 直接以 0 次数创建；
//  2. SetCount(0) 把运行中的定时器次数清零；
//  3. count==1 扣到 0 后返回 false 的原语义保持不变。
func TestRegression_HasNext_ZeroCountNeverGoesNegative(t *testing.T) {
	// 场景 1：Init(interval, 0, ...)
	tm0, err := NewTimer[*plainTimer](100, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm0.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ Init(count=0) 后剩余次数应为 0，实际 %d", got)
	}
	if tm0.HasNext() {
		t.Fatal("✘ count=0 的定时器 HasNext 应返回 false")
	}
	if got := tm0.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ count=0 调用 HasNext 后剩余次数被扣成 %d（期望仍为 0，修复前为 -1）", got)
	}

	// 场景 2：SetCount(0) 复验
	tm1, err := NewTimer[*plainTimer](100, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm1.SetCount(0)
	if got := tm1.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ SetCount(0) 后剩余次数应为 0，实际 %d", got)
	}
	if tm1.HasNext() {
		t.Fatal("✘ SetCount(0) 后 HasNext 应返回 false")
	}
	if got := tm1.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ SetCount(0) 后调用 HasNext 剩余次数被扣成 %d（期望仍为 0）", got)
	}

	// 场景 3：count==1 扣到 0 返回 false，原语义不变
	tm2, err := NewTimer[*plainTimer](100, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tm2.HasNext() {
		t.Fatal("✘ count=1 的定时器 HasNext 应扣减至 0 并返回 false")
	}
	if got := tm2.GetRemainingCount(); got != 0 {
		t.Fatalf("✘ count=1 扣减后剩余次数应为 0，实际 %d", got)
	}

	// 场景 4：无限次数定时器不受影响，仍返回 InfiniteCount 且 HasNext 为 true
	tm3, err := NewTimer[*plainTimer](100, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tm3.GetRemainingCount(); got != InfiniteCount {
		t.Fatalf("✘ 无限次数定时器剩余次数应为 %d，实际 %d", InfiniteCount, got)
	}
	if !tm3.HasNext() {
		t.Fatal("✘ 无限次数定时器 HasNext 应返回 true")
	}

	t.Log("✔ count=0 / SetCount(0) 时 HasNext 不再产生 -1，count==1 原语义保持")
}

// TestRegression_HasNext_ConcurrentDecrement_NeverNegative
// 并发 HasNext 竞争扣减：总扣减量不超过初始 count，剩余次数永不为负。
// （修复前的无条件 Add(-1) 在耗尽后会把 -1 存回。）
func TestRegression_HasNext_ConcurrentDecrement_NeverNegative(t *testing.T) {
	const initial = 100
	tm, err := NewTimer[*plainTimer](100, initial, nil)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				_ = tm.HasNext()
			}
		}()
	}
	wg.Wait()

	if got := tm.GetRemainingCount(); got < 0 {
		t.Fatalf("✘ 并发 HasNext 后剩余次数为负: %d", got)
	}
	t.Logf("✔ 并发扣减后剩余次数 %d（>= 0，总扣减未越界）", tm.GetRemainingCount())
}

// ----------------------------------------------------------------------------
// 测试 B（并发压力，针对修复 1）：AddTimer / Remove 并发竞态下
// GetTimerCount 恒 >= 0 且最终收敛为 0，无 panic、无死锁。
// ----------------------------------------------------------------------------

// TestRegression_ConcurrentAddRemoveChurn_TimerCountNeverNegative
//
// 模拟「TimeTick 续期协程 + 业务协程先 Remove 再 AddTimer 复活」的竞态：
// 每轮创建一个毫秒级无限次数定时器，两个 goroutine 分别对同一实例持续
// AddTimer 与 Remove+AddTimer，持续一段时间后移除，断言：
//   - 压测全程 GetTimerCount() >= 0（由看门狗协程持续采样）；
//   - 每轮结束后计数收敛归零；
//   - 无 panic（goroutine panic 会直接令测试失败）、无死锁（外层 -timeout 保护，
//     且每轮清理带 deadline 轮询）。
//
// 说明：gen 重复入链需要毫秒级时间窗对齐，直接断言「复现 -1」在修复后不可行；
// 本用例采用「长时间高频压测下计数恒非负且收敛为 0」的验证口径。
func TestRegression_ConcurrentAddRemoveChurn_TimerCountNeverNegative(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	// 计数看门狗：全程采样 GetTimerCount，记录最小值并标记是否出现过负数
	var (
		minCount  atomic.Int64
		negative  atomic.Bool
		stopWatch = make(chan struct{})
	 watchDone = make(chan struct{})
	)
	minCount.Store(math.MaxInt64)
	go func() {
		defer close(watchDone)
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			c := m.GetTimerCount()
			if c < 0 {
				negative.Store(true)
			}
			for {
				old := minCount.Load()
				if c >= old || minCount.CompareAndSwap(old, c) {
					break
				}
			}
			time.Sleep(500 * time.Microsecond)
		}
	}()

	const (
		rounds     = 200 // 轮数（>=200）
		churn      = 100 * time.Millisecond
		cleanupMax = 3 * time.Second
	)

	for r := 0; r < rounds; r++ {
		tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}

		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		// 协程 1：模拟 TimeTick 续期路径的 AddTimer（不含 Remove）
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = AddTimer(tm) // 通道满丢弃属正常背压
				}
			}
		}()
		// 协程 2：模拟业务重启路径的 Remove + AddTimer 复活
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					tm.Remove()
					_ = AddTimer(tm)
				}
			}
		}()

		time.Sleep(churn)
		close(stop)
		wg.Wait()

		// 移除并轮询等待计数收敛归零（带 deadline，防死锁式挂起）
		deadline := time.Now().Add(cleanupMax)
		for {
			tm.Remove()
			if m.GetTimerCount() == 0 {
				break
			}
			if time.Now().After(deadline) {
				close(stopWatch)
				<-watchDone
				t.Fatalf("✘ 第 %d 轮结束后定时器计数未归零: %d", r+1, m.GetTimerCount())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	close(stopWatch)
	<-watchDone

	if negative.Load() {
		t.Fatalf("✘ 压测期间 GetTimerCount 出现负数（最小值 %d）", minCount.Load())
	}
	if c := m.GetTimerCount(); c != 0 {
		t.Fatalf("✘ 测试结束后计数未归零: %d", c)
	}
	t.Logf("✔ %d 轮高频 AddTimer/Remove 竞态压测：计数全程最小值 %d（>=0），最终归零",
		rounds, minCount.Load())
}
