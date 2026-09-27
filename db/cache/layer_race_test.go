package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ==================== H2：Layer.Start 与 Register 竞态 ====================
//
// 事故形状：旧实现里 Start 先在锁外做 started.CAS，再进 mu 赋 l.ctx；
// Register 在 mu 内读 started 与 ctx。于是存在一个窗口——Register 读到
// started=true 而 l.ctx 仍为 nil，判定「不用我起协程」直接返回；它没变成
// 线上事故只因为 Start 随后取的句柄快照恰好把这个 Cache 补上了，一个纯靠
// 时序巧合成立的不变量。任何一侧改动（比如 Start 改成先取快照再置 started）
// 就会退化成「启动后注册的 Cache 永远没有 flush 定时器与账本恢复」：
// 写只停在 Redis + 进程内缓冲，非优雅退出即丢，且账本永远重放不掉。
//
// 另一处更硬的 bug：wg.Add 曾在 mu 之外，与 Stop 的 wg.Wait 并发——
// 计数已归零且存在等待者时 Add 会 panic「sync: WaitGroup misuse」。
//
// 修复：started / ctx / wg.Add / closed 全部纳入同一把 mu。
// 本文件用 Layer.startGate 注入交错 + 「run 协程必然存在」的可观测后果断言。

// layerRecoverySeed 预置一条「上次进程被杀时没落库」的脏账本 + Redis envelope。
// 返回丢弃掉的 Cache（模拟崩溃：缓冲随进程消失，账本与 Redis 值留下）。
// run 协程存在的证据就是它：Cache.Run 开头会扫账重放，Recovered 变 1。
func layerRecoverySeed(t *testing.T, kv *fakeKV, clk *fakeClock, name, key string, n int) {
	t.Helper()
	c0, err := New[string, testVal](name, kv,
		WithConfig[string, testVal](testConfig()),
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](newRecSaver()),
	)
	if err != nil {
		t.Fatalf("预置 Cache: %v", err)
	}
	if err := c0.Write(context.Background(), key, &testVal{N: n}); err != nil {
		t.Fatalf("预置脏数据: %v", err)
	}
	if jm := kv.journalOf(c0.jz); len(jm) != 1 {
		t.Fatalf("预置失败：账本应有 1 条: %v", jm)
	}
}

// assertRunGoroutine 断言该 Cache 真的拿到了 run 协程，用两个互相独立的后果：
//  1. 账本恢复：Run 开头的 recoverJournal 把预置的脏账重放落库（Recovered=1）；
//  2. flush 定时器：不碰 Stop/Flush，光靠 ticker 就能把新写落库。
//
// 只断言其一会漏：Stop 的 FlushAll 也能落库，于是「没有定时器」会被掩盖。
func assertRunGoroutine(t *testing.T, c *Cache[string, testVal], sn *recSaver, key string, n int) {
	t.Helper()
	waitFor(t, 3*time.Second, func() bool {
		return c.Stats().Snapshot().Recovered == 1
	}, "该 Cache 没有 run 协程：启动账本恢复从未执行（Register/Start 竞态把它漏掉了）")

	// 定时器证据：写一个不同的键（避免与恢复路径的 buf.has 短路混淆），
	// 之后绝不调用 Stop/Flush，只等 ticker 自己把它落库。
	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Write(wctx, key, &testVal{N: n}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return sn.get(key) != nil
	}, "该 Cache 没有 flush 定时器：脏条目只能靠 Stop 兜底，非优雅退出即丢")
}

// TestLayer_RegisterBeforeStartCritical Register 在 Start 进入临界区之前完成：
// Register 看到 started=false 不起协程，必须由 Start 的句柄快照补上。
func TestLayer_RegisterBeforeStartCritical(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	layerRecoverySeed(t, kv, clk, "late", "seed", 7)

	l := NewLayer(kv, WithLayerConfig(testConfig()))
	gateEnter := make(chan struct{})
	gateRelease := make(chan struct{})
	l.startGate = func() {
		close(gateEnter)
		<-gateRelease // 挂在 mu.Lock 之前：此时 started 仍为 false
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- l.Start(ctx) }()
	waitGate(t, gateEnter, "Start 抵达临界区门口")

	sn := newRecSaver()
	c, err := Register[string, testVal](l, "late",
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](sn),
		WithWriteBehind[string, testVal](20*time.Millisecond, 10),
	)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if l.started.Load() {
		t.Fatal("前置不成立：Start 还挂在门口，started 不该已置位")
	}
	close(gateRelease)
	if err := <-startDone; err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertRunGoroutine(t, c, sn, "tick", 42)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := l.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := sn.get("seed"); got == nil || got.N != 7 {
		t.Fatalf("账本恢复应把预置脏数据落库: %+v", got)
	}
}

// TestLayer_RegisterAfterStartGetsRunGoroutine Register 在 Start 之后到达：
// 必须在锁内同时看到 started=true 与 ctx!=nil，于是自己起协程。
// 旧实现若正好读到「started=true 而 ctx 仍为 nil」就会返回一个死 Cache。
// （layer_test.go 的 TestLayer_RegisterAfterStart 只断言 Stop 能兜底 flush，
// 那条路径即使没有 run 协程也会过——本用例断言的是协程本身存在。）
func TestLayer_RegisterAfterStartGetsRunGoroutine(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	layerRecoverySeed(t, kv, clk, "late2", "seed", 11)

	l := NewLayer(kv, WithLayerConfig(testConfig()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	sn := newRecSaver()
	c, err := Register[string, testVal](l, "late2",
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](sn),
		WithWriteBehind[string, testVal](20*time.Millisecond, 10),
	)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	assertRunGoroutine(t, c, sn, "tick", 43)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := l.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := sn.get("seed"); got == nil || got.N != 11 {
		t.Fatalf("启动后注册的 Cache 也必须完成账本恢复: %+v", got)
	}
}

// TestLayer_StartRegisterConcurrentJitter 真并发：Start 挂在临界区门口，
// 「放行 Start」与「Register」两个动作各自在独立协程里带递变抖动，
// 于是两者的相对时序每轮都不一样（Register 先 / 撞上临界区 / Start 先都采得到）。
// 不变量：无论怎么交错，Register 返回的 Cache 一定拿得到 run 协程。
func TestLayer_StartRegisterConcurrentJitter(t *testing.T) {
	for i := range 12 {
		name := fmt.Sprintf("jit%d", i)
		clk := newFakeClock()
		kv := newFakeKV(clk)
		layerRecoverySeed(t, kv, clk, name, "seed", 100+i)

		l := NewLayer(kv, WithLayerConfig(testConfig()))
		gateEnter := make(chan struct{})
		gateRelease := make(chan struct{})
		l.startGate = func() {
			close(gateEnter)
			<-gateRelease
		}

		ctx, cancel := context.WithCancel(context.Background())
		startDone := make(chan error, 1)
		go func() { startDone <- l.Start(ctx) }()
		waitGate(t, gateEnter, "Start 抵达临界区门口")

		type regOut struct {
			c  *Cache[string, testVal]
			sn *recSaver
		}
		regDone := make(chan regOut, 1)
		go func() {
			// 抖动放在 Register 之前：让它与下面那个「放行 Start」的协程互相赛跑
			time.Sleep(time.Duration(i%5) * 120 * time.Microsecond)
			sn := newRecSaver()
			c, err := Register[string, testVal](l, name,
				WithClock[string, testVal](clk.Now),
				WithSaver[string, testVal](sn),
				WithWriteBehind[string, testVal](10*time.Millisecond, 10),
			)
			if err != nil {
				t.Errorf("Register: %v", err)
				regDone <- regOut{}
				return
			}
			regDone <- regOut{c: c, sn: sn}
		}()
		go func() {
			time.Sleep(time.Duration((i*3)%5) * 120 * time.Microsecond)
			close(gateRelease)
		}()

		if err := <-startDone; err != nil {
			t.Fatalf("Start: %v", err)
		}
		got := <-regDone
		cancel()
		if got.c == nil {
			t.Fatalf("第 %d 轮 Register 未返回 Cache", i)
		}
		waitFor(t, 3*time.Second, func() bool {
			return got.c.Stats().Snapshot().Recovered == 1
		}, fmt.Sprintf("第 %d 轮：Register 返回的 Cache 没有 run 协程（账本恢复从未执行）", i))
	}
}

// TestLayer_ConcurrentLifecycleNoWaitGroupMisuse 生命周期全并发压力：
// Start / Register / Stop 同时打。旧实现里 wg.Add 在 mu 之外，与 Stop 的
// wg.Wait 并发会 panic「sync: WaitGroup misuse: Add called concurrently with Wait」
// ——协程里的 panic 会直接掀掉测试进程，所以「跑完不炸」本身就是断言。
func TestLayer_ConcurrentLifecycleNoWaitGroupMisuse(t *testing.T) {
	for round := range 15 {
		clk := newFakeClock()
		kv := newFakeKV(clk)
		l := NewLayer(kv, WithLayerConfig(testConfig()))
		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() { defer wg.Done(); _ = l.Start(ctx) }()
		}
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, _ = Register[string, testVal](l, fmt.Sprintf("r%d_c%d", round, i),
					WithClock[string, testVal](clk.Now),
					WithSaver[string, testVal](newRecSaver()),
				)
			}(i)
		}
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sctx, sc := context.WithTimeout(context.Background(), 3*time.Second)
				defer sc()
				_ = l.Stop(sctx)
			}()
		}
		wg.Wait()
		cancel()

		// Stop 之后再 Start 必须是幂等空转，不得复活已关闭的 Layer
		if err := l.Start(context.Background()); err != nil {
			t.Fatalf("Stop 后 Start 应幂等返回 nil: %v", err)
		}
	}
}

// TestLayer_StopRejectsLateRegister Stop 之后到达的 Register 必须被拒：
// 返回一个永远不会被 flush 的死 Cache 比直接报错危险得多。
func TestLayer_StopRejectsLateRegister(t *testing.T) {
	clk := newFakeClock()
	kv := newFakeKV(clk)
	l := NewLayer(kv, WithLayerConfig(testConfig()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Register[string, testVal](l, "before",
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](newRecSaver()),
	); err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := l.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}

	before := len(l.handles)
	c, err := Register[string, testVal](l, "after",
		WithClock[string, testVal](clk.Now),
		WithSaver[string, testVal](newRecSaver()),
	)
	if err == nil {
		t.Fatal("Layer 关闭后 Register 必须报错，不能返回死 Cache")
	}
	if c != nil {
		t.Fatal("报错时不得返回 Cache 实例")
	}
	l.mu.RLock()
	after := len(l.handles)
	l.mu.RUnlock()
	if after != before {
		t.Fatalf("被拒的 Register 不得留下句柄: before=%d after=%d", before, after)
	}
}
