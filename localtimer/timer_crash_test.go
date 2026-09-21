package localtimer

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// ============================================================================
// localtimer 宕机问题修复后的回归验证。
//
// 历史问题（修复前实测可复现，详见 git 历史）：
//   1. TimeStop → cleanupWheel 持 wheelLock 同步执行 Tick，Tick 内 Remove /
//      AddTimer 会重入同一把锁 → 进程永久挂死；
//   2. 未 Run（或停止后）直接 NewTimerManager → nil timerManagerMap panic；
//   3. Range 内「摘除 + 回加」的节点被延迟删除误删 → 定时器永久丢失；
//   4. 对象池复用同一实例 → 两个逻辑定时器 UID 相同、互相串扰；
//   5. 业务协程 Remove 与调度协程 Tick/HasNext/AddTimer 的数据竞争。
//
// 致命场景（1、2）会被 runtime 直接杀死/挂死，recover 无效，因此用
// 「子进程重入自身测试二进制」运行，依据子进程是否超时/崩溃来判定。
//
// 运行方式：
//   go test ./localtimer -run 'TestRegression_' -v
//   CGO_ENABLED=1 go test ./localtimer -run TestRace_ -race -v
// ============================================================================

const crashChildEnv = "TG_LOCALTIMER_CRASH_CHILD"

// runCrashChild 在子进程中单独运行指定测试函数，返回子进程输出与是否超时（挂死）。
func runCrashChild(t *testing.T, testName string, wait time.Duration) (out string, timedOut bool) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), crashChildEnv+"=1")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(wait):
		_ = cmd.Process.Kill()
		<-done
		timedOut = true
	}
	return buf.String(), timedOut
}

func isChild() bool { return os.Getenv(crashChildEnv) == "1" }

// ============================================================================
// 场景 1（回归）：退出流程不得死锁。
//
//	cleanupWheel 必须在释放 wheelLock 之后才执行业务 Tick，
//	否则 Tick 内调用 Remove / AddTimer 会重入同一把锁，TimeStop 永久挂起。
// ============================================================================

type selfRemoveTimer struct {
	Timer
	ticked atomic.Int64
}

func (s *selfRemoveTimer) Tick() {
	s.ticked.Add(1)
	// 业务最常见的写法：跑完把自己摘掉。同时覆盖「回调在飞时收到作废请求」
	// 这条路径 —— 归还必须延迟到 endTick，不能把正在执行回调的对象交给新主人。
	s.Remove()
}

func scenarioCloseNoDeadlock() {
	// 看门狗：超时后 dump 全部 goroutine 栈并退出，用于定位死锁发生的确切位置
	go func() {
		time.Sleep(5 * time.Second)
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		fmt.Fprintf(os.Stderr, "\n===== DEADLOCK DETECTED: goroutine dump =====\n%s\n", buf[:n])
		os.Exit(3)
	}()

	Run(context.Background())

	m := GetDefaultManager()
	tm, err := NewTimer[*selfRemoveTimer](10, 1, nil)
	if err != nil {
		return
	}

	// 放入小时轮（1 小时才 tick 一次），保证 Close 前不会被正常调度取走；
	// 同时把 nextTime 设为已过期，保证 cleanupWheel 一定会执行它的 Tick。
	p := tm.GetParent()
	p.nextTime.Store(util.CurrentMS() - 1)
	wheel := m.wheels[TimerTypeHour]
	p.wheel.Store(wheel)
	wheel.tickWheel.Add(tm)

	time.Sleep(50 * time.Millisecond)
	TimeStop(context.Background())
}

func TestRegression_CloseNoDeadlock_TickRemovesSelf(t *testing.T) {
	if isChild() {
		scenarioCloseNoDeadlock()
		return
	}

	out, timedOut := runCrashChild(t, "TestRegression_CloseNoDeadlock_TickRemovesSelf", 20*time.Second)
	if strings.Contains(out, "DEADLOCK DETECTED") {
		t.Fatalf("✘ 退出流程仍然存在死锁:\n%s", extractDeadlockStack(out))
	}
	if strings.Contains(out, "all goroutines are asleep - deadlock!") {
		t.Fatalf("✘ 退出流程死锁，进程被 runtime 杀死:\n%s", tailLines(out, 12))
	}
	if timedOut {
		t.Fatalf("✘ 退出流程未在 20s 内结束（疑似挂死），输出:\n%s", tailLines(out, 12))
	}
	t.Log("✔ TimeStop 正常返回，退出无死锁")
}

// ============================================================================
// 场景 2（回归）：未 Run / 停止后重建管理器不得 panic。
// ============================================================================

func scenarioNewManagerNoPanic() {
	// 故意不调用 Run，模拟「自建管理器 / 停止后重建管理器」的用法
	mgr := NewTimerManager()
	mgr.Close()
}

func TestRegression_NewTimerManager_NoPanic(t *testing.T) {
	if isChild() {
		scenarioNewManagerNoPanic()
		return
	}

	out, timedOut := runCrashChild(t, "TestRegression_NewTimerManager_NoPanic", 20*time.Second)
	if strings.Contains(out, "nil pointer dereference") || strings.Contains(out, "panic:") {
		t.Fatalf("✘ 仍然 panic:\n%s", tailLines(out, 20))
	}
	if timedOut {
		t.Fatalf("✘ 子进程挂死，输出:\n%s", tailLines(out, 12))
	}
	t.Log("✔ 未 Run 时创建管理器不再 panic")
}

// ============================================================================
// 场景 3（回归）：Range 内「摘除 + 回加」的节点必须保留。
//
//	processWheelTick 迁移失败时会在 Range 回调里把节点回加到当前时间轮，
//	list 必须撤销该节点的待删除标记，否则定时器会被静默丢弃。
// ============================================================================

func TestRegression_RangeRemoveThenAddKeepsTimer(t *testing.T) {
	l := list.NewList()
	n := &Timer{}
	if !l.Add(n) {
		t.Fatal("Add 失败")
	}

	l.Range(func(node list.INode) bool {
		node.GetNode().Remove() // processWheelTick：先摘
		l.Add(node)             // 迁移目标通道满，回加当前轮
		return true
	})

	if got := l.Length(); got != 1 {
		t.Fatalf("✘ 回加的节点被延迟删除误删，链表长度=%d（期望 1）", got)
	}
	t.Log("✔ Range 内回加的节点被正确保留")
}

// ============================================================================
// 场景 4（回归）：对象池允许复用实例，但每个逻辑定时器必须拿到独立 UID。
// ============================================================================

type plainTimer struct {
	Timer
	n atomic.Int64
}

func (p *plainTimer) Tick() { p.n.Add(1) }

func TestRegression_PoolReuseKeepsUniqueUID(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	a, err := NewTimer[*plainTimer](1000, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(a); err != nil {
		t.Fatal(err)
	}
	uidA := a.GetUID()

	a.Remove() // 彻底作废并归还对象池，b 很可能正好认领到这个实例

	b, err := NewTimer[*plainTimer](1000, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(b); err != nil {
		t.Fatal(err)
	}
	uidB := b.GetUID()

	if a == b {
		t.Logf("实例被对象池复用（正常），需保证身份隔离: a=b=%p", a)
	}
	if uidA == 0 || uidB == 0 || uidA == uidB {
		t.Fatalf("✘ UID 未隔离: uidA=%d uidB=%d", uidA, uidB)
	}
	t.Logf("✔ 对象池复用实例但 UID 独立: uidA=%d uidB=%d", uidA, uidB)
}

// ============================================================================
// 场景 5（回归）：已移除的定时器不得再被调度（幽灵定时器）。
//
//	代次机制必须让残留在通道中的在途调度项失效。
// ============================================================================

func TestRegression_RemovedTimerNotRescheduled(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](5, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	before := tm.n.Load()
	if before == 0 {
		t.Fatal("定时器未被执行，调度链路异常")
	}

	tm.Remove()
	time.Sleep(300 * time.Millisecond)

	if after := tm.n.Load(); after != before {
		t.Fatalf("✘ 已移除的定时器仍在执行: 移除前=%d 移除后=%d", before, after)
	}
	t.Logf("✔ 移除后不再执行（移除前执行 %d 次）", before)
}

// ============================================================================
// 场景 6（回归）：周期性定时器必须持续触发，且恢复后能重新调度。
// ============================================================================

func TestRegression_PeriodicTimerKeepsTicking(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](10, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	if n := tm.n.Load(); n < 3 {
		t.Fatalf("✘ 周期定时器执行次数过少: %d（期望 >=3）", n)
	} else {
		t.Logf("✔ 周期定时器正常执行 %d 次", n)
	}
}

// ============================================================================
// 场景 7（回归）：TimeStop 后可再次 Run，定时器恢复工作。
// ============================================================================

func TestRegression_RestartAfterTimeStop(t *testing.T) {
	Run(context.Background())
	TimeStop(context.Background())

	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*plainTimer](5, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := tm.n.Load(); n == 0 {
		t.Fatal("✘ 二次 Run 后定时器不再执行")
	}
	t.Log("✔ 停止后重启，定时器恢复正常")
}

// ============================================================================
// 场景 8（数据竞争，需 go test -race）：
//
//	telegram/rpc 等调用方会在「另一个 goroutine」里调用 timer.Pause()/Remove()，
//	而调度协程同时在 Tick → HasNext → AddTimer 上读写同一 Timer。
//	Timer 的状态字段已全部原子化，本用例在 -race 下不应报竞争。
//	这里用 Pause 而非 Remove：后者会把实例交还对象池，此后的 AddTimer 一律被拒，
//	覆盖不到「业务停表」与「调度续期」并发的真实窗口。
// ============================================================================

type raceTimer struct {
	Timer
	n atomic.Int64
}

func (r *raceTimer) Tick() { r.n.Add(1) }

func TestRace_RemoveFromOtherGoroutine(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer[*raceTimer](1, -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tm.Pause()       // 业务侧并发停表
				time.Sleep(0)    // 放大竞态窗口
				_ = tm.HasNext() // 与调度协程争抢 count
				if err := AddTimer(tm); err == nil {
					_ = tm.GetParent().GetRemainingCount()
				}
			}
		}()
	}
	wg.Wait()
	tm.Remove() // 全部协程退出后才正式作废归还，避免对已归池实例做悬垂操作
}

// extractDeadlockStack 抽取 dump 中「卡在锁上」的关键栈帧，避免日志刷屏
func extractDeadlockStack(out string) string {
	var keep []string
	hit := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "DEADLOCK DETECTED") {
			hit = true
			continue
		}
		if !hit {
			continue
		}
		if strings.Contains(line, "goroutine ") ||
			strings.Contains(line, "localtimer.") ||
			strings.Contains(line, "sync.") ||
			strings.Contains(line, "list.") {
			keep = append(keep, strings.TrimSpace(line))
		}
	}
	if len(keep) == 0 {
		return tailLines(out, 12)
	}
	return strings.Join(keep, "\n")
}

// tailLines 取输出末尾若干行，避免日志刷屏
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// 场景 9（背压）：调度通道打满时不得 fork 协程，且定时器「留轮重试」而非永久丢弃。
//
//	修复前：通道满 → 每个到期定时器 go executeTimer → 与续期 AddTimer 递归 fork，
//	毫秒轮 1ms 一次即可 OOM。修复后：发送失败就留在当前轮，下个 tick 重试，零新增协程。
//
//	用 NewTimerManager 独立驱动（不 Run、无消费协程），全局通道天然保持满，
//	隔离出纯背压路径。
// ============================================================================

func TestRegression_BackpressureNoGoroutineExplosion(t *testing.T) {
	m := NewTimerManager()
	defer m.Close()

	wheel := m.wheels[TimerTypeHour] // 小时轮 1h 才 tick 一次，不会自我干扰
	ch := currentTimerChannel()

	// 灌满全局调度通道，制造持续背压
fill:
	for i := int64(0); i < MaxTimerChannelNum; i++ {
		select {
		case ch <- timerTask{}:
		default:
			break fill
		}
	}

	base := runtime.NumGoroutine()

	plant := func() {
		tm, err := NewTimer[*plainTimer](1000, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := tm.GetParent()
		p.nextTime.Store(util.CurrentMS() - 1) // 已过期，必然走派发分支
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
	}

	for i := 0; i < 200; i++ {
		plant()
		m.processWheelTick(wheel, TimerTypeHour)
	}

	if g := runtime.NumGoroutine(); g-base > 5 {
		t.Fatalf("✘ 背压期间协程数暴涨: base=%d now=%d", base, g)
	}
	if d := m.GetStats().TimersDropped; d < 200 {
		t.Fatalf("✘ 丢弃计数不足: %d（期望 >=200）", d)
	}
	if l := wheel.tickWheel.Length(); l != 200 {
		t.Fatalf("✘ 定时器应全部留在轮里等待重试: 链表长度=%d（期望 200）", l)
	}
	if c := wheel.timerCount.Load(); c != 200 {
		t.Fatalf("✘ 计数与链表不一致: count=%d len=%d", c, wheel.tickWheel.Length())
	}

	// 腾出一个空位，验证「留轮重试」而非永久丢弃
	<-ch
	m.processWheelTick(wheel, TimerTypeHour)
	if l := wheel.tickWheel.Length(); l != 199 {
		t.Fatalf("✘ 通道腾空后应有一个定时器恢复派发: 链表长度=%d（期望 199）", l)
	}

	// 清理：排空灌入的占位任务，避免污染后续测试的全局通道
drain:
	for {
		select {
		case <-ch:
		default:
			break drain
		}
	}
	t.Logf("✔ 背压期间零协程增长，累计丢弃 %d 次，通道腾空后恢复派发", m.GetStats().TimersDropped)
}

// ============================================================================
// 场景 10（并发）：N 个协程并发 TimeStop 不得触发 "close of closed channel"。
//
//	修复前：TimeStop 对裸全局 closech 先查后关，非原子，两个并发调用会双重 close。
//	修复后：timerRT.Swap(nil) 只有一方拿到 runtime，closeOnce 再兜一层幂等。
// ============================================================================

func scenarioConcurrentTimeStop() {
	Run(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			TimeStop(context.Background())
		}()
	}
	wg.Wait()
}

func TestRegression_ConcurrentTimeStopNoPanic(t *testing.T) {
	if isChild() {
		scenarioConcurrentTimeStop()
		return
	}
	out, timedOut := runCrashChild(t, "TestRegression_ConcurrentTimeStopNoPanic", 25*time.Second)
	if strings.Contains(out, "close of closed channel") {
		t.Fatalf("✘ 并发 TimeStop 触发双重 close:\n%s", tailLines(out, 20))
	}
	if strings.Contains(out, "panic:") {
		t.Fatalf("✘ 并发 TimeStop panic:\n%s", tailLines(out, 20))
	}
	if timedOut {
		t.Fatalf("✘ 并发 TimeStop 挂死:\n%s", tailLines(out, 12))
	}
	t.Log("✔ 32 个协程并发 TimeStop 无 panic、无双重 close")
}

// ============================================================================
// 场景 11（生命周期）：连续多次 Run 不得泄漏管理器 / 时间轮协程。
//
//	修复前：Run 只 Clear 管理器 map 不 Close，每多一次 Run 就泄漏 5 个时间轮协程
//	+ 5 个 ticker。修复后：Run 先 Close 上一轮全部管理器并等待旧 TimeTick 退出。
// ============================================================================

func TestRegression_RepeatedRunNoManagerLeak(t *testing.T) {
	Run(context.Background())
	TimeStop(context.Background())
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 3; i++ {
		Run(context.Background())
	}

	// 确定性断言：任意时刻管理器 map 里最多一个存活管理器
	if l := timerManagerMap.Length(); l != 1 {
		t.Fatalf("✘ 重复 Run 后管理器泄漏: map 长度=%d（期望 1）", l)
	}
	if g := runtime.NumGoroutine(); g-base > 12 {
		t.Logf("⚠ 协程数增长偏多: base=%d now=%d（软提示）", base, g)
	}

	TimeStop(context.Background())
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	if g := runtime.NumGoroutine(); g > base+3 {
		t.Fatalf("✘ TimeStop 后协程未回落: base=%d now=%d", base, g)
	}
	t.Logf("✔ 连续 3 次 Run 无管理器泄漏，TimeStop 后协程回落至 %d", runtime.NumGoroutine())
}

// ============================================================================
// 场景 12（跨管理器）：非默认管理器的定时器触发后续期必须回到原管理器。
//
//	修复前：调度通道全局共享（各管理器共用同一组分片），消费端恒用默认管理器执行并续期，
//	非默认管理器的定时器首次触发后即被改嫁。修复后：timerTask 携带 mgr 归属。
// ============================================================================

func TestManagerIsolation_NoHijack(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	custom := NewTimerManager()
	defer custom.Close()

	// 注意：NewTimer 返回的是对象池里的实例，不是外部构造的那个，
	// 所以必须用返回的 tm 本身，而非外部新建的 &plainTimer{}。
	tm, err := NewTimer[*plainTimer](5, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := custom.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	pt := tm

	time.Sleep(300 * time.Millisecond)

	if n := pt.n.Load(); n == 0 {
		t.Fatal("✘ 非默认管理器的定时器未被执行")
	}
	customStats := custom.GetStats()
	defaultStats := GetDefaultManager().GetStats()
	if customStats.TimersAdded < 2 {
		t.Fatalf("✘ 非默认管理器续期计数异常: TimersAdded=%d（期望 >=2）", customStats.TimersAdded)
	}
	if defaultStats.TimersAdded != 0 {
		t.Fatalf("✘ 定时器被改嫁到默认管理器: 默认 TimersAdded=%d（期望 0）", defaultStats.TimersAdded)
	}
	t.Logf("✔ 非默认管理器定时器持续在原管理器续期: custom.Added=%d default.Added=%d",
		customStats.TimersAdded, defaultStats.TimersAdded)
}

// ============================================================================
// 场景 13（计数一致性）：大量派发 / 迁移 / 移除后，timerCount 与链表长度必须一致且归零。
//
//	修复前：AddTimer 入队时 +1、迁移到达不 +1、触发/迁移各 -1 → 每次迁移净 -1，长期变负；
//	Remove 摘链不 -1 → 泄漏。修复后：计数只在「节点真正进出链表」处增减，严格配对。
// ============================================================================

func TestTimerCount_MatchesListAfterChurn(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	m := GetDefaultManager()
	var timers []TimerInterface
	for i := 0; i < 40; i++ {
		// 间隔跨越时间轮边界，强制触发派发 + 迁移
		tm, err := NewTimer[*plainTimer](int64(50+i*100), InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}

	// 让派发 / 迁移充分发生，期间采样断言计数不为负
	for i := 0; i < 5; i++ {
		time.Sleep(100 * time.Millisecond)
		if c := m.GetTimerCount(); c < 0 {
			t.Fatalf("✘ 计数为负: %d", c)
		}
	}

	for _, tm := range timers {
		tm.Remove()
	}

	// 轮询等待全部摘除。失活残留由 processWheelTick 的清理分支负责摘链，
	// 这里绝不补第二轮 Remove：实例一旦归还对象池就随时可能被别人认领，
	// 陈旧主人的任何再操作都是在撕别人的调度状态。
	deadline := time.Now().Add(2 * time.Second)
	for {
		var sumCount, sumLen int64
		for _, w := range m.wheels {
			sumCount += w.timerCount.Load()
			sumLen += int64(w.tickWheel.Length())
		}
		if sumCount == 0 && sumLen == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("✘ 全部移除后计数/链表未归零: count=%d len=%d", sumCount, sumLen)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("✔ 大量派发/迁移/移除后计数与链表一致且归零")
}

// ============================================================================
// 基准：processWheelTick 的 Range 快照分配。
//
//	list.Range 改为 sync.Pool 复用快照 buffer 后，每 tick 不应再为链表长度
//	线性分配短命切片。用 -benchmem 对比 allocs/op。
// ============================================================================

func BenchmarkProcessWheelTick(b *testing.B) {
	m := NewTimerManager()
	defer m.Close()

	wheel := m.wheels[TimerTypeHour]
	for i := 0; i < 1000; i++ {
		tm, err := NewTimer[*plainTimer](3600*1000, InfiniteCount, nil)
		if err != nil {
			b.Fatal(err)
		}
		p := tm.GetParent()
		// 剩余时间需 >= 1h，calculateType 才判为小时轮，既不派发也不迁移，
		// 这样基准只测量 Range 快照分配本身。
		p.nextTime.Store(util.CurrentMS() + 3600*1000 + 60*1000)
		m.handleTimerAdd(wheel, timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.processWheelTick(wheel, TimerTypeHour)
	}
}
