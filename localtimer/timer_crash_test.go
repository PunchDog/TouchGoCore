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
	s.Remove() // 业务最常见的写法：跑完把自己摘掉
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
	tm, err := NewTimer(10, 1, &selfRemoveTimer{})
	if err != nil {
		return
	}

	// 放入小时轮（1 小时才 tick 一次），保证 Close 前不会被正常调度取走；
	// 同时把 nextTime 设为已过期，保证 cleanupWheel 一定会执行它的 Tick。
	p := tm.GetParent()
	p.nextTime.Store(util.CurrentMS() - 1)
	wheel := m.wheels[TimerTypeHour]
	p.wheel.Store(wheel)
	wheel.tickWheel.Add(tm.(list.INode))

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

	a, err := NewTimer(1000, -1, &plainTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(a); err != nil {
		t.Fatal(err)
	}
	uidA := a.GetUID()

	a.Remove() // 归还对象池

	b, err := NewTimer(1000, -1, &plainTimer{})
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

	tm, err := NewTimer(5, -1, &plainTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)
	before := tm.(*plainTimer).n.Load()
	if before == 0 {
		t.Fatal("定时器未被执行，调度链路异常")
	}

	tm.Remove()
	time.Sleep(300 * time.Millisecond)

	if after := tm.(*plainTimer).n.Load(); after != before {
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

	tm, err := NewTimer(10, InfiniteCount, &plainTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)
	if n := tm.(*plainTimer).n.Load(); n < 3 {
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

	tm, err := NewTimer(5, -1, &plainTimer{})
	if err != nil {
		t.Fatal(err)
	}
	if err := AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	if n := tm.(*plainTimer).n.Load(); n == 0 {
		t.Fatal("✘ 二次 Run 后定时器不再执行")
	}
	t.Log("✔ 停止后重启，定时器恢复正常")
}

// ============================================================================
// 场景 8（数据竞争，需 go test -race）：
//
//	telegram/rpc 等调用方会在「另一个 goroutine」里调用 timer.Remove()，
//	而调度协程同时在 Tick → HasNext → AddTimer 上读写同一 Timer。
//	Timer 的状态字段已全部原子化，本用例在 -race 下不应报竞争。
// ============================================================================

type raceTimer struct {
	Timer
	n atomic.Int64
}

func (r *raceTimer) Tick() { r.n.Add(1) }

func TestRace_RemoveFromOtherGoroutine(t *testing.T) {
	Run(context.Background())
	defer TimeStop(context.Background())

	tm, err := NewTimer(1, -1, &raceTimer{})
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
				tm.Remove()      // 业务侧并发移除
				time.Sleep(0)    // 放大竞态窗口
				_ = tm.HasNext() // 与调度协程争抢 count
				if err := AddTimer(tm); err == nil {
					_ = tm.GetParent().GetRemainingCount()
				}
			}
		}()
	}
	wg.Wait()
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
