package localtimer

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/list"
)

// ============================================================================
// Tick 续期回链与引用计数非负性的定向验证。
//
// 验证点 1（计数）：wheel.timerCount 与 Timer.count 在派发/续期/移除/并发压测
//   全程不为负；有限次数耗尽后归零而非 -1。
// 验证点 2（回链）：-1（InfiniteCount）永久定时器与 count>0 的定时器，Tick 后
//   必须恰好重入队 1 条调度项（gen 最新、mgr 归属不变），经 handleTimerAdd
//   重新入链后计数与链表恢复一致；次数耗尽则不再入队。
//
// 调度拓扑提醒：AddTimer/续期写入的是 wheel.addTimerChan（轮私有通道），
// processWheelTick 到期派发才写入全局 timerChannel。测试 1/2 直驱 executeTimer
// 并消费轮通道，排除时间片竞态；测试 3 走完整 Run/TimeTick/时间轮链路。
//
// 运行方式：
//   go test ./localtimer -run 'TestTickReschedule' -v
//   go test -race -count=2 ./localtimer/
// ============================================================================

// newRawWheelManager 构造一个不启动轮协程的管理器：wheels[i] 为裸 TimerWheel，
// addTimerChan 无人消费，入链全部由测试手动喂给 handleTimerAdd，保证确定性。
func newRawWheelManager() *TimerManager {
	ensureTimerChannel()
	m := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, 5),
	}
	for i, config := range []int64{1, 1000, 60000, 600000, 3600000} {
		m.wheels[i] = &TimerWheel{
			wheelConfig:  config,
			tickWheel:    newTestRing(config), // 与生产同规格的桶环（S67）
			addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
		}
	}
	return m
}

// wheelAddTimer 复刻 runWheel 消费轮通道的动作：取出本定时器的一条入队任务
// 并交给 handleTimerAdd 入链。expect=false 时断言通道中没有本定时器的任务。
// 续期入队发生在 executeTimer 返回之前（同步路径），因此只做非阻塞检查。
func wheelAddTimer(t *testing.T, m *TimerManager, wi TimerType, tm TimerInterface, expect bool) {
	t.Helper()
	ch := m.wheels[wi].addTimerChan
	if !expect {
		select {
		case task := <-ch:
			if task.timer == tm {
				t.Fatalf("✘ 不该续期的定时器被重新入队: gen=%d", task.gen)
			}
			m.handleTimerAdd(m.wheels[wi], task)
		default:
		}
		return
	}
	for {
		select {
		case task := <-ch:
			if task.timer != tm {
				m.handleTimerAdd(m.wheels[wi], task)
				continue
			}
			m.handleTimerAdd(m.wheels[wi], task)
			return
		default:
			// 通道已被清空但仍没等到：续期彻底丢失
			t.Fatal("✘ Tick 后未产生续期入队任务（定时器未回到 manager 队列）")
			return
		}
	}
}

// assertWheelRequeueOnce 校验轮通道里恰好只有这一条本定时器的入队任务。
func assertWheelRequeueOnce(t *testing.T, m *TimerManager, wi TimerType, tm TimerInterface) {
	t.Helper()
	wheelAddTimer(t, m, wi, tm, false)
}

// dispatchAndUnlink 走生产派发入口（commitDispatch）：认领归属 → 投递全局调度
// 通道 → 摘链扣计数，再把这条任务从通道里取出来交给 executeTimer，等价于
// TimeTick 消费端拿走它。
//
// 早期版本是手写复刻那段临界区（摘链 + Store(nil) + 计数 -1），与改造后的协议
// 已经漂移：真实路径写的是 migratingMarker 而不是 nil，且摘链前要先过 CAS。
// 复刻版测得再绿也证明不了生产代码，所以改成直接调用被测函数。
// 代次在投递前捕获，与 executeTimer 续期时比对的正是这条调度项的代次。
func dispatchAndUnlink(t *testing.T, m *TimerManager, wi TimerType, timer TimerInterface, parent *Timer) timerTask {
	t.Helper()
	ch := currentTimerChannel()
	task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
	node, ok := timer.(list.INode)
	if !ok {
		t.Fatalf("✘ 测试定时器未实现 list.INode: %T", timer)
	}
	if got := m.commitDispatch(m.wheels[wi], parent, node, task, ch); got != dispatchSent {
		t.Fatalf("✘ 生产派发入口拒绝派发本定时器: result=%d", got)
	}
	for {
		select {
		case got := <-ch:
			if got.timer == timer {
				return got
			}
		default:
			t.Fatal("✘ 结论是已投递，调度通道里却取不到这条任务")
		}
	}
}

// assertWheelConsistent 断言某个裸轮计数与链表严格一致（无并发时可精确相等）。
func assertWheelConsistent(t *testing.T, m *TimerManager, wi TimerType, want int64) {
	t.Helper()
	w := m.wheels[wi]
	if c := w.timerCount.Load(); c != want {
		t.Fatalf("✘ wheel[%d] 计数异常: got=%d want=%d len=%d", wi, c, want, w.tickWheel.Length())
	}
	if l := int64(w.tickWheel.Length()); l != want {
		t.Fatalf("✘ wheel[%d] 链表长度与计数不一致: count=%d len=%d", wi, w.timerCount.Load(), l)
	}
}

// ----------------------------------------------------------------------------
// 测试 1：-1 永久定时器，每次 Tick 后必须恰好重入队 1 条最新代次的调度项，
// 重新入链后计数/链表恢复；count 恒为无限哨兵，永不递减。
// ----------------------------------------------------------------------------

func TestTickReschedule_InfiniteTimerReentersQueue(t *testing.T) {
	m := newRawWheelManager()
	tm, err := NewTimer[*plainTimer](50, InfiniteCount, func(p *plainTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatal(err)
	}
	p := tm.GetParent()
	if p.wheel.Load() != nil {
		t.Fatal("池中复用实例带有残留的时间轮归属，前置状态不干净")
	}

	// 初始入链：模拟轮协程消费 AddTimer 的入队任务
	if err := m.AddTimer(tm); err != nil {
		t.Fatal(err)
	}
	wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
	assertWheelConsistent(t, m, TimerTypeMillisecond, 1)

	const rounds = 3
	for r := 0; r < rounds; r++ {
		beforeGen := p.gen.Load()
		task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
		assertWheelConsistent(t, m, TimerTypeMillisecond, 0)

		m.executeTimer(task)

		if got := tm.n.Load(); got != int64(r+1) {
			t.Fatalf("✘ 第 %d 轮 Tick 未正确执行: n=%d", r+1, got)
		}
		if c := p.GetRemainingCount(); c != InfiniteCount {
			t.Fatalf("✘ 永久定时器剩余次数被误扣减: %d", c)
		}
		if !p.IsActive() {
			t.Fatal("✘ Tick 后定时器不应失活")
		}
		if p.gen.Load() <= beforeGen {
			t.Fatalf("✘ 续期未推进代次: %d -> %d", beforeGen, p.gen.Load())
		}

		// 必须恰好回队 1 条，且代次为最新、归属管理器不变
		select {
		case readd := <-m.wheels[TimerTypeMillisecond].addTimerChan:
			if readd.timer != TimerInterface(tm) {
				t.Fatal("✘ 轮通道出现非本定时器的任务")
			}
			if readd.gen != p.gen.Load() {
				t.Fatalf("✘ 续期调度项代次过期: task.gen=%d 当前 gen=%d", readd.gen, p.gen.Load())
			}
			if readd.mgr != m {
				t.Fatal("✘ 续期调度项归属管理器被改嫁")
			}
			m.handleTimerAdd(m.wheels[TimerTypeMillisecond], readd)
		default:
			t.Fatal("✘ Tick 后未产生续期入队任务（定时器未回到 manager 队列）")
		}
		assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
		assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
	}
	t.Logf("✔ %d 轮 Tick 续期：每轮恰好回队 1 条最新代次任务，入链后计数与链表恒一致", rounds)
}

// ----------------------------------------------------------------------------
// 测试 2：count>0 的定时器逐次 Tick 续期，恰好执行 count 次后不再入队；
// 次数收敛为 0（不出现 -1），count==1 / count==0 边界同口径。
// ----------------------------------------------------------------------------

func TestTickReschedule_FiniteCountUntilExhausted(t *testing.T) {
	cases := []struct {
		name        string
		count       int64
		ticks       int64
		neverLinked bool
	}{
		{"count=3", 3, 3, false},
		{"count=1边界", 1, 1, false},
		{"count=0边界", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRawWheelManager()
			tm, err := NewTimer[*plainTimer](50, tc.count, func(p *plainTimer) { p.n.Store(0) })
			if err != nil {
				t.Fatal(err)
			}
			p := tm.GetParent()
			if p.wheel.Load() != nil {
				t.Fatal("池中复用实例带有残留的时间轮归属，前置状态不干净")
			}

			if tc.count > 0 {
				if err := m.AddTimer(tm); err != nil {
					t.Fatal(err)
				}
				wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
				assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
			}

			for r := int64(0); r < tc.ticks; r++ {
				task := dispatchAndUnlink(t, m, TimerTypeMillisecond, tm, p)
				m.executeTimer(task)

				if got := tm.n.Load(); got != r+1 {
					t.Fatalf("✘ 第 %d 次 Tick 未执行: n=%d", r+1, got)
				}
				want := tc.count - (r + 1)
				if got := p.GetRemainingCount(); got != want {
					t.Fatalf("✘ 第 %d 次 Tick 后剩余次数: got=%d want=%d", r+1, got, want)
				}
				if want > 0 {
					// count 仍 >0：必须重新入队并回链
					wheelAddTimer(t, m, TimerTypeMillisecond, tm, true)
					assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
					assertWheelConsistent(t, m, TimerTypeMillisecond, 1)
				} else {
					// 最后一次执行：不再续期，节点保持已摘链状态自然消亡
					assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
					assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
					if p.wheel.Load() != nil {
						t.Fatal("✘ 次数耗尽后不应仍挂在时间轮上")
					}
				}
			}

			if got := p.GetRemainingCount(); got != 0 {
				t.Fatalf("✘ 耗尽后剩余次数应为 0（不得为负）: %d", got)
			}
			if tc.neverLinked {
				// count=0 从未入链：即便业务误触发一次到期执行，也不得回队
				m.executeTimer(timerTask{timer: tm, gen: p.gen.Load(), mgr: m})
				assertWheelRequeueOnce(t, m, TimerTypeMillisecond, tm)
				assertWheelConsistent(t, m, TimerTypeMillisecond, 0)
			}
		})
	}
	t.Log("✔ count>0 逐次回链续期、耗尽即止，计数收敛为 0 无 -1")
}

// ----------------------------------------------------------------------------
// 测试 3：完整链路（Run + TimeTick + 真实轮协程）业务形态压测。
//
// 看门狗持续校验：
//   - 逐轮 timerCount >= 0 且 timerCount <= 链表长度（见说明 1）；
//   - 全局计数求和不超过存活定时器数（超发即幻影）；
//   - 每个定时器 GetRemainingCount() 永不为负（业务只设正数或 InfiniteCount）。
//
// 说明 1：逐轮只断言 0 <= count <= len，不要求严格相等。相等在无并发时成立
// （测试 1/2 逐轮精确断言），并发下会瞬时偏移：timerCount 的扣减与摘链在同一
// wheelLock 临界区内完成，而 list 在 Range 期间走延迟删除（delPending），节点
// 真正离开链表、len 随之递减要等到整轮遍历收尾，故观测窗口内可能 len > count。
// 非负、不超存活数、不多于链表长度这三条则是任何时刻都成立的硬不变量，
// 最终一致由「全部移除后收敛归零」保证。
// 说明 2：扰动协程刻意不做裸 AddTimer 洪泛——那是纯背压场景（已由
// TestRegression_BackpressureNoGoroutineExplosion 覆盖），会把有限的通道容量
// 打满并触发设计内的丢弃/续期失败告警，干扰本次「回链正确性」的观测口径。
// 说明 3：压测覆盖「Pause → AddTimer 复活」（rpc 断线重连的真实形态）。
// Remove 的语义是「彻底作废 + 归还对象池」，作废后指针即成为悬垂引用，故本用例
// 只在收尾各调用一次。历史缺陷（30 个逻辑定时器只剩 7 个唯一实例、0 次 Tick）
// 的根因是「Remove 回池而业务仍持有同一指针」，如今由三条协议共同封死：
// inPool 的 CAS 认领保证同一实例只发一份、released 粘性标记让 AddTimer 拒绝复活
// 已作废实例、Tick 在飞时归还请求延迟到回调结束（endTick）才落地。
// ----------------------------------------------------------------------------

func TestTickReschedule_InvariantUnderLoad(t *testing.T) {
	Run(context.Background())
	putsStart := GetTimerPoolStats().Puts
	m := GetDefaultManager()
	if m == nil {
		t.Fatal("默认管理器未就绪")
	}

	const numTimers = 30
	timers := make([]*plainTimer, 0, numTimers)
	for i := 0; i < numTimers; i++ {
		cnt := int64(InfiniteCount)
		if i%2 == 0 {
			cnt = int64(3 + i%5)
		}
		// 对象池会复用实例，n 带历史值，必须归零后才是本轮真实 Tick 数
		tm, err := NewTimer[*plainTimer](int64(10+i*70), cnt, func(p *plainTimer) {
			p.n.Store(0)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := AddTimer(tm); err != nil {
			t.Fatal(err)
		}
		timers = append(timers, tm)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	var maxLinked atomic.Int64
	created := make([][2]int64, numTimers) // [uid, 初始count]
	for i, tm := range timers {
		created[i] = [2]int64{tm.GetUID(), tm.GetRemainingCount()}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var sumCount int64
			for wi, w := range m.wheels {
				w.wheelLock.RLock()
				c := w.timerCount.Load()
				l := int64(w.tickWheel.Length())
				w.wheelLock.RUnlock()
				if c < 0 {
					t.Errorf("✘ 压测期间 wheel[%d] 计数为负: %d", wi, c)
					return
				}
				if c > l {
					t.Errorf("✘ wheel[%d] 计数多于链表长度（幻影计数）: count=%d len=%d", wi, c, l)
					return
				}
				if c > numTimers {
					t.Errorf("✘ wheel[%d] 计数超发: count=%d 存活定时器仅 %d", wi, c, numTimers)
					return
				}
				sumCount += c
			}
			if sumCount > maxLinked.Load() {
				maxLinked.Store(sumCount)
			}
			if sumCount > numTimers {
				t.Errorf("✘ 全局计数 %d 超过存活定时器数 %d（幻影定时器）", sumCount, numTimers)
				return
			}
			for ti, tm := range timers {
				p := tm.GetParent()
				if raw := p.count.Load(); raw != CountCorrectionValue && raw < 0 {
					t.Errorf("✘ count 原始值为负: raw=%d | idx=%d uid=%d(创建时=%d) initCount=%d gen=%d active=%v interval=%d wheel=%v",
						raw, ti, p.GetUID(), created[ti][0], created[ti][1], p.gen.Load(), p.IsActive(),
						p.GetInterval(), p.wheel.Load() != nil)
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// 业务侧并发扰动：重新入队、改次数、Pause 后复活（rpc 重连形态）。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tm := timers[(idx*3)%numTimers]
				switch idx % 4 {
				case 0:
					_ = AddTimer(tm)
				case 1:
					tm.SetCount(InfiniteCount)
				case 2:
					tm.SetCount(5)
				case 3:
					tm.Pause()
					time.Sleep(2 * time.Millisecond)
					_ = AddTimer(tm)
				}
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	time.Sleep(800 * time.Millisecond)
	close(stop)
	wg.Wait()
	<-done

	// Tick 总数必须在作废之前统计：Remove 之后实例已归池，随时可能被下一个
	// NewTimer 认领，再读它的字段就是悬垂访问。
	var tickSum int64
	for _, tm := range timers {
		tickSum += tm.n.Load()
	}

	// 全量作废并等待收敛：全局计数与逐轮链表长度归零
	for _, tm := range timers {
		tm.Remove()
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var sumCount, sumLen int64
		for _, w := range m.wheels {
			w.wheelLock.RLock()
			sumCount += w.timerCount.Load()
			sumLen += int64(w.tickWheel.Length())
			w.wheelLock.RUnlock()
		}
		if sumCount == 0 && sumLen == 0 {
			break
		}
		if time.Now().After(deadline) {
			var detail string
			for wi, w := range m.wheels {
				detail += fmt.Sprintf("wheel[%d] count=%d len=%d; ",
					wi, w.timerCount.Load(), w.tickWheel.Length())
			}
			TimeStop(context.Background())
			t.Fatalf("✘ 全部移除后未收敛归零: count=%d len=%d | %s", sumCount, sumLen, detail)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stats := m.GetStats()
	TimeStop(context.Background())

	t.Logf("迭代统计: Executed=%d Added=%d Removed=%d Dropped=%d RescheduleFailed=%d | 业务Tick总和=%d 在轮峰值=%d",
		stats.TimersExecuted, stats.TimersAdded, stats.TimersRemoved, stats.TimersDropped,
		stats.TimersRescheduleFailed, tickSum, maxLinked.Load())

	if stats.TimersExecuted == 0 {
		ch := currentTimerChannel()
		t.Fatalf("✘ 压测期间没有任何 Tick 被正确执行（续期链路中断）| chanLen=%d mapLen=%d exec=%d added=%d",
			len(ch), timerManagerMap.Length(), stats.TimersExecuted, stats.TimersAdded)
	}
	if stats.TimersRescheduleFailed > 0 {
		t.Errorf("✘ 存在续期最终失败的定时器(丢失风险): RescheduleFailed=%d Dropped=%d",
			stats.TimersRescheduleFailed, stats.TimersDropped)
	}
	// 净归还次数超过存活实例数 ⇒ 同一实例被重复 Put（所有权失控的直接证据）
	if puts := GetTimerPoolStats().Puts - putsStart; puts > numTimers {
		t.Errorf("✘ 对象池归还次数超过定时器数量（疑似重复归还）: Puts=+%d 定时器=%d", puts, numTimers)
	}
	t.Logf("✔ 压测通过: Executed=%d Migrations=%d Dropped=%d，全程计数非负且收敛归零",
		stats.TimersExecuted, stats.WheelMigrations, stats.TimersDropped)
}
