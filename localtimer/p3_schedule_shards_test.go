package localtimer

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================================
// S68：调度通道分片消费。
//
// 协议要点（逐条钉住）：
//   - 分片数在调度通道建立那一刻定死，且消费者协程数与通道数严格相等；
//   - 同一 uid 恒定落同一片（片内保序 ⟹ 同一定时器绝不并发执行）；
//   - 各分片容量之和 = MaxTimerChannelNum（放开消费者不该顺带放大允许的积压量）；
//   - 一片卡在业务 Tick 里，只拖住那一片；某片消费协程 panic 独死时只降级、
//     不摘除全局 runtime（否则存活分片既没人消费也没人收尾）。
// ============================================================================

// shardBlockTimer 在 arm 之后第一次 Tick 阻塞到测试放行，用于占住所在分片的消费协程。
//
// 「先造后武装」是必需的：为了把定时器钉到指定分片，本文件会反复 AddTimer 试探，
// 落错片的那些会被立刻 Pause 掉。若 Tick 一上来就阻塞，第一个试探成功的实例就会
// 把某个分片的消费者永久占住（放行通道还不存在），测试变成自己造的 deadlock。
type shardBlockTimer struct {
	Timer
	n       atomic.Int64
	arm     chan struct{}
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (b *shardBlockTimer) Tick() {
	b.n.Add(1)
	select {
	case <-b.arm:
		if b.blocked.CompareAndSwap(false, true) {
			close(b.entered)
			<-b.release
		}
	default:
	}
}

// shardBeatTimer 只累加执行次数，充当「被邻居拖累」的观测探针
type shardBeatTimer struct {
	Timer
	n atomic.Int64
}

func (c *shardBeatTimer) Tick() { c.n.Add(1) }

// setupShards 把系统切到 n 片并启动。
//
// 分片数只在「通道还不存在」时可改（SetScheduleShards 的拒绝语义），所以必须
// 先 TimeStop 释放通道再设置；收尾同样恢复默认单片，避免污染同包其他用例
// （它们都按「一条通道、0 号片即全部」写）。
func setupShards(t *testing.T, n int) {
	t.Helper()
	TimeStop(context.Background())
	resetTimerChannel()
	want := n
	if n > MaxScheduleShards {
		want = MaxScheduleShards
	}
	if n <= 0 {
		want = DefaultScheduleShards
	}
	if got := SetScheduleShards(n); got != want {
		t.Fatalf("✘ 设置分片数未生效: got=%d want=%d", got, want)
	}
	t.Cleanup(func() {
		// 带截止的 ctx：TimeStop(Background) 在无预算时被一个仍卡在业务 Tick 里的
		// 消费协程挂住，CI 上表现为整包超时而不是这条用例失败。
		ctx, cancel := context.WithTimeout(context.Background(), DefaultCloseBudget)
		defer cancel()
		TimeStop(ctx)
		resetTimerChannel()
		SetScheduleShards(DefaultScheduleShards)
		// 显式确认恢复：分片数泄漏到别的用例时，那些按「0 号片即全部」写的用例会
		// 以「协程泄漏/通道永远不空」的形式失败，排查成本远高于这一行断言。
		if got := ScheduleShards(); got != DefaultScheduleShards {
			t.Errorf("✘ 收尾未恢复默认单片: got=%d", got)
		}
	})
	Run(context.Background())
	if got := scheduleShardCount(); got != want {
		t.Fatalf("✘ 生效分片数不对: got=%d want=%d", got, want)
	}
}

// addTimerOnShard 造一个 10ms 间隔的无限次定时器并投入调度，直到它落在 wantShard 片。
//
// uid 是在 AddTimer 里才分配的（见 handleTimerAdd 前的「没有分配时，分配唯一ID」），
// 所以落哪一片只能在入队之后判定；落错片的实例当场 Pause 停掉调度后丢弃，
// 只保留它消耗掉的那个 uid。init 必须把业务字段复位：实例来自对象池。
func addTimerOnShard[T TimerInterface](t *testing.T, wantShard int, init func(T)) T {
	t.Helper()
	var zero T
	channels := currentTimerChannels()
	shards := len(channels)
	for i := 0; i < 200; i++ {
		tm, err := NewTimer[T](10, InfiniteCount, init)
		if err != nil {
			t.Fatalf("✘ NewTimer 失败: %v", err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatalf("✘ AddTimer 失败: %v", err)
		}
		if shardOfTest(tm.GetUID(), shards) == wantShard {
			return tm
		}
		tm.Pause()
	}
	t.Fatalf("✘ 连续 200 个 uid 都没落到 %d 号分片（共 %d 片）", wantShard, shards)
	return zero
}

func shardOfTest(uid int64, shards int) int {
	if uid < 0 {
		uid = -uid
	}
	return int(uid % int64(shards))
}

// TestShardRoutingStableAndSpread 钉住路由本身：同 uid 恒定、uid 铺满各片、
// 空通道快照不 panic（未 Run 时轮协程照样要扫，nil 通道走「满」分支留在原轮）。
func TestShardRoutingStableAndSpread(t *testing.T) {
	setupShards(t, 4)
	channels := currentTimerChannels()
	if len(channels) != 4 {
		t.Fatalf("✘ 通道数组长度应等于分片数: %d", len(channels))
	}

	for uid := int64(0); uid < 500; uid++ {
		first := shardOf(channels, uid)
		for k := 0; k < 3; k++ {
			if shardOf(channels, uid) != first {
				t.Fatalf("✘ 同一 uid 落片不稳定: uid=%d", uid)
			}
		}
	}

	seen := make(map[int]int)
	for uid := int64(1); uid <= 40; uid++ {
		hit := -1
		for i, ch := range channels {
			if shardOf(channels, uid) == ch {
				hit = i
			}
		}
		if hit < 0 {
			t.Fatalf("✘ uid=%d 的路由结果不在任何分片里", uid)
		}
		seen[hit]++
	}
	for i := 0; i < 4; i++ {
		if seen[i] == 0 {
			t.Fatalf("✘ 分片 %d 一个都没分到: %v", i, seen)
		}
	}

	// 负 uid 与空快照
	if shardOf(nil, 7) != nil {
		t.Error("✘ 空快照应返回 nil 通道")
	}
	if shardOf(channels, -5) != shardOf(channels, 5) {
		t.Error("✘ 负 uid 未取绝对值")
	}
}

// TestShardCapacitySumStable 各片容量之和必须等于既往单通道容量：
// 分片是「把一条队切成 N 条」，不是「把允许的积压放大 N 倍」。
func TestShardCapacitySumStable(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 3} {
		setupShards(t, n)
		var total int64
		for _, ch := range currentTimerChannels() {
			total += int64(cap(ch))
		}
		if total > MaxTimerChannelNum {
			t.Fatalf("✘ 分片 %d 时总容量被放大: %d > %d", n, total, MaxTimerChannelNum)
		}
		if total < MaxTimerChannelNum-int64(n) {
			t.Fatalf("✘ 分片 %d 时总容量掉得超过取整余量: %d", n, total)
		}
		if stats := GetQueueStats(); stats.ScheduleCap != int(total) {
			t.Fatalf("✘ 监控口径未覆盖全部分片: ScheduleCap=%d 实际=%d", stats.ScheduleCap, total)
		}
	}
}

// TestSetScheduleShardsRefusedAfterChannelsBuilt 通道已建好后改分片必须被拒绝，
// 否则新多出来的那片永久无人消费（调度项默默堆到满），少掉的那片被两个协程抢。
func TestSetScheduleShardsRefusedAfterChannelsBuilt(t *testing.T) {
	setupShards(t, 2)
	before := currentTimerChannels()
	if got := SetScheduleShards(8); got != 2 {
		t.Fatalf("✘ 通道已建好时应保持 2 片并返回 2: got=%d", got)
	}
	after := currentTimerChannels()
	if len(after) != 2 || &after[0] != &before[0] {
		t.Fatalf("✘ 被拒绝的设置仍改动了通道数组: %d 片", len(after))
	}
	if got := ScheduleShards(); got != 2 {
		t.Fatalf("✘ ScheduleShards 读数不对: %d", got)
	}
}

// TestAllShardsHaveConsumers 每一片都必须有人在消费：给每一片各放一个定时器，
// 全部要跑起来。少起一个协程就是那一片的定时器永久停摆，比单通道更糟。
func TestAllShardsHaveConsumers(t *testing.T) {
	const shards = 4
	setupShards(t, shards)

	probes := make([]*shardBeatTimer, shards)
	for i := 0; i < shards; i++ {
		probes[i] = addTimerOnShard[*shardBeatTimer](t, i, func(p *shardBeatTimer) { p.n.Store(0) })
	}

	waitUntil(t, 5*time.Second, "有分片无人消费，探针定时器一次都没跑", func() bool {
		for _, p := range probes {
			if p.n.Load() == 0 {
				return false
			}
		}
		return true
	})
}

// TestSlowTickOnlyBlocksOwnShard 是 S68 的收益本身，两个子用例互为对照。
//
// 单片（既往行为）：一个卡住的 Tick 占住唯一的消费者，同片其它定时器的调度项
// 只能在通道里排队 —— 全系统一起停。
// 分片：卡住的那一片仍然停（片内保序是代价，不是 bug），其它片照常走。
//
// 判据刻意用「被探针计数」而不是「计时」：慢 Tick 放行后计数会立刻继续涨，
// 观测窗口里有没有涨只取决于消费者是否被占住。
func TestSlowTickOnlyBlocksOwnShard(t *testing.T) {
	const observe = 400 * time.Millisecond

	// 单片：阻塞者与探针必然同片，探针在观察窗口内几乎不动
	t.Run("单片被慢回调拖住", func(t *testing.T) {
		setupShards(t, 1)
		blocked, beat := startBlockAndBeat(t, 0)
		defer close(blocked.release)
		blockNow(t, blocked)

		base := beat.n.Load()
		time.Sleep(observe)
		if delta := beat.n.Load() - base; delta > 2 {
			t.Fatalf("✘ 单片下探针本该被卡住（这条断言描述既往瓶颈），却跑了 %d 次", delta)
		}
	})

	// 分片：探针与阻塞者落在不同片，探针不受影响
	t.Run("分片后其它片照常", func(t *testing.T) {
		setupShards(t, 4)
		channels := currentTimerChannels()
		blocked, beat := startBlockAndBeat(t, 1)
		defer close(blocked.release)

		if shardOf(channels, blocked.GetUID()) == shardOf(channels, beat.GetUID()) {
			t.Fatal("✘ 探针与阻塞者落在同一片，本用例无从对照")
		}
		blockNow(t, blocked)
		base := beat.n.Load()
		time.Sleep(observe)
		delta := beat.n.Load() - base
		// 10ms 间隔、观察 400ms：满打满算约 40 次，留足调度抖动余量只要 ≥5
		if delta < 5 {
			t.Fatalf("✘ 另一片被慢回调拖住了: 探针 %v 内只跑了 %d 次", observe, delta)
		}
	})
}

// startBlockAndBeat 在 beatShard 放探针，在另一片（单片时只能是同一片）放阻塞者。
func startBlockAndBeat(t *testing.T, beatShard int) (*shardBlockTimer, *shardBeatTimer) {
	t.Helper()
	channels := currentTimerChannels()
	blockShard := 0
	if len(channels) > 1 {
		blockShard = (beatShard + 1) % len(channels)
	}
	blocked := addTimerOnShard[*shardBlockTimer](t, blockShard, func(p *shardBlockTimer) {
		p.n.Store(0)
		p.arm = make(chan struct{})
		p.entered = make(chan struct{})
		p.release = make(chan struct{})
		p.blocked.Store(false)
	})
	beat := addTimerOnShard[*shardBeatTimer](t, beatShard, func(p *shardBeatTimer) { p.n.Store(0) })
	return blocked, beat
}

// blockNow 武装阻塞者并等它真的把所在分片的消费者占住
func blockNow(t *testing.T, blocked *shardBlockTimer) {
	t.Helper()
	close(blocked.arm)
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("✘ 阻塞型定时器一直没被派发，占不住消费者，本用例的观测无意义")
	}
}

// TestScheduleChannelAtOutOfRange 越界与空快照都必须返回 nil 而不是 panic：
// 消费协程靠这条走到「等生命周期切换」分支（facade 里明文承诺的行为）。
func TestScheduleChannelAtOutOfRange(t *testing.T) {
	TimeStop(context.Background())
	resetTimerChannel()
	if scheduleChannelAt(0) != nil {
		t.Fatal("✘ 通道未建立时应当读到 nil")
	}
	setupShards(t, 3)
	if scheduleChannelAt(2) == nil {
		t.Fatal("✘ 合法下标 2 读不到通道")
	}
	if scheduleChannelAt(-1) != nil || scheduleChannelAt(3) != nil {
		t.Fatal("✘ 越界下标必须返回 nil（-1 与 len 两侧）")
	}
}

// TestPerShardDepthIsVisible 逐片深度必须能从统计里读到：只有总和时，
// 一片被慢回调占死并开始丢弃，总深度才走到 1/N，按总量配的告警永远不响。
func TestPerShardDepthIsVisible(t *testing.T) {
	TimeStop(context.Background())
	resetTimerChannel()
	if got := SetScheduleShards(3); got != 3 {
		t.Fatalf("✘ 设置分片失败: %d", got)
	}
	ensureTimerChannel()
	t.Cleanup(func() {
		resetTimerChannel()
		SetScheduleShards(DefaultScheduleShards)
	})

	// 不起消费者，直接往 1 号片压三条
	channels := currentTimerChannels()
	for i := 0; i < 3; i++ {
		channels[1] <- timerTask{}
	}

	m := NewTimerManager()
	stats := m.GetQueueStats()
	if len(stats.ScheduleShardLen) != 3 {
		t.Fatalf("✘ 逐片深度长度不等于分片数: %v", stats.ScheduleShardLen)
	}
	if stats.ScheduleShardLen[1] != 3 || stats.ScheduleShardLen[0] != 0 {
		t.Fatalf("✘ 逐片深度读不到: %v", stats.ScheduleShardLen)
	}
	if stats.ScheduleLen != 3 {
		t.Fatalf("✘ 总和口径不对: %d", stats.ScheduleLen)
	}
	if stats.ScheduleCap != 3*stats.ScheduleShardCap {
		t.Fatalf("✘ 容量口径不对: total=%d per=%d", stats.ScheduleCap, stats.ScheduleShardCap)
	}
	if stats.ScheduleShardCap*3 > int(MaxTimerChannelNum) {
		t.Fatalf("✘ 单片容量放大总盘: per=%d", stats.ScheduleShardCap)
	}
	m.Close()

	// 未 Run 的门面也要按当前分片数给足序列（序列缺失 ≠ 取值为 0），
	// 而容量与轮一样留 0：此刻压根没有通道，报「理论每片容量」会让
	// depth/capacity 算出 0%，看着像健康运行而不是停摆。
	facade := GetQueueStats()
	if len(facade.ScheduleShardLen) != 3 {
		t.Fatalf("✘ 未 Run 时逐片序列缺席: len=%d", len(facade.ScheduleShardLen))
	}
	if facade.ScheduleCap != 0 || facade.ScheduleShardCap != 0 {
		t.Fatalf("✘ 未 Run 时容量口径不一致: total=%d per=%d",
			facade.ScheduleCap, facade.ScheduleShardCap)
	}
}

// reentryTimer 自己侦测「同一定时器的 Tick 被并发执行」
type reentryTimer struct {
	Timer
	n        atomic.Int64
	inflight atomic.Int32
	races    atomic.Int64
}

func (r *reentryTimer) Tick() {
	if r.inflight.Add(1) > 1 {
		r.races.Add(1)
	}
	r.n.Add(1)
	time.Sleep(time.Millisecond) // 故意把临界区拉长，给并发制造机会
	r.inflight.Add(-1)
}

// TestShardKeepsSingleTimerSerialized 分片的立身之本：同一定时器绝不并发执行。
// 边跑边做 Pause/AddTimer 复活（uid 与代次都会动），这是最容易换片的窗口。
func TestShardKeepsSingleTimerSerialized(t *testing.T) {
	setupShards(t, 4)

	const num = 12
	timers := make([]*reentryTimer, num)
	for i := range timers {
		tm, err := NewTimer[*reentryTimer](10, InfiniteCount, func(p *reentryTimer) {
			p.n.Store(0)
			p.inflight.Store(0)
			p.races.Store(0)
		})
		if err != nil {
			t.Fatal(err)
		}
		timers[i] = tm
	}
	batch := make([]TimerInterface, num)
	for i, tm := range timers {
		batch[i] = tm
	}
	if err := AddTimer(batch...); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(600 * time.Millisecond)
	for round := 0; time.Now().Before(deadline); round++ {
		victim := timers[round%num]
		victim.Pause()
		if err := AddTimer(victim); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var ran int64
	for _, tm := range timers {
		ran += tm.n.Load()
	}
	if ran == 0 {
		t.Fatal("✘ 全程没有任何 Tick 执行，本用例失去意义")
	}
	for i, tm := range timers {
		if races := tm.races.Load(); races != 0 {
			t.Fatalf("✘ 第 %d 个定时器出现并发 Tick: 次数=%d 分片数=%d", i, races, ScheduleShards())
		}
	}
	t.Logf("✔ 4 片下 %d 个定时器共 %d 次 Tick，无一次重入", num, ran)
}

// tickingConsumers 数一数当前还停在 timeTick 里的协程。
// 比 runtime.NumGoroutine 的绝对值可靠：同包别的用例、GC、测试框架自己的协程
// 都会让总数起伏，而这里要判的是「分片消费协程有没有被收尾掉」。
func tickingConsumers(t *testing.T) int {
	t.Helper()
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "touchgocore/localtimer.timeTick(")
		}
		buf = make([]byte, len(buf)*2)
	}
}

// TestConsumerPanicDegradesShard 一片消费协程被 panic 打死时的处置（S68）：
// 只标记降级，绝不能摘掉本轮 runtime。
//
// executeTimer 内层只 recover 业务 Tick 的 panic，这里走的是它之前的 isValid。
// 摘除的代价分三步：TimeStop 与下一次 Run 的收尾路径 timerRT.Swap 读到 nil，
// 既不 shutdown 也不等 tickWG ⇒ 存活分片协程永久挂在 closech 上泄漏；而通道数组
// 被下一轮复用（ensureTimerChannel 已就绪即早退），新旧协程同抢一片，
// 「一片一个消费者」这条片内保序的全部依据就此失效。
func TestConsumerPanicDegradesShard(t *testing.T) {
	setupShards(t, 2)
	mgr := GetDefaultManager()
	if mgr == nil {
		t.Fatal("✘ 默认管理器未就绪")
	}
	if !IsSystemRunning() {
		t.Fatal("✘ Run 之后系统应处于运行状态")
	}
	// 另一片上的活定时器：用来证明一片死了不牵连别片
	probe := addTimerOnShard[*shardBeatTimer](t, 1, func(p *shardBeatTimer) { p.n.Store(0) })

	scheduleChannelAt(0) <- timerTask{timer: &bombTimer{}, gen: 1, mgr: mgr}

	waitUntil(t, 3*time.Second, "分片 panic 后 IsSystemRunning 仍谎报正常", func() bool {
		return !IsSystemRunning()
	})
	if rt := timerRT.Load(); rt == nil {
		t.Fatal("✘ 单片 panic 竟摘除了全局 runtime，存活分片将无人能收尾")
	}
	before := probe.n.Load()
	time.Sleep(200 * time.Millisecond)
	if probe.n.Load() <= before {
		t.Fatal("✘ 一片 panic 连带把其它分片也拖停了")
	}

	// 收尾必须还能正常进行：TimeStop 拿得到 rt，存活协程全部退出
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCloseBudget)
	defer cancel()
	TimeStop(ctx)
	if got := tickingConsumers(t); got != 0 {
		t.Fatalf("✘ TimeStop 后仍有 %d 个分片消费协程存活（泄漏到下一轮会同抢一片）", got)
	}

	// 重新 Run 之后每片仍是单消费者：同一定时器不出现并发 Tick
	Run(context.Background())
	timers := make([]*reentryTimer, 8)
	batch := make([]TimerInterface, len(timers))
	for i := range timers {
		tm, err := NewTimer[*reentryTimer](10, InfiniteCount, func(p *reentryTimer) {
			p.n.Store(0)
			p.inflight.Store(0)
			p.races.Store(0)
		})
		if err != nil {
			t.Fatalf("✘ NewTimer 失败: %v", err)
		}
		timers[i] = tm
		batch[i] = tm
	}
	if err := AddTimer(batch...); err != nil {
		t.Fatalf("✘ AddTimer 失败: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	for _, tm := range timers {
		tm.Pause()
	}
	for i, tm := range timers {
		if races := tm.races.Load(); races != 0 {
			t.Fatalf("✘ 降级后重新 Run 出现同片双消费者: 第 %d 个定时器重入 %d 次", i, races)
		}
	}
}

// TestShardResetAcrossLifecycle 跨 Run→TimeStop→Run 重新按当前期望值建通道：
// 上一轮的通道数组若被复用，新轮的消费者数与通道数就会错位。
func TestShardResetAcrossLifecycle(t *testing.T) {
	setupShards(t, 3)
	old := currentTimerChannels()
	TimeStop(context.Background())
	if got := currentTimerChannels(); got != nil {
		t.Fatalf("✘ TimeStop 未释放调度通道: %d 片", len(got))
	}
	SetScheduleShards(5)
	Run(context.Background())
	fresh := currentTimerChannels()
	if len(fresh) != 5 {
		t.Fatalf("✘ 新一轮未按新分片数建通道: %d", len(fresh))
	}
	for i := range fresh {
		if fresh[i] == old[i%len(old)] {
			t.Fatal("✘ 新轮复用了旧通道")
		}
	}
	// 消费者数跟上了新通道数：每片都要有人跑
	probe := addTimerOnShard[*shardBeatTimer](t, 4, func(p *shardBeatTimer) { p.n.Store(0) })
	waitUntil(t, 5*time.Second, "新分片无人消费", func() bool { return probe.n.Load() > 0 })
}
