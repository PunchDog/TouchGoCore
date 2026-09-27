package websocket

import (
	"context"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/corectx"
)

// ============================================================================
// M4 回归用例：websocket 包级状态跨 Run 代际混用修复。
//
// 修复前的三连缺陷：
//  1. closeCh / msgQueue / tickDone / stopOnce 都是包级变量，二次 Run 直接覆盖；
//  2. 旧 Tick 仍在 select 这两个名字 → 「旧 Tick + 新 Tick 同时消费新 msgQueue」，
//     新旧连接的消息跨代混投；
//  3. 旧 Tick 永远收不到旧 closeCh 的关闭信号（那个 chan 已被新值替换且无人引用），
//     协程泄漏直到进程退出。
//
// 修复后这些字段全部收进 runState 实例：每代 Run 持有独立 state，Tick 与 Client
// 通过自己捕获的指针访问，Run 在切换前 stopRunState 旧代并等其 tickDone。
//
// 本机无 gcc，go test -race 不可用，因此用「代际指针身份 + tickDone 关闭 + 队列
// 滞留观测」三组事实替代竞态检测：旧 Tick 真退出 ↔ 旧 tickDone 真关闭；
// 新代消息只被新代消费 ↔ 旧 msgQueue 永远滞留、新 msgQueue 立刻被取走。
// ============================================================================

// makeM4Cfg 装配一个最小可启动的 ws 配置：监听端口为空（不起 HTTP 服务器），
// 仅用于驱动 Run 的代际状态切换路径。
func makeM4Cfg() *config.Cfg {
	return &config.Cfg{
		Ws: &config.WebsocketConfig{},
	}
}

// waitTickDone 在预算内等待 Tick 协程退出。
func waitTickDone(t *testing.T, s *runState, d time.Duration, what string) {
	t.Helper()
	select {
	case <-s.tickDone:
	case <-time.After(d):
		t.Fatalf("✘ %s: tickDone 在 %v 内未关闭", what, d)
	}
}

// assertTickNotDone 断言 tickDone 仍未关闭（Tick 还在跑）。
func assertTickNotDone(t *testing.T, s *runState, what string) {
	t.Helper()
	select {
	case <-s.tickDone:
		t.Fatalf("✘ %s: tickDone 不应已关闭", what)
	default:
	}
}

// snapshotM4Globals 在测试前后还原会被 Run 改写的全局指针，避免污染其它用例。
func snapshotM4Globals(t *testing.T) {
	t.Helper()
	prevState := loadRunState()
	prevQueue := wsQueue.Swap(nil)
	prevRunCtx := runCtxValue.Swap(nil)
	prevClientMap := loadClientMap()
	prevPool := clientpool
	t.Cleanup(func() {
		// 先把测试期间起的 Tick 收掉，避免它在新测试里继续消费旧 state
		if s := loadRunState(); s != nil {
			stopRunState(s, context.Background())
		}
		currentRunState.Store(prevState)
		wsQueue.Store(prevQueue)
		runCtxValue.Store(prevRunCtx)
		storeClientMap(prevClientMap)
		clientpool = prevPool
	})
}

// TestStopRunStateTerminatesBoundTick 验证 stopRunState 必定让绑定该代的 Tick 退出。
//
// 修复前 closeCh 是包级变量，二次 Run 覆盖后旧 Tick 永远收不到关闭信号 → 协程泄漏。
// 现在 closeCh 是 state 字段，Tick 通过自己的 state 指针读它，stopRunState 一关必退。
func TestStopRunStateTerminatesBoundTick(t *testing.T) {
	snapshotM4Globals(t)

	s1 := newRunState(context.Background(), 4)
	currentRunState.Store(s1)
	go tickLoop(s1)

	// 给 Tick 协程一点时间进入 select；本机调度抖动较大，50ms 已足够
	time.Sleep(50 * time.Millisecond)
	assertTickNotDone(t, s1, "未触发停机时 Tick 不应自行退出")

	stopRunState(s1, context.Background())
	waitTickDone(t, s1, time.Second, "stopRunState 后旧 Tick 必须退出")

	// 幂等：再次调用必须立即返回，不得 panic（重复 close 已关闭的 chan）
	stopRunState(s1, context.Background())
}

// TestRunTwiceIsolatesGenerations 验证：二次 Run（中间不调 Stop）后
//   - 旧 Tick 已退出；
//   - 新旧 runState 是不同实例，代号递增；
//   - 新代 msgQueue 仍被新代 Tick 消费；
//   - 旧代 msgQueue 不再有消费者，消息滞留（不会跨代混投）。
func TestRunTwiceIsolatesGenerations(t *testing.T) {
	snapshotM4Globals(t)

	ctx1 := corectx.WithCfg(context.Background(), makeM4Cfg())
	if err := Run(ctx1); err != nil {
		t.Fatalf("Run #1 失败: %v", err)
	}
	state1 := loadRunState()
	if state1 == nil {
		t.Fatal("✘ Run #1 后未装上代际状态")
	}
	gen1 := state1.generation
	assertTickNotDone(t, state1, "Run #1 后新 Tick 应在运行")

	// Run #2 不先 Stop：必须自己把上一代收掉。这是修复前协程泄漏的复现路径。
	ctx2 := corectx.WithCfg(context.Background(), makeM4Cfg())
	if err := Run(ctx2); err != nil {
		t.Fatalf("Run #2 失败: %v", err)
	}
	state2 := loadRunState()
	if state2 == nil {
		t.Fatal("✘ Run #2 后未装上代际状态")
	}
	if state2 == state1 {
		t.Fatal("✘ Run #2 复用了 Run #1 的 runState 实例（包级状态未隔离）")
	}
	if state2.generation <= gen1 {
		t.Fatalf("✘ 代号未递增: gen1=%d gen2=%d", gen1, state2.generation)
	}
	if state2.msgQueue == state1.msgQueue {
		t.Fatal("✘ 新旧 msgQueue 是同一个 chan（跨代混投的根源）")
	}

	// Run 内部已 stopRunState 旧代并等过 tickDone：此处必为已关闭。
	waitTickDone(t, state1, time.Second, "Run #2 切换后旧 Tick 必须已退出")
	assertTickNotDone(t, state2, "Run #2 后新 Tick 应在运行")

	// 新代 msgQueue 必须被新代 Tick 消费（无 client 时 processMessage 会记录
	// 「客户端未找到」错误，但消息一定会被取走）。
	probe := &msgQueueType{uid: 999001, data: []byte("probe")}
	select {
	case state2.msgQueue <- probe:
	default:
		t.Fatal("✘ 新代 msgQueue 容量异常，无法投递探针")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(state2.msgQueue) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(state2.msgQueue) != 0 {
		t.Fatalf("✘ 新代 Tick 未消费新代 msgQueue: len=%d", len(state2.msgQueue))
	}

	// 旧代 msgQueue 不再有消费者：投一条进去后必须滞留。
	stale := &msgQueueType{uid: 999002, data: []byte("stale")}
	select {
	case state1.msgQueue <- stale:
	default:
		t.Fatal("✘ 旧代 msgQueue 容量异常，无法投递滞留探针")
	}
	time.Sleep(100 * time.Millisecond)
	if len(state1.msgQueue) != 1 {
		t.Fatalf("✘ 旧代 msgQueue 被消费了（不应再有活跃 Tick）: len=%d", len(state1.msgQueue))
	}
	if got := <-state1.msgQueue; got != stale {
		t.Fatal("✘ 旧代 msgQueue 中的探针被替换")
	}
}

// TestClientBindsToRunStateAtConnect 验证 Client 在 NewClient 时锁定本代 runState，
// 后续 Run 换代不会让旧连接的消息流向新一代的 msgQueue。
//
// 修复前 readLoop 直接读写包级 msgQueue：Run 换代后旧连接的消息会落进新代队列，
// 被新代 Tick 当成新连接的消息处理（即「跨代混投」）。
func TestClientBindsToRunStateAtConnect(t *testing.T) {
	snapshotM4Globals(t)

	// 装第一代。本用例不起 Tick，预关 tickDone 让 cleanup 里的 stopRunState 立即返回。
	s1 := newRunState(context.Background(), 8)
	close(s1.tickDone)
	currentRunState.Store(s1)

	// 模拟 NewClient 中的代际快照（不走完整 NewClient，避免依赖 ws 连接）
	c := &Client{UID: 4242, remoteAddr: "bare://m4"}
	c.runState = loadRunState()
	c.initChannels()

	if c.runState != s1 {
		t.Fatal("✘ Client 未绑定到当前代际")
	}
	if c.dispatchQueue() != s1.msgQueue {
		t.Fatal("✘ dispatchQueue 未返回绑定代际的 msgQueue")
	}

	// 换代：currentRunState 指向 s2，但 c.runState 仍是 s1
	s2 := newRunState(context.Background(), 8)
	close(s2.tickDone)
	currentRunState.Store(s2)

	if c.dispatchQueue() != s1.msgQueue {
		t.Fatal("✘ Run 换代后 Client.dispatchQueue 漂到了新一代（跨代混投）")
	}
	if c.dispatchQueue() == s2.msgQueue {
		t.Fatal("✘ 旧连接的消息会落进新一代 msgQueue")
	}

	// 裸构造（未走 NewClient）的 Client 才回退到当前代——这是测试旁路，业务路径不会进
	bare := &Client{UID: 1}
	if bare.dispatchQueue() != s2.msgQueue {
		t.Fatal("✘ 裸 Client 应回退到当前代际 msgQueue")
	}
}
