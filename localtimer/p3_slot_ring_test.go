package localtimer

// 阶段13（S67）桶环回归用例：分桶只收窄扫描范围，绝不能改动派发/迁移/计数协议。
//
// 判定口径全部沿用变更前的逻辑（nextTime 与墙钟复核、remaining 与档位下界比较），
// 因此这里要证明的是三件事：
//   1. 未到期的桶不再被扫到（收益）；
//   2. 到期的一个都不漏，包括「入链即到期」被钳到身后一格的节点（正确性）；
//   3. 游标落后超过一圈、时钟回拨、桶间物理迁移等退化路径仍与原实现同果。

import (
	"testing"

	"touchgocore/list"
	"touchgocore/util"
)

// newTestRing 造与生产同规格的桶环：按精度在 defaultWheelConfigs 里找档序，
// 桶数由同一张精度表推导，避免测试自造一套「只够测试用」的桶数而测不到真实形状。
func newTestRing(config int64) *slotRing {
	for i, c := range defaultWheelConfigs {
		if c == config {
			return newSlotRing(config, slotCountFor(defaultWheelConfigs, i))
		}
	}
	return newSlotRing(config, 64)
}

// putAt 造一个 nextTime 精确可控的定时器并挂进环里。
//
// 必须「先定时刻再入链」：入链时按 nextTime 归桶，先入链再改 nextTime 只会让节点
// 留在一个与它无关的桶里（生产路径不存在这个顺序，续期是先摘后挂）。
func putAt(t *testing.T, r *slotRing, nextTime int64) TimerInterface {
	t.Helper()
	tm, err := NewTimer[*plainTimer](1000, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	tm.GetParent().nextTime.Store(nextTime)
	if !r.Add(tm) {
		t.Fatal("Add 失败")
	}
	return tm
}

// nextTimeOf 取回调所见节点的时刻；非定时器节点返回 -1，便于断言「没看到别人」。
func nextTimeOf(node list.INode) int64 {
	timer, ok := node.(TimerInterface)
	if !ok {
		return -1
	}
	return timer.GetParent().nextTime.Load()
}

func TestSlotRingPlacesByAbsoluteStep(t *testing.T) {
	r := newTestRing(util.MILLISECONDS_OF_SECOND) // 秒轮：一格 1s，60 桶
	now := util.CurrentMS()
	step := now / util.MILLISECONDS_OF_SECOND

	for i := int64(1); i <= 5; i++ {
		putAt(t, r, now+i*util.MILLISECONDS_OF_SECOND)
	}
	if r.Length() != 5 {
		t.Fatalf("✘ 环内总数不符: %d", r.Length())
	}
	for i := int64(1); i <= 5; i++ {
		idx := int((step + i) % r.slotCount)
		if got := r.slots[idx].Length(); got != 1 {
			t.Fatalf("✘ 桶 %d 节点数不符: got=%d want=1", idx, got)
		}
	}
}

// TestSlotRingScanSkipsUnexpiredBuckets 是 S67 的收益本身：墙钟往前走一格只付那一格的钱。
//
// 窗口形状随档位不同，两条都要钉住：
//   - 毫秒轮（一格 == 一毫秒）只有「到期格」，没有预扫格 —— 整数毫秒下扫描不可能早于
//     到期，多扫一格是纯白走；
//   - 秒轮及以上是「到期格 + lookahead 预扫格」两格，第三格起一律不看（这一格是跨档
//     下沉的时机，见 RangeWindow 的推导）。
func TestSlotRingScanSkipsUnexpiredBuckets(t *testing.T) {
	now := util.CurrentMS()

	r := newTestRing(1) // 毫秒轮：1000 桶，一格 1ms
	r.cursor.Store(now)
	// 未来 1~900ms 各挂一个：只有 now+1 那一格落在本拍窗口里
	for offset := int64(1); offset <= 900; offset++ {
		putAt(t, r, now+offset)
	}

	var visited []int64
	r.RangeWindow(now+1, func(node list.INode) bool {
		visited = append(visited, nextTimeOf(node))
		return true
	})
	if len(visited) != 1 || visited[0] != now+1 {
		t.Fatalf("✘ 毫秒轮多扫了：本档没有跨档下沉可做，窗口只该覆盖到期格: %v", visited)
	}
	if got := r.cursor.Load(); got != now+1 {
		t.Fatalf("✘ 游标越过了本拍那一格: cursor=%d want=%d", got, now+1)
	}
	if got := r.Length(); got != 900 {
		t.Fatalf("✘ 扫描不该改动环内容: len=%d want=900", got)
	}

	// 同一毫秒内再驱动一次（target 仍等于游标）：不得退化成整环扫描。
	// 看到的仍只有「本格 + 钳制落点格」这两个桶——本格被重扫是有意为之：
	// 上一拍投递满而留在原地的节点，必须在墙钟没往前走时也拿到重试机会。
	visited = visited[:0]
	r.RangeWindow(now+1, func(node list.INode) bool {
		visited = append(visited, nextTimeOf(node))
		return true
	})
	if len(visited) > 2 {
		t.Fatalf("✘ 重复驱动扫掉了游标之外的桶: %d 个", len(visited))
	}

	// 秒轮：一格 1s，窗口 = 到期格 + 预扫格
	s := newTestRing(util.MILLISECONDS_OF_SECOND) // 60 桶
	sec := util.MILLISECONDS_OF_SECOND
	base := now / sec
	s.cursor.Store(base)
	for i := int64(1); i <= 40; i++ {
		putAt(t, s, (base+i)*sec+sec/2) // 各占一格，落在 base+1 .. base+40
	}
	visited = visited[:0]
	s.RangeWindow(base*sec+sec/2, func(node list.INode) bool {
		visited = append(visited, nextTimeOf(node))
		return true
	})
	// target == base == cursor → 走「墙钟没往前走」分支，覆盖 slot(base) 与 slot(base+1)：
	// 前者空、后者装着 base+1 那一格的那个节点
	if len(visited) != 1 {
		t.Fatalf("✘ 秒轮重复驱动时覆盖的桶数不符: %v", visited)
	}
	visited = visited[:0]
	s.RangeWindow((base+1)*sec+sec/2, func(node list.INode) bool {
		visited = append(visited, nextTimeOf(node))
		return true
	})
	// 这一拍 target = base+1 > cursor = base：窗口 (base, base+2] 两格，各一个节点
	if len(visited) != 2 {
		t.Fatalf("✘ 秒轮窗口不是「到期格 + 预扫格」两格: %v", visited)
	}
	if got := s.cursor.Load(); got != base+1 {
		t.Fatalf("✘ 游标应停在到期格而不是预扫格: cursor=%d want=%d", got, base+1)
	}
}

// TestSlotRingClampedDueNodeIsFired 回归「入链即到期」的钳制落点：
// nextTime 落在游标身后，节点被抬到 cursor+1，必须在紧接着的下一拍被看到，
// 而不是等墙钟真的推进过那一格（毫秒轮等一格就是一秒）。
func TestSlotRingClampedDueNodeIsFired(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	putAt(t, r, now-50)

	var fired []int64
	// 同一毫秒内驱动：target == cursor，走「墙钟没往前走」分支
	r.RangeWindow(now, func(node list.INode) bool {
		fired = append(fired, nextTimeOf(node))
		return true
	})
	if len(fired) != 1 || fired[0] != now-50 {
		t.Fatalf("✘ 钳制到 cursor+1 的到期节点没被扫到: %v", fired)
	}
}

// TestSlotRingLappedScansEverything 落后超过一圈（进程被冻结后恢复）时退回整环扫描。
func TestSlotRingLappedScansEverything(t *testing.T) {
	r := newTestRing(util.MILLISECONDS_OF_SECOND)
	now := util.CurrentMS()
	step := now / util.MILLISECONDS_OF_SECOND
	r.cursor.Store(step - 10*60) // 落后 10 分钟 > 60 桶

	for i := int64(0); i < 7; i++ {
		putAt(t, r, now+i)
	}
	var seen int
	r.RangeWindow(now, func(list.INode) bool { seen++; return true })
	if seen != 7 {
		t.Fatalf("✘ 落后超一圈未整环扫描: seen=%d want=7", seen)
	}
	if got := r.cursor.Load(); got != step {
		t.Fatalf("✘ 游标未推进到当前格: %d want=%d", got, step)
	}
}

// TestSlotRingClockRewindKeepsCursor 时钟回拨：不推进游标，也不把桶环搞坏。
func TestSlotRingClockRewindKeepsCursor(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	putAt(t, r, now+5)
	r.RangeWindow(now-1000, func(list.INode) bool { return true })
	if got := r.cursor.Load(); got != now {
		t.Fatalf("✘ 时钟回拨把游标倒退了: %d", got)
	}
	if r.Length() != 1 {
		t.Fatalf("✘ 回拨扫掉了节点: len=%d", r.Length())
	}
}

// TestSlotRingAddAcrossSlotsMovesNode 同一节点改期后重新入环：物理上换桶，
// 旧桶不留残链（List.Add 先 detach 再挂新链），Length 只算一次。
func TestSlotRingAddAcrossSlotsMovesNode(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	tm, err := NewTimer[*plainTimer](1000, InfiniteCount, nil)
	if err != nil {
		t.Fatal(err)
	}
	parent := tm.GetParent()
	parent.nextTime.Store(now + 5)
	if !r.Add(tm) {
		t.Fatal("Add 失败")
	}
	first := int((now + 5) % r.slotCount)
	if got := r.slots[first].Length(); got != 1 {
		t.Fatalf("✘ 首次入环位置不符: slot=%d len=%d", first, got)
	}
	parent.nextTime.Store(now + 700)
	if !r.Add(tm) {
		t.Fatal("二次 Add 失败")
	}
	second := int((now + 700) % r.slotCount)
	if r.slots[first].Length() != 0 || r.slots[second].Length() != 1 {
		t.Fatalf("✘ 改期后未真正换桶: slot%d=%d slot%d=%d", first, r.slots[first].Length(),
			second, r.slots[second].Length())
	}
	if got := r.Length(); got != 1 {
		t.Fatalf("✘ 迁移后总数被重复统计: %d", got)
	}
}

// TestSlotRingRemoveFromSlotKeepsRingConsistent 桶内摘链（业务 Remove 走的就是这条）
// 之后 Length 与整环扫描看到的数量仍一致。
func TestSlotRingRemoveFromSlotKeepsRingConsistent(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	nodes := make([]TimerInterface, 0, 10)
	for i := 0; i < 10; i++ {
		nodes = append(nodes, putAt(t, r, now+int64(i)+1))
	}
	for i := 0; i < 10; i += 2 {
		node, ok := nodes[i].(list.INode)
		if !ok {
			t.Fatal("✘ 定时器未实现 list.INode")
		}
		node.GetNode().Remove()
	}
	if got := r.Length(); got != 5 {
		t.Fatalf("✘ 隔位摘链后总数不符: %d want=5", got)
	}
	var seen int
	r.Range(func(list.INode) bool { seen++; return true })
	if seen != 5 {
		t.Fatalf("✘ 整环扫描看到的节点数不符: %d want=5", seen)
	}
	r.Clear()
	if r.Length() != 0 {
		t.Fatalf("✘ Clear 后仍有残留: %d", r.Length())
	}
}

// TestSlotRingNilAndNonTimerNodes 非定时器节点（无 nextTime 可依）必须落到
// 紧挨游标的下一格而不是消失，Add(nil) 返回 false。
func TestSlotRingNilAndNonTimerNodes(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	if r.Add(nil) {
		t.Fatal("✘ Add(nil) 不应成功")
	}
	raw := list.NewNode("payload", nil)
	if !r.Add(raw) {
		t.Fatal("✘ 非定时器节点入环失败")
	}
	if got := r.slots[int((now+1)%r.slotCount)].Length(); got != 1 {
		t.Fatalf("✘ 非定时器节点未落在 cursor+1 格: slot len=%d", got)
	}
}

// TestSlotRingCallbackAbort 回调返回 false 必须终止整个窗口扫描，
// 且游标不推进（宁可下一拍重扫，也不把没扫过的桶永久留在身后）。
func TestSlotRingCallbackAbort(t *testing.T) {
	r := newTestRing(1)
	now := util.CurrentMS()
	r.cursor.Store(now)
	for i := int64(1); i <= 3; i++ {
		putAt(t, r, now+i)
	}
	var seen int
	r.RangeWindow(now+3, func(list.INode) bool {
		seen++
		return false
	})
	if seen != 1 {
		t.Fatalf("✘ 回调返回 false 后仍在继续: seen=%d", seen)
	}
	if got := r.cursor.Load(); got != now {
		t.Fatalf("✘ 提前终止却推进了游标: cursor=%d want=%d", got, now)
	}
}

// TestSlotRingNodeVisitedAtOwnStep 跨圈节点：桶数小于滞留跨度时（小时轮 24 格、
// 节点在未来 10 天）节点会在自己那一格之前被提前看到若干次（每圈一次）。
// 分桶不错发的前提就在这里——提前看到只会走「原地留下、等下一圈」，
// 派发判定仍由回调按 nextTime 与墙钟复核，环本身绝不替节点做决定。
//
// S67 补上 lookahead 之后每圈被看到两拍：预扫格那一拍与到期格那一拍，间隔恒为
// {1, 23} 成对出现（24 格一圈），第 10 圈共 20 次。
func TestSlotRingNodeVisitedAtOwnStep(t *testing.T) {
	const hour = util.MILLISECONDS_OF_HOUR
	r := newSlotRing(hour, 24) // 刻意用小于跨度的桶数，逼出「一圈被复核一次」
	now := util.CurrentMS()
	step := now / hour
	r.cursor.Store(step)

	next := now + 240*hour
	putAt(t, r, next)
	nodeStep := next / hour

	var visits []int64
	for s := step + 1; s <= nodeStep+1; s++ {
		r.RangeWindow(s*hour, func(list.INode) bool {
			visits = append(visits, s)
			return true
		})
	}
	if len(visits) == 0 {
		t.Fatal("✘ 未来 10 天的节点一次都没被看到，说明归桶算错了")
	}
	if last := visits[len(visits)-1]; last != nodeStep {
		t.Fatalf("✘ 最后一次复核不在自己那一格: last=%d nodeStep=%d", last, nodeStep)
	}
	for i := 1; i < len(visits); i++ {
		if d := visits[i] - visits[i-1]; d != 1 && d != 23 {
			t.Fatalf("✘ 复核间隔不是「每圈两拍」: %d -> %d（差 %d）", visits[i-1], visits[i], d)
		}
	}
	if want := int((nodeStep-step)/24) * 2; len(visits) != want {
		t.Fatalf("✘ 每圈两拍合计不符: got=%d want=%d", len(visits), want)
	}
	if r.Length() != 1 {
		t.Fatalf("✘ 节点在等待期间丢失: len=%d", r.Length())
	}
}

// TestSlotRingResidueBucketIsRescanned 回归（S67）：投递通道满而留在原地的节点，
// 下一拍必须重新被扫到。
//
// 变更前每拍全扫，这条性质天然成立；桶化后若游标径直推进到本拍那一格，节点所在的
// 桶要等游标绕完一整圈才被重访（秒档 60s、分档 10min），等于把「背压时下个 tick
// 重试」偷偷改成「下圈重试」——实测会让反复跨档的定时器整批停摆。
// 残留由 commitDispatch 经 markResidue 上报，环只负责把游标退回残留段之前。
func TestSlotRingResidueBucketIsRescanned(t *testing.T) {
	const sec = util.MILLISECONDS_OF_SECOND
	r := newTestRing(sec)
	base := util.CurrentMS() / sec
	r.cursor.Store(base)
	putAt(t, r, (base+1)*sec+sec/2) // 属于 base+1 这一格

	scan := func(at int64, residue bool) int {
		seen := 0
		r.RangeWindow(at*sec+sec/2, func(list.INode) bool {
			seen++
			if residue {
				r.markResidue()
			}
			return true
		})
		return seen
	}

	if got := scan(base+1, true); got != 1 {
		t.Fatalf("✘ 第一拍没看到到期桶里的节点: seen=%d", got)
	}
	if got := r.cursor.Load(); got != base {
		t.Fatalf("✘ 有残留时游标仍推进了: cursor=%d want=%d", got, base)
	}
	// 第二拍：残留桶重新覆盖；这一拍通道空了，不再上报残留
	if got := scan(base+2, false); got != 1 {
		t.Fatalf("✘ 残留桶没有在下一拍被重扫（退化成整圈延迟）: seen=%d", got)
	}
	if got := r.cursor.Load(); got != base+2 {
		t.Fatalf("✘ 无残留时游标应推进到本拍那一格: cursor=%d want=%d", got, base+2)
	}
	// 第三拍起不再重复扫过去的那几格：回退只补偿残留，不变成每拍全扫
	if got := scan(base+3, false); got != 0 {
		t.Fatalf("✘ 已派发完的桶被无谓重扫: seen=%d", got)
	}
}

// TestSlotRingResidueKeepsFullScanWhileLapped 落后超过一圈时本就已整环扫描，
// 残留期间游标必须停在「下一拍仍整环」的位置，绝不能推进到只剩一段窗口。
func TestSlotRingResidueKeepsFullScanWhileLapped(t *testing.T) {
	const sec = util.MILLISECONDS_OF_SECOND
	r := newTestRing(sec)
	base := util.CurrentMS() / sec
	r.cursor.Store(base - 2*r.slotCount) // 人为落后两圈
	putAt(t, r, (base-1)*sec+sec/2)      // 身后一格：只有整环扫描才看得到

	seen := 0
	r.RangeWindow(base*sec+sec/2, func(list.INode) bool { seen++; r.markResidue(); return true })
	if seen != 1 {
		t.Fatalf("✘ 落后超过一圈时没走整环扫描: seen=%d", seen)
	}
	if got, want := r.cursor.Load(), base-r.slotCount; got != want {
		t.Fatalf("✘ 残留期间游标未留在整环区间: cursor=%d want=%d", got, want)
	}
	// 下一拍仍在整环区间内，身后的节点第二次被看到
	seen = 0
	r.RangeWindow((base+1)*sec+sec/2, func(list.INode) bool { seen++; return true })
	if seen != 1 {
		t.Fatalf("✘ 残留后下一拍丢了身后的节点: seen=%d", seen)
	}
}
