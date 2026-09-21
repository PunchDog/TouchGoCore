package localtimer

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// S68 复核整改：上一轮遗留的消费协程不得消费新一轮的调度项。
//
// 现场：TimeStop 等待超时（有分片卡在业务 Tick 里）后把 timerRT 置 nil，下一次 Run
// 的 Swap 因此拿不到 old，谁都不等那条遗留协程。它醒来后按「通道快照重取」读到本轮
// 新建的通道数组，于是同一片出现两个消费者 —— 片内保序（S68 的全部依据）被破坏。
//
// 两道闸门各钉一条用例：
//   - 循环顶的生命周期检查：协程自己绝不会去读新通道；
//   - Run 起新轮之前先排干在途协程：新轮根本不会和旧协程并存。
// ============================================================================

// installScheduleChannels 把全局调度通道换成一片一条、容量 4 的测试数组
func installScheduleChannels(t *testing.T, shards int) []chan timerTask {
	t.Helper()
	TimeStop(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for liveConsumers.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := liveConsumers.Load(); n != 0 {
		t.Fatalf("✘ 前置条件：仍有 %d 个消费协程在途，换表会互相干扰", n)
	}
	resetTimerChannel()
	if got := SetScheduleShards(shards); got != shards {
		t.Fatalf("✘ 设置分片数失败: got=%d want=%d", got, shards)
	}
	ensureTimerChannel()
	channels := currentTimerChannels()
	if len(channels) != shards {
		t.Fatalf("✘ 通道数组长度不等于分片数: %d", len(channels))
	}
	t.Cleanup(func() {
		resetTimerChannel()
		SetScheduleShards(DefaultScheduleShards)
	})
	return channels
}

// TestStaleConsumerExitsInsteadOfAdoptingChannels 本轮生命周期已结束时，消费协程
// 必须在读到新通道之前退出：旧实现把「有新任务」和「本轮已结束」放在同一个 select 里，
// Go 在两个就绪分支之间随机挑一个，于是遗留协程有约一半概率消费新一轮的调度项。
//
// 判据用「重复 200 次、一次都没消费」而不是单次：单次在旧实现下有 50% 概率蒙对（红不起来），
// 而 200 次全对的概率是 2^-200。修复后每轮都在循环顶直接返回，绿色是确定的。
func TestStaleConsumerExitsInsteadOfAdoptingChannels(t *testing.T) {
	channels := installScheduleChannels(t, 1)
	ch := channels[0]

	rt := &timerRuntime{closech: make(chan struct{}), ctx: context.Background()}
	close(rt.closech) // 本轮已结束：TimeStop 的第一步就是 shutdown

	consumed := 0
	for i := 0; i < 200; i++ {
		ch <- timerTask{}
		timeTick(rt, 0)
		if len(ch) == 0 {
			consumed++
		}
	}
	if consumed != 0 {
		t.Fatalf("✘ 已结束生命周期的协程消费了新一轮的调度项 %d 次（同片双消费者，片内保序被破坏）", consumed)
	}
}

// TestRunWaitsForOrphanConsumer TimeStop 超时留下的孤儿协程必须在新一轮起之前离开。
func TestRunWaitsForOrphanConsumer(t *testing.T) {
	setupShards(t, 2)
	blocked := addTimerOnShard[*shardBlockTimer](t, 0, func(p *shardBlockTimer) {
		p.n.Store(0)
		p.arm = make(chan struct{})
		p.entered = make(chan struct{})
		p.release = make(chan struct{})
		p.blocked.Store(false)
	})
	defer func() {
		select {
		case <-blocked.release:
		default:
			close(blocked.release)
		}
	}()
	blockNow(t, blocked)

	// 超短预算：卡在业务 Tick 里的 0 号片协程必然等不到，成为跨代次的孤儿
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	TimeStop(ctx)
	cancel()
	if got := liveConsumers.Load(); got == 0 {
		t.Fatalf("✘ 前置条件不成立：被业务 Tick 卡住的协程已经退出，本用例失去意义（在途=%d）", got)
	}

	release := make(chan struct{})
	go func() {
		<-release
		close(blocked.release)
	}()

	runDone := make(chan struct{})
	go func() {
		Run(context.Background())
		close(runDone)
	}()

	select {
	case <-runDone:
		t.Fatal("✘ Run 未等待上一轮遗留的消费协程就起了新一轮（同片两个消费者）")
	case <-time.After(500 * time.Millisecond):
		// Run 仍在等孤儿：此刻放行，它应当从生命周期检查处退出而不是去读新通道
		close(release)
	}

	select {
	case <-runDone:
	case <-time.After(DefaultCloseBudget + 2*time.Second):
		t.Fatal("✘ 放行后 Run 仍未返回")
	}

	// 新轮协程是异步起手的，先等它们就位；再留一点时间让「如果存在」的孤儿协程
	// 走到它自己的下场（修复后它在放行瞬间就自行退出，修复前它会常驻消费新通道）。
	shards := scheduleShardCount()
	waitUntil(t, 5*time.Second, "新一轮消费协程没起来", func() bool {
		return int(liveConsumers.Load()) >= shards
	})
	time.Sleep(300 * time.Millisecond)
	if got := liveConsumers.Load(); int(got) != shards {
		t.Fatalf("✘ 停在 timeTick 里的协程数=%d 分片数=%d（多出的就是上一轮遗留、正在同抢一片的协程）", got, shards)
	}

	// 新一轮必须真的在消费：否则「等孤儿」把整台机器等停了
	probe := addTimerOnShard[*shardBeatTimer](t, 1, func(p *shardBeatTimer) { p.n.Store(0) })
	waitUntil(t, 5*time.Second, "新一轮没有消费者在跑", func() bool { return probe.n.Load() > 0 })
}
