package localtimer

import (
	"testing"
	"time"

	"touchgocore/util"
)

// ============================================================================
// 断言用例（S64 配套 + 阶段12 复核整改 F1/F3）：逐节点回锁认领的三条结论。
//
// 变更前一次扫描整轮持锁，派发路径没有「认领失败」这种结局；改成短临界区后，
// 「快照判定」与「抢到锁」之间出现了窗口，归属可能被业务侧拿走。这个窗口只有
// 直接构造才能覆盖，压测式用例（TestTickReschedule_InvariantUnderLoad）只在
// 恰好撞上时才有意义，撞不上时恒绿，等于没测。
// ============================================================================

// linkInto 把定时器手动入链到指定轮（绕过 AddTimer 的选轮逻辑，以便造出
// 「节点在 A 轮、而 AddTimer 复活会把它投给 B 轮」这种跨轮交错）。
func linkInto(t *testing.T, m *TimerManager, wi TimerType, tm TimerInterface) {
	t.Helper()
	parent := tm.GetParent()
	m.handleTimerAdd(m.wheels[wi], timerTask{timer: tm, gen: parent.gen.Load(), mgr: m})
	if parent.wheel.Load() != m.wheels[wi] {
		t.Fatalf("✘ 前置入链未生效: 归属不是 wheel[%d]", wi)
	}
}

// TestCommitDispatchFullKeepsOwnership 目标通道满：归属必须原样还回本轮，
// 节点留在链上、计数不动，否则该定时器此后每个 tick 都被 ownedWheel 判给「无轮」
// 而跳过 —— 等价于永久停摆，且现场看不出来。
func TestCommitDispatchFullKeepsOwnership(t *testing.T) {
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeMinute]
	tm, err := NewTimer[*plainTimer](60*1000, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	linkInto(t, m, TimerTypeMinute, tm)
	parent := tm.GetParent()

	full := make(chan timerTask) // 无接收方的零容量通道：select 必走 default
	countBefore := m.stats.timersDropped.Load()
	task := timerTask{timer: tm, gen: parent.gen.Load(), mgr: m}

	if got := m.commitDispatch(wheel, parent, tm, task, full); got != dispatchFull {
		t.Fatalf("✘ 通道满时的结论应为 dispatchFull, got=%d", got)
	}
	if parent.wheel.Load() != wheel {
		t.Fatal("✘ 投递失败后归属没有还回本轮，此后每个 tick 都会跳过它")
	}
	assertWheelConsistent(t, m, TimerTypeMinute, 1)
	if l := int64(wheel.tickWheel.Length()); l != 1 {
		t.Fatalf("✘ 投递失败不应摘链: len=%d", l)
	}
	if d := m.stats.timersDropped.Load(); d != countBefore {
		t.Fatalf("✘ 计数只由 processWheelTick 汇总, commitDispatch 不该改: %d→%d", countBefore, d)
	}
}

// TestCommitDispatchSkippedWhenOwnershipLost 快照判定到抢锁之间归属易主（已改嫁
// 别的轮、或已在别的协程认领途中）：本次必须什么都不做，连摘链都不能碰。
func TestCommitDispatchSkippedWhenOwnershipLost(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(parent *Timer, other *TimerWheel)
	}{
		{"已改嫁别的轮", func(p *Timer, other *TimerWheel) { p.wheel.Store(other) }},
		{"已在投递途中", func(p *Timer, _ *TimerWheel) { p.wheel.Store(migratingMarker) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子用例一套独立的裸轮管理器：共享会让上一例留下的计数污染本例的断言
			m := newRawWheelManager()
			aWheel := m.wheels[TimerTypeMinute]

			tm, err := NewTimer[*plainTimer](60*1000, InfiniteCount, nil)
			if err != nil {
				t.Fatal(err)
			}
			linkInto(t, m, TimerTypeMinute, tm)
			parent := tm.GetParent()
			// 节点仍在 A 链上（计数 1），但归属已被改成别的值，模拟别的路径抢先动手
			tc.mutate(parent, m.wheels[TimerTypeSecond])

			task := timerTask{timer: tm, gen: parent.gen.Load(), mgr: m}
			if got := m.commitDispatch(aWheel, parent, tm, task, make(chan timerTask, 1)); got != dispatchSkipped {
				t.Fatalf("✘ 归属易主后应返回 dispatchSkipped, got=%d", got)
			}
			if c := aWheel.timerCount.Load(); c != 1 {
				t.Fatalf("✘ 未认领却扣了计数: count=%d", c)
			}
			if l := int64(aWheel.tickWheel.Length()); l != 1 {
				t.Fatalf("✘ 未认领却摘了链: len=%d", l)
			}
		})
	}
}

// TestTickWheelUnlinksStaleInactiveNode 「已失活却仍挂在轮上」的残留节点必须被
// 清掉：不处理它既不会被调度也不会被回收，计数永久多 1。
func TestTickWheelUnlinksStaleInactiveNode(t *testing.T) {
	m := newRawWheelManager()
	wheel := m.wheels[TimerTypeMinute]
	tm, err := NewTimer[*plainTimer](60*1000, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	linkInto(t, m, TimerTypeMinute, tm)
	parent := tm.GetParent()
	parent.isActive.Store(false) // 业务已判它不活跃，但节点还挂在链上
	removedBefore := m.stats.timersRemoved.Load()

	if dropped := m.tickWheelSection(wheel, TimerTypeMinute); dropped != 0 {
		t.Fatalf("✘ 清理残留不该算作丢弃: dropped=%d", dropped)
	}
	assertWheelConsistent(t, m, TimerTypeMinute, 0)
	if parent.wheel.Load() != nil {
		t.Fatal("✘ 清理后仍挂着轮归属")
	}
	if r := m.stats.timersRemoved.Load(); r != removedBefore+1 {
		t.Fatalf("✘ 真正摘链处未计数: %d→%d", removedBefore, r)
	}
}

// TestUnlinkStaleBlocksBehindParentMu 是复核发现的回归：unlinkStale 原先只挂
// wheelLock，而认领后的「摘链 + 扣计数 + 清归属」不再与业务临界区互斥。
//
// 时序：扫描判定节点失活 → unlinkStale 取 A 轮锁并把归属改成在途标记 →
// 业务此刻走 AddTimer 复活（只需 mu + 目标轮锁），handleTimerAdd 认在途标记为
// 「无归属」从而放行入链 → unlinkStale 随后 Remove() 摘的是新主的链、扣的是
// A 的计数，最后的 Store(nil) 又抹掉新主归属：新轮永久幻影 +1，定时器脱离调度。
// 修法与 commitDispatch 同序取 mu，于是本用例断言「持 mu 时它必须等着」。
func TestUnlinkStaleBlocksBehindParentMu(t *testing.T) {
	m := newRawWheelManager()
	aWheel := m.wheels[TimerTypeMinute]
	tm, err := NewTimer[*plainTimer](2*1000, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 节点在分钟轮，而 nextTime 落在秒级 → 复活时 AddTimer 会把它投给秒轮（跨轮）
	tm.GetParent().nextTime.Store(util.CurrentMS() + 2000)
	linkInto(t, m, TimerTypeMinute, tm)
	parent := tm.GetParent()
	parent.isActive.Store(false)

	parent.mu.Lock()
	done := make(chan struct{})
	go func() {
		m.unlinkStale(aWheel, parent, tm)
		close(done)
	}()

	select {
	case <-done:
		parent.mu.Unlock()
		t.Fatal("✘ unlinkStale 没取 parent.mu：它能与复活临界区交错，把跨轮入链的节点错摘并扣错计数")
	case <-time.After(50 * time.Millisecond):
	}
	parent.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("✘ 释放 mu 后 unlinkStale 没有继续（疑似锁序错误导致死锁）")
	}
	assertWheelConsistent(t, m, TimerTypeMinute, 0)
}

// TestReviveDuringStaleScanKeepsWheelInvariant 真实并发下的同类判据：一边持续
// 扫描挂着失活残留节点的轮，一边由业务复活它，结束后每档轮的计数都必须与链表
// 长度严格相等（无幻影、无漏扣）。
//
// 说明这条与上面那条的分工：本用例是「不变式守门员」，撞不上交错时恒绿（实测去掉
// unlinkStale 的 mu 仍然通过），所以锁序本身由 TestUnlinkStaleBlocksBehindParentMu
// 那条确定性用例守住，这里只兜住后续再改锁粒度时的整体一致性。
func TestReviveDuringStaleScanKeepsWheelInvariant(t *testing.T) {
	for round := 0; round < 200; round++ {
		m := newRawWheelManager()
		aWheel := m.wheels[TimerTypeMinute]
		tm, err := NewTimer[*plainTimer](2*1000, InfiniteCount, nil)
		if err != nil {
			t.Fatal(err)
		}
		parent := tm.GetParent()
		parent.nextTime.Store(util.CurrentMS() + 2000)
		linkInto(t, m, TimerTypeMinute, tm)
		parent.isActive.Store(false)

		stop := make(chan struct{})
		scannerDone := make(chan struct{})
		go func() {
			defer close(scannerDone)
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.processWheelTick(aWheel, TimerTypeMinute)
				if dst := m.wheels[parent.GetType()]; len(dst.addTimerChan) > 0 {
					task := <-dst.addTimerChan
					m.handleTimerAdd(dst, task)
				}
			}
		}()

		if err := m.AddTimer(tm); err != nil {
			close(stop)
			<-scannerDone
			t.Fatalf("✘ 复活失败: %v", err)
		}
		close(stop)
		<-scannerDone

		for wi, w := range m.wheels {
			if c, l := w.timerCount.Load(), int64(w.tickWheel.Length()); c != l {
				t.Fatalf("✘ round=%d wheel[%d] 计数与链表漂移: count=%d len=%d", round, wi, c, l)
			}
		}
		if w := m.wheels[parent.GetType()]; w.timerCount.Load()+aWheel.timerCount.Load() != 1 {
			t.Fatalf("✘ round=%d 复活后定时器丢失或重复: minute=%d own=%d",
				round, aWheel.timerCount.Load(), w.timerCount.Load())
		}
	}
}

// TestGetWheelTimerCountBounds 越界档位与空轮位都必须安静返回 0：
// 采集器每 15 秒就按 TimerType 遍历一次，这里 panic 会把整次抓取带崩。
func TestGetWheelTimerCountBounds(t *testing.T) {
	m := newRawWheelManager()
	if got := m.GetWheelTimerCount(TimerType(99)); got != 0 {
		t.Fatalf("✘ 越界档位应返回 0, got=%d", got)
	}
	if got := m.GetWheelTimerCount(TimerType(-1)); got != 0 {
		t.Fatalf("✘ 负数档位应返回 0, got=%d", got)
	}
	m.wheels[TimerTypeHour] = nil
	if got := m.GetWheelTimerCount(TimerTypeHour); got != 0 {
		t.Fatalf("✘ 空轮位应返回 0, got=%d", got)
	}
	if got := m.GetWheelTimerCount(TimerTypeMinute); got != 0 {
		t.Fatalf("✘ 空轮不应受邻居置空影响, got=%d", got)
	}
}

// TestGetQueueStatsLengthMatchesWheels 未 Close/未建轮时 WheelLen 仍与轮表等长：
// 采集器按下标配「每档一条序列」，长度不足会让整档序列凭空消失。
func TestGetQueueStatsLengthMatchesWheels(t *testing.T) {
	m := newRawWheelManager()
	s := m.GetQueueStats()
	if len(s.WheelLen) != len(m.wheels) || len(s.WheelCap) != len(m.wheels) {
		t.Fatalf("✘ 长度与轮表不符: len=%d cap=%d wheels=%d", len(s.WheelLen), len(s.WheelCap), len(m.wheels))
	}
	for i := range s.WheelCap {
		if s.WheelCap[i] != int(MaxAddTimerChannelNum) {
			t.Fatalf("✘ wheel[%d] 容量口径错: got=%d want=%d", i, s.WheelCap[i], MaxAddTimerChannelNum)
		}
	}
	m.wheels[TimerTypeHour] = nil
	if s := m.GetQueueStats(); s.WheelLen[TimerTypeHour] != 0 || s.WheelCap[TimerTypeHour] != 0 {
		t.Fatal("✘ 空轮位应记为 0 而不是保留邻居通道的数值")
	}
}
