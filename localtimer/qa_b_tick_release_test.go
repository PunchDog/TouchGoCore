package localtimer

// ============================================================================
// B-F3（槽位B）：beginTick 代次失配分支不得让挂起的归还请求永久悬挂。
//
// 缺陷形态：消费协程在 beginTick 的「inTick.Add(1) → 代次校验」窗口内，业务
// Remove 因 inTick>0 判 releaseDeferred（置 pendingRelease）并推进代次；随后
// beginTick 走失配分支只做裸 inTick.Add(-1)，pendingRelease 的唯一落地者
// （endTick）永远不会被调用——实例不回池、池 puts 少计，且此后 AddTimer 一律
// 被 abandoned 拒绝，实例彻底泄漏在使用者手里。
//
// 窗口极窄，自然命中不现实：按包内既有手法手动编排原子时序（对照
// p1_migration_release_test.go 的 S13/S14 用例形态）确定性复现。
// ============================================================================

import (
	"testing"
)

// TestQaBBeginTickStaleGenLandsPendingRelease 编排出的确定性时序：
//  1. 先登记在飞（模拟消费协程 A 已过 beginTick 的 inTick.Add(1)、未到代次校验）；
//  2. 业务 Remove 走真实路径：inTick>0 → pendingRelease 挂起 + 代次推进；
//  3. 撤销第 1 步的补偿计数——A 的那次 +1 由下面真正的 beginTick 重放，
//     此刻实例状态与「缺陷交错完成后、A 尚未做代次校验」逐字段一致；
//  4. 陈旧代次的 beginTick 走失配分支：修复后必须经 endTick 把归还落地。
func TestQaBBeginTickStaleGenLandsPendingRelease(t *testing.T) {
	tm, err := NewTimer[*qaBBeatTimer](1000, InfiniteCount, func(p *qaBBeatTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	parent := tm.GetParent()

	putsBefore := GetTimerPoolStats().Puts
	staleGen := parent.gen.Load()

	// 1) 模拟消费协程 A 进入 beginTick 的在飞窗口
	parent.inTick.Add(1)

	// 2) 窗口内业务作废（真实 Remove 路径）：inTick>0 → releaseDeferred
	tm.Remove()
	if !parent.pendingRelease.Load() {
		t.Fatal("✘ 前置条件：Remove 未把归还挂起（pendingRelease 未置位），本用例无从观测")
	}
	if parent.gen.Load() == staleGen {
		t.Fatal("✘ 前置条件：Remove 未推进代次，失配分支不会被触发")
	}

	// 3) 撤掉补偿计数，A 的 +1 交由下面真正的 beginTick 重放
	parent.inTick.Add(-1)

	// 4) 陈旧代次的调度项到达 beginTick：必须拒绝执行，且把挂起的归还落地
	if parent.beginTick(staleGen) {
		t.Fatal("✘ 过期代次的调度项竟然登记成功")
	}
	if got := parent.inTick.Load(); got != 0 {
		t.Fatalf("✘ 失配分支返回后 inTick 未归零: %d", got)
	}
	if !parent.inPool.Load() {
		t.Fatal("✘ 失配分支裸减在飞计数，挂起的归还请求永久悬挂（实例不回池）")
	}
	if delta := GetTimerPoolStats().Puts - putsBefore; delta != 1 {
		t.Fatalf("✘ 归还未落地或重复落地: puts 增量=%d want=1", delta)
	}
	t.Logf("✔ 失配分支经 endTick 落地挂起的归还，puts 增量=1")
}

// TestQaBBeginTickStaleGenBareDecrementKept 对照用例：没有挂起归还时，
// 失配分支维持裸减语义（不误触 endTick/归还路径），代次匹配的登记照常成功。
func TestQaBBeginTickStaleGenBareDecrementKept(t *testing.T) {
	tm, err := NewTimer[*qaBBeatTimer](1000, InfiniteCount, func(p *qaBBeatTimer) { p.n.Store(0) })
	if err != nil {
		t.Fatalf("✘ NewTimer 失败: %v", err)
	}
	parent := tm.GetParent()
	current := parent.gen.Load()
	parent.nextGen() // 模拟新主人认领后推进代次（pendingRelease 保持为假）

	if parent.pendingRelease.Load() {
		t.Fatal("✘ 前置条件：pendingRelease 不该置位")
	}
	if parent.beginTick(current) {
		t.Fatal("✘ 过期代次的调度项竟然登记成功")
	}
	if got := parent.inTick.Load(); got != 0 {
		t.Fatalf("✘ 拒绝后 inTick 未回退: %d", got)
	}
	if parent.inPool.Load() {
		t.Fatal("✘ 无挂起归还时失配分支误把实例送回了池")
	}
	if !parent.beginTick(parent.gen.Load()) {
		t.Fatal("✘ 当代调度项应能登记在飞")
	}
	parent.endTick()
	if got := parent.inTick.Load(); got != 0 {
		t.Fatalf("✘ endTick 后 inTick 未归零: %d", got)
	}
	tm.Remove()
}
