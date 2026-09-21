package localtimer

import (
	"sync/atomic"

	"touchgocore/list"
	"touchgocore/util"
	"touchgocore/vars"
)

// ============================================================================
// 桶化时间轮（S67）。
//
// 变更前每档轮是一条按入链顺序排列的链表，processWheelTick 每 tick 从头扫到尾，
// 单次 tick 代价是 O(轮内在链数) 而与「这一瞬真正到期几个」无关：亚秒定时器越多，
// 每拍越慢，能消化的到期数越少。基线实测（变更前代码配同一套 harness，小时轮纯扫描
// 1k/10k/50k）为 25.75 / 28.15 / 40.33 ns/timer，即单次 tick 25.7µs / 281µs /
// 2.02ms；1 万条亚秒混合负载 315µs/tick —— 5 万在链已经超掉毫秒轮 1ms 的节拍。
//
// 现在每档轮内部按 slot = floor(nextTime / wheelConfig) % slotCount 分成多条子链，
// 一次 tick 只看「墙钟走过的那几个绝对步数」对应的桶，再加 lookahead 一格用于跨档
// 下沉（见 RangeWindow），代价降为 O(到期桶 + 预扫桶内的节点数)。
// 游标以「绝对步数」而非 0..N-1 的下标保存，于是「绕了多少圈」这件事不需要额外计数。
//
// 这里只桶化扫描，不动任何协议：timerCount、migratingMarker 认领、commitDispatch、
// unlinkStale、每轮一把 wheelLock 全部原样保留（阶段12 S64/S66 的用例与不变式继续成立）。
//
// 新增一条契约（桶化换来的唯一约束）：**节点在链期间 nextTime 不得再改**。
// 桶位在入链那一刻按 nextTime 算定，之后改时刻等于把节点留在一个与它无关的桶里，
// 最坏要等游标绕完一整圈才被重访（秒档 60s、分档 10min）。生产路径本就满足：
// Init 在未挂链时写，续期是先摘链再写再重挂（executeTimer），Pause/复活同理。
// 需要改时刻的调用方必须走 AddTimer 重挂——它先 removeFromManagerLocked 摘干净、
// 再按当前 nextTime 归桶，跨档迁移照旧发生。
//
// 例外由残留机制兜住：投递通道满时节点留在原桶，markResidue 让游标退回这一格，
// 下一拍重试，不必等一圈（S64 的背压语义原样保留）。
// ============================================================================

// slotRing 一档时间轮内部的桶环。
//
// 每条子链自带读写锁，语义与原先那一条大链表完全一致：Add 在子链写锁里挂节点，
// 扫描走子链自己的无锁快照（List.Range），摘链仍由节点自己的 Remove 完成。
// 因此调用方（handleTimerAdd / commitDispatch / drainWheelLocked）的加锁顺序不变。
type slotRing struct {
	wheelConfig int64        // 一格代表多少毫秒
	slotCount   int64        // 桶数
	slots       []*list.List // 长度 = slotCount，构造后不再增删
	cursor      atomic.Int64 // 已扫描过的最大绝对步数 = floor(now / wheelConfig)

	// residue 只由本档轮的消费协程读写（runWheel 每档一条），不参与跨协程通信，
	// 其效果最终通过 cursor 这一个原子量体现，故不必原子。
	residue bool // 本桶扫描中有节点被留在原地（投递通道满或回调 panic）
}

// newSlotRing 建一环。slotCount 必须 > 0。
func newSlotRing(wheelConfig int64, slotCount int) *slotRing {
	if wheelConfig <= 0 {
		wheelConfig = 1
	}
	if slotCount <= 0 {
		slotCount = 1
	}
	r := &slotRing{wheelConfig: wheelConfig, slotCount: int64(slotCount)}
	r.slots = make([]*list.List, slotCount)
	for i := range r.slots {
		r.slots[i] = list.NewUnindexedList()
	}
	// 游标起于「此刻」：冷启动时 nextTime 是绝对毫秒戳，若留在 0，第一拍会算出
	// 「落后几十万圈」而走整环扫描分支；空轮那一下不痛，但归桶钳制也会跟着失真。
	r.cursor.Store(util.CurrentMS() / wheelConfig)
	return r
}

// slotOfStep 绝对步数到物理桶下标。
func (r *slotRing) slotOfStep(step int64) int {
	idx := step % r.slotCount
	if idx < 0 {
		idx += r.slotCount // 时钟回拨出负步数时仍要落在合法桶上
	}
	return int(idx)
}

// slotFor 取节点应落在的绝对步数（= floor(nextTime / wheelConfig)，与游标同一量纲，
// 不是 0..slotCount-1 的物理下标）；已落后于游标的（入链即到期、或墙钟追过了它的轮）
// 一律抬到 cursor+1，让它下一次扫描就被看到。
//
// 抬到下一格而不是就地放进「本轮已扫过的桶」是必须的：物理桶一圈才被重访一次，
// 压在身后的桶要等整整一圈（毫秒轮即 1 秒）才被发现，等价于把定时器拖死在这一档。
func (r *slotRing) slotFor(node list.INode) int64 {
	cursor := r.cursor.Load()
	if step, ok := stepOf(node, r.wheelConfig); ok && step > cursor {
		return step
	}
	return cursor + 1
}

// stepOf 读节点自己的到期步数。取不到（不是定时器、无父项、时刻非正、或实现方
// 在 GetParent 里直接 panic）时返回 ok=false，由调用方按 cursor+1 归桶：
// 入链本身绝不因此失败，异常节点下一拍照常被扫到，届时仍走原有判定分支处置。
// 不把异常抛给 Add 的 recover，是为了保住「入链失败」这条通道只表示链表本身出错。
func stepOf(node list.INode, wheelConfig int64) (step int64, ok bool) {
	defer func() {
		if recover() != nil {
			step, ok = 0, false
		}
	}()
	timer, isTimer := node.(TimerInterface)
	if !isTimer {
		return 0, false
	}
	parent := timer.GetParent()
	if parent == nil {
		return 0, false
	}
	next := parent.nextTime.Load()
	if next <= 0 {
		return 0, false
	}
	return next / wheelConfig, true
}

// Add 按 nextTime 归桶入链。失败语义与 List.Add 一致：panic 被 recover 后返回 false，
// 调用方据此回滚归属，且绝不留下「计数加了而节点不在环上」的中间态。
func (r *slotRing) Add(node list.INode) (bret bool) {
	defer func() {
		if err := recover(); err != nil {
			bret = false
		}
	}()
	if node == nil {
		return false
	}
	return r.slots[r.slotOfStep(r.slotFor(node))].Add(node)
}

// markResidue 记下「本次扫描有节点因为目标通道满而留在原地」。
//
// 桶化后「留在原地」的含义变了：变更前每拍全扫，它下一拍必然再被看到；现在它所在
// 的那个桶要等游标绕完一整圈才被重访（秒档 60s、分档 10min），等于把 S64 的
// 「背压时下个 tick 重试」悄悄改成了「下圈重试」。于是 RangeWindow 收尾时把游标
// 退回本段第一个残留桶之前，下一拍重新覆盖这一段。
//
// 调用约束：只允许出现在 slotRing.scanSlot 的调用链里（即 RangeWindow 驱动的那次扫描）。
// 别处都不该调 —— 收尾路径（drainWheelLocked/cleanupWheel）不投递、业务 Remove 与
// 测试直调都没有「本拍没送走」这层含义；残留的唯一来源就是扫描中被留下的节点。
// 不加锁同理：状态只在本档轮消费协程（runWheel 每档一条）上读写，
// 对外只通过 cursor 这一个原子量生效。
func (r *slotRing) markResidue() {
	r.residue = true
}

// Length 环内在链总数。逐桶求和（桶数最大 1000），不另设计数器：
// 节点在桶间物理迁移由 List.Add 的 detach 完成，外挂计数会跟丢，
// 而本方法只在测试与诊断路径上被调用，不在每拍热路径里。
func (r *slotRing) Length() int {
	total := 0
	for _, slot := range r.slots {
		total += slot.Length()
	}
	return total
}

// Clear 清空整环。与 List.Clear 同样的遍历期保护由子链自己负责。
func (r *slotRing) Clear() {
	for _, slot := range r.slots {
		slot.Clear()
	}
}

// Range 全环扫描。只在「收尾清空」与「落后超过一圈」两种场合使用，
// 正常 tick 走 RangeWindow，代价才与到期桶数量挂钩。
func (r *slotRing) Range(f func(list.INode) bool) {
	for _, slot := range r.slots {
		if !rangeSlot(slot, f) {
			return
		}
	}
}

// RangeWindow 扫描「游标之后到 currentTime 的下一格」的桶，并把游标推进到 currentTime
// 所属那一格（提前看过的那一格不计入游标，下一拍会作为到期格正式重访）。
//
// 这是 S67 的收益来源：一次 tick 只付「到期桶 + 前一格的预扫」的钱，而不是整轮。
// 三种边界：
//   - target <= cursor：墙钟没往前走（同一毫秒内被重复驱动、或时钟回拨）。本格与钳制
//     落点都要扫，否则新落进这一格的到期项要等墙钟推进才被发现；
//   - target - cursor >= slotCount：落后超过一圈（比如进程被冻结后恢复）。逐格扫已
//     无意义，整环扫一遍，代价退回 O(在链数) 但不漏派发；
//   - 其余：逐格扫 (cursor, target+1]。
//
// 为什么必须多扫「下一格」（lookahead，实测必需而非锦上添花）：一档轮的 ticker 周期
// 等于该档精度，于是每个桶一拍只被看一次，落在 W = 格起点 + δ（δ 为该协程的固定节拍
// 相位）这一刻。节点到期时刻 T 落在格 B 内的偏移 r 与 δ 素不相识：δ >= r 时这一拍看到
// 它已到期，直接从粗档派发出去，晚点 δ-r 最大接近一整格（秒档 1s、分档 60s、10 分档
// 600s）。变更前每拍全扫，节点在被划入本档的那一刻起每拍都被复查，remaining 一跌破
// 本档下界就下沉到更精确的一档，最终由毫秒轮按 1ms 兑现 —— 实测变更后 40 个 1500ms
// 周期定时器每次派发都稳定晚 500ms 且 wheelMigrations 恒为 0，是纯粹的精度倒退。
// 多扫一格把每个节点的被访次数恢复成两次：早拍（W' = (B-1)格起点 + δ）与到期拍。
// 早拍时 remaining = r + cfg - δ，到期拍时 remaining = r - δ；两者恰好在 δ 的两侧互补，
// 必有其一落在 [0, cfg) 区间内，于是节点总能在到期前下沉到更精确的一档，而不是被粗档
// 直接派发。早拍绝不会再早：到期判定仍是 nextTime <= currentTime，未到期且仍属本档的
// 节点原样留下，下一拍作为到期格正式重访。代价是每拍固定多扫一格。
//
// 毫秒轮（cfg == 1）不带这个代价：nextTime 与墙钟同为整数毫秒，一格的宽度就是一毫秒，
// 扫描永远不可能早于到期，预扫那一格只是白走一遍 —— 见下面 last 的推导。
//
// 游标在扫完之后才推进：中途 panic 时下一次仍会重扫这一段，宁可重复派发一次
// （认领协议会把重复派发判给 dispatchSkipped）也不能把整段桶永久留在身后。
// 扫描中留下残留（通道满）时同样回退游标，见 markResidue。
//
// 回退幅度恒不超过一圈：窗口分支退回「本段第一个残留桶之前」，整环分支退回
// target-slotCount（下一拍仍满足整环条件，保持变更前的全扫节奏）。两条都以本拍的
// target 为基准，因此时钟回拨后游标只会随墙钟一起往前走，不会退到几十年前反复重扫。
//
// 游标只由本档轮的消费协程推进（runWheel 每档一条），handleTimerAdd 只读它做归桶
// 钳制，因此这里不额外加锁；钳制读到的是「刚刚扫过」而非「正要扫」的游标，
// 至多让一个节点提前一格被看到，不会让它等上一整圈。
func (r *slotRing) RangeWindow(currentTime int64, f func(list.INode) bool) {
	if r.wheelConfig <= 0 {
		r.Range(f)
		return
	}
	r.residue = false
	target := currentTime / r.wheelConfig
	cur := r.cursor.Load()
	if target <= cur {
		// 墙钟没往前走：除了本格，还要扫「归桶钳制」的落点与 lookahead 的落点，
		// 即 cur+1 那一格。少了这一格，入链即到期（nextTime <= 此刻）而被钳到身后
		// 一格的节点要等墙钟推进才可能被发现；而 cur == target 时这一格正是常态
		// 分支里的那一格预扫，两条分支的口径保持一致。
		// 本格自己也重扫一遍是有意保留的：上一拍因投递通道满而留在原地的节点，
		// 不等墙钟推进也该拿到重试机会——这正是变更前「每拍全扫」唯一有价值的部分。
		// 这一支不回退游标（游标本拍就没动，下一拍仍覆盖同一对桶），依据的是那条
		// 未成文不变式：在链节点的应落桶步数恒 > 游标（slotFor 一律钳到 cursor+1），
		// 所以这里能留下残留的只有 slot(cur+1)，而它正是下一拍要扫的那一格。
		if !r.scanSlot(r.slots[r.slotOfStep(target)], f) {
			return
		}
		r.scanSlot(r.slots[r.slotOfStep(cur+1)], f)
		return
	}
	if target-cur >= r.slotCount {
		// 落后超过一圈：逐格追已无意义，整环扫一遍（lookahead 那格也在这一次里覆盖了）。
		// 此时若有残留，游标退回「再落后一圈」的位置，下一拍仍满足整环条件——背压没
		// 消化完就保持变更前的全扫节奏，绝不把留在身后任意一格里的节点推到一整圈之后。
		for _, slot := range r.slots {
			if !r.scanSlot(slot, f) {
				return
			}
		}
		if r.residue {
			r.cursor.Store(target - r.slotCount)
			return
		}
		r.cursor.Store(target)
		return
	}
	rewindTo, rewound := int64(0), false // 本段第一个留下残留的桶步数（0 不可作哨兵：步数合法值含 0）
	// 预扫那一格只对「一格宽于一毫秒」的档有意义：nextTime 与墙钟都是整数毫秒，
	// 毫秒轮里 target == 节点自己的步数，扫描发生的瞬间必有 now >= step == 那一毫秒，
	// 也就是不可能「早于到期看到」——那半格永远只会白走一遍。粗档（1s/60s/600s）
	// 才存在「同一格内早于到期被扫到」的窗口，而那正是跨档下沉要抓的时机。
	last := target
	if r.wheelConfig > 1 {
		last = target + 1
	}
	for step := cur + 1; step <= last; step++ {
		if !r.scanSlot(r.slots[r.slotOfStep(step)], f) {
			return
		}
		if r.residue {
			r.residue = false
			if !rewound {
				rewound, rewindTo = true, step
			}
		}
	}
	// 游标只推进到 target，而不是本段实际扫到的 target+1：预扫过的那一格下一拍要作为
	// 到期格正式重访（早拍里 remaining 还大于本档下界、原样留下的节点，全靠这一次
	// 落到更精确的一档）。残留回退同理以本格为上限，保证留下残留的那格必被重扫。
	next := target
	if rewound && rewindTo-1 < next {
		next = rewindTo - 1
	}
	r.cursor.Store(next)
}

// scanSlot 扫描一个桶，返回「是否继续扫后面的桶」。
//
// 比裸 rangeSlot 多做两件事：
//
//  1. 空桶快路径。落后超过一圈（进程被 GC/挂起恢复）时要逐个过 slotCount 个桶，
//     毫秒轮就是 1000 次读锁加一次快照取还，即使全空也要几十到上百微秒；
//     先比一次长度，空桶直接跳过。非空桶多付一次 Length（一次读锁），可忽略。
//
//  2. 回调 panic 隔离。业务实现的 TimerInterface.GetParent 若炸，panic 绝不能抛出
//     RangeWindow：游标在扫描之后才推进，于是它永远停在原地，这一档轮每个 tick 都在
//     同一个节点上重炸——整档轮永久停摆（变更前一次 panic 只毁掉当轮扫描）。
//     这里就地收下并按残留处理：游标退回本桶之前，本桶剩余节点与其余各桶下一拍重试，
//     其余定时器照常派发。日志只能留在这里——扫描不在任何锁内（commitDispatch
//     自带 defer 释放），而 vars 异步缓冲满时最长阻塞 5 秒，持锁打日志会拖死整轮。
func (r *slotRing) scanSlot(slot *list.List, f func(list.INode) bool) (cont bool) {
	if slot.Length() == 0 {
		return true
	}
	defer func() {
		if err := recover(); err != nil {
			r.markResidue()
			vars.Error("时间轮桶扫描回调发生panic，本桶剩余节点推迟到下一拍重试: %v", err)
			cont = true
		}
	}()
	return rangeSlot(slot, f)
}

// rangeSlot 包一层，让回调返回 false 能终止整个窗口/全环扫描。
func rangeSlot(slot *list.List, f func(list.INode) bool) bool {
	stop := false
	slot.Range(func(node list.INode) bool {
		if !f(node) {
			stop = true
			return false
		}
		return true
	})
	return !stop
}
