package localtimer

// ============================================================================
// B-F5（槽位B）：确定性 panic 节点必须在有限拍内被隔离，轮恢复桶扫描节奏。
//
// 缺陷形态：scanSlot 对每次扫描必炸的节点只做 markResidue → 游标冻结在本桶之前
// → 落后满一圈后整档轮退化为每拍整环扫描 + 每拍一条 vars.Error（异步缓冲满时
// 单条最长阻塞 5 秒，日志洪水拖死整轮）。
// 修复后：同一桶连续 panic 达到 slotPanicQuarantineThreshold 后，逐节点定位肇事者
// 并按 unlinkStale 语义摘除隔离；panic 日志经所属轮的 wheelShouldWarn 限频。
// ============================================================================

import (
	"testing"
	"time"

	"touchgocore/list"
	"touchgocore/util"
)

// qaBBomb 「GetParent 必炸」的业务节点：确定性 panic 源
type qaBBomb struct {
	Timer
}

func (*qaBBomb) Tick() {}

func (*qaBBomb) GetParent() *Timer { panic("qaB: 业务实现的 GetParent 必炸") }

// qaBPlainNode 正常节点（手工构造，不入池）
type qaBPlainNode struct {
	Timer
}

func (*qaBPlainNode) Tick() {}

// TestQaBSlotRingQuarantinesDeterministicPanic 环级确定性编排：
// 炸点与正常节点同桶，逐拍驱动 RangeWindow。
//   - 前 threshold-1 拍：按残留处理，游标冻结、炸点仍在环上（既往行为不变）；
//   - 第 threshold 拍：定位并隔离炸点，游标照常推进（不再残留）；
//   - 其后各拍：无 panic、无隔离、日志钩子零增量（洪水停止），正常节点持续被扫到。
func TestQaBSlotRingQuarantinesDeterministicPanic(t *testing.T) {
	const sec = util.MILLISECONDS_OF_SECOND
	r := newTestRing(sec)
	base := util.CurrentMS() / sec
	r.cursor.Store(base)

	var quarantined []list.INode
	r.quarantineNode = func(n list.INode) {
		quarantined = append(quarantined, n)
		n.GetNode().Remove()
	}
	warnCalls := 0
	r.shouldWarn = func() bool { warnCalls++; return true }

	bomb := &qaBBomb{}
	bomb.nextTime.Store((base + 1) * sec) // 落点无所谓：stepOf 会因 panic 钳到 cursor+1
	if !r.Add(bomb) {
		t.Fatal("✘ 炸点入链失败")
	}
	healthy := &qaBPlainNode{}
	healthy.nextTime.Store((base+1)*sec + sec/2) // 与炸点同桶（base+1）
	if !r.Add(healthy) {
		t.Fatal("✘ 正常节点入链失败")
	}
	if got := r.Length(); got != 2 {
		t.Fatalf("✘ 前置条件：环内应有 2 个节点: %d", got)
	}

	cb := func(n list.INode) bool {
		if _, ok := n.(*qaBBomb); ok {
			panic("qaB: 扫描回调在炸点上必炸")
		}
		return true
	}
	scanOnce := func() {
		t.Helper()
		defer func() {
			if err := recover(); err != nil {
				t.Fatalf("✘ panic 逃出 RangeWindow: %v", err)
			}
		}()
		r.RangeWindow((base+1)*sec+sec/2, cb)
	}

	// 前 threshold-1 拍：残留处理，游标冻结在 base，炸点仍在环上
	for i := 1; i < slotPanicQuarantineThreshold; i++ {
		scanOnce()
		if got := r.cursor.Load(); got != base {
			t.Fatalf("✘ 第 %d 拍游标提前推进（残留语义被破坏）: cursor=%d want=%d", i, got, base)
		}
		if len(quarantined) != 0 {
			t.Fatalf("✘ 第 %d 拍未到阈值就触发了隔离", i)
		}
	}
	if got := r.Length(); got != 2 {
		t.Fatalf("✘ 隔离前炸点不该离开桶环: len=%d", got)
	}

	// 第 threshold 拍：隔离炸点，游标推进
	scanOnce()
	if len(quarantined) != 1 || quarantined[0] != list.INode(bomb) {
		t.Fatalf("✘ 第 %d 拍未隔离肇事节点: %v", slotPanicQuarantineThreshold, quarantined)
	}
	if got := r.cursor.Load(); got != base+1 {
		t.Fatalf("✘ 隔离后游标未推进（轮仍冻结）: cursor=%d want=%d", got, base+1)
	}
	if got := r.Length(); got != 1 {
		t.Fatalf("✘ 隔离后环内应只剩正常节点: len=%d", got)
	}
	warnsAtQuarantine := warnCalls
	if warnsAtQuarantine != slotPanicQuarantineThreshold {
		t.Fatalf("✘ 日志钩子调用次数与 panic 拍数不符（每拍至多一条）: %d", warnsAtQuarantine)
	}

	// 其后各拍：轮恢复桶扫描，无 panic、无隔离、日志零增量
	for i := 0; i < 3; i++ {
		scanOnce()
	}
	if len(quarantined) != 1 {
		t.Fatalf("✘ 隔离之后又发生了隔离（炸点未摘干净）: %v", quarantined)
	}
	if warnCalls != warnsAtQuarantine {
		t.Fatalf("✘ 隔离之后日志钩子仍被逐拍调用（洪水未止）: %d -> %d", warnsAtQuarantine, warnCalls)
	}
	if got := r.cursor.Load(); got < base+1 {
		t.Fatalf("✘ 隔离后游标倒退: %d", got)
	}
}

// TestQaBManagerQuarantinesPanicNode 管理器级（生产接线）：NewTimerManager 建的轮
// 必须挂好隔离/限频钩子；真实轮协程驱动下，炸点在有限拍内被摘除，
// 计数与环长恢复一致，其后正常定时器照常派发。
func TestQaBManagerQuarantinesPanicNode(t *testing.T) {
	mgr := NewTimerManager()
	defer mgr.Close()

	for i, w := range mgr.wheels {
		if w.tickWheel.quarantineNode == nil || w.tickWheel.shouldWarn == nil {
			t.Fatalf("✘ 第 %d 档轮未挂隔离/限频钩子（生产接线缺失）", i)
		}
	}

	wheel := mgr.wheels[TimerTypeMillisecond]
	bomb := &qaBBomb{}
	bp := &bomb.Timer
	bp.nextTime.Store(util.CurrentMS() + 5)
	bp.isActive.Store(true)
	if !wheel.tickWheel.Add(bomb) { // stepOf 的 panic 被吞掉，按 cursor+1 钳制入链
		t.Fatal("✘ 炸点灌入失败")
	}
	bp.wheel.Store(wheel)
	wheel.timerCount.Add(1)

	// 正常定时器：炸点被隔离后必须照常派发（轮恢复了桶扫描）
	healthy, err := NewTimer[*qaBBeatTimer](30, InfiniteCount, func(p *qaBBeatTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	if err := mgr.AddTimer(healthy); err != nil {
		t.Fatalf("✘ AddTimer 失败: %v", err)
	}
	defer healthy.Pause()

	// 毫秒轮每 1ms 一拍，阈值 8 拍：隔离应在几十毫秒内发生，3 秒是宽裕上界
	qaBWaitFor(t, 3*time.Second, "确定性炸点未在有限拍内被隔离（timerCount 未回落）", func() bool {
		return wheel.timerCount.Load() == 0 && wheel.tickWheel.Length() == 0
	})
	if s := mgr.GetStats(); s.TimersRemoved == 0 {
		t.Fatal("✘ 隔离未计入 timersRemoved（监控上看不出这次摘除）")
	}

	// 系统未 Run，没有消费协程：从全局调度通道确认正常定时器的派发确实到达
	channels := currentTimerChannels()
	deadline := time.Now().Add(3 * time.Second)
	dispatched := false
	for time.Now().Before(deadline) && !dispatched {
		for _, ch := range channels {
			select {
			case task := <-ch:
				if task.timer == TimerInterface(healthy) {
					dispatched = true
				}
			default:
			}
		}
		if !dispatched {
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !dispatched {
		t.Fatal("✘ 隔离之后正常定时器仍未被派发（轮没有恢复桶扫描）")
	}
}
