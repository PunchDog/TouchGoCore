package localtimer

// ============================================================================
// releaseOutcome 仲裁的显式状态转移表
//
// timer.go 的 beginTick / endTick / requestReleaseLocked / removeFromManagerLocked
// 用 7 个原子标志共同决定「实例该不该、能不能归还对象池」。本文件把这些散在注释里的
// 不变量落成数据：一行 = 一种标志组合 → 允许的动作 → 目标 releaseOutcome。
//
// 表是声明式的：仲裁代码仍按原顺序执行，7 个标志的语义与判定顺序一字未改；
// 表用于两件事——让状态空间可读可评审，以及 debug 构建下的不变量断言
// （timer_assert_debug.go）。生产构建（无 debug tag）里两个断言钩子都是空操作，
// 零开销、零行为变化。
//
// 7 个标志：inPool / released / fromPool / inTick / pendingRelease /
// putClaimed / poolEpoch。其中 poolEpoch 区分 sync.Pool 复用同一地址后的前后任
// 主人，不参与 outcome 判定，但随快照留存以便排障。
// ============================================================================

// releaseState 是一次仲裁开始瞬间的 7 标志快照。
type releaseState struct {
	inPool         bool
	released       bool
	fromPool       bool
	inTick         int32
	pendingRelease bool
	putClaimed     bool
	poolEpoch      uint64
}

// snapshotReleaseState 读取 7 个原子标志，供状态表 / debug 断言使用。
//
// 这是同一把 t.mu 下的近似快照：inTick 由 beginTick/endTick 在锁外原子修改，
// 因此 debug 断言只校验「不可能发生」的组合，不把快照当成精确的并发预测。
func (t *Timer) snapshotReleaseState() releaseState {
	return releaseState{
		inPool:         t.inPool.Load(),
		released:       t.released.Load(),
		fromPool:       t.fromPool.Load(),
		inTick:         t.inTick.Load(),
		pendingRelease: t.pendingRelease.Load(),
		putClaimed:     t.putClaimed.Load(),
		poolEpoch:      t.poolEpoch.Load(),
	}
}

// releaseAction 是仲裁允许执行的动作，与 releaseOutcome 一一对应。
type releaseAction int8

const (
	actionStopOnly  releaseAction = iota // 只停调度 / 只减在飞计数，不归还
	actionPutNow                         // 解锁后立即 Put
	actionDeferPut                       // 置 pendingRelease，endTick 补做
	actionNoop                           // 幂等丢弃（重复作废 / 已抢占）
	actionRejectPut                      // 无池发放凭证，禁止入池
)

// releasePhase 标明一行转移来自哪个函数，避免把不同阶段的同名条件混为一谈。
type releasePhase uint8

const (
	phaseArbitration releasePhase = iota // requestReleaseLocked 的判定（决定 outcome）
	phaseTickBegin                       // beginTick
	phaseTickEnd                         // endTick
	phaseRemove                          // removeFromManagerLocked
)

// releaseTransition 是一行显式状态转移：guard 命中即得到动作 / outcome / 目标状态。
type releaseTransition struct {
	phase   releasePhase
	name    string
	guard   func(releaseState) bool
	action  releaseAction
	outcome releaseOutcome
	to      string
	note    string
}

// releaseTransitions 按阶段分组；phaseArbitration 段的顺序即
// requestReleaseLocked 的判定优先级，expectedReleaseOutcome 依此推导预测值。
var releaseTransitions = []releaseTransition{
	// ---- requestReleaseLocked：5 个互斥分支，顺序不可调换 ----
	{
		phase:   phaseArbitration,
		name:    "arbitration/no-pool-voucher",
		guard:   func(s releaseState) bool { return !s.fromPool },
		action:  actionRejectPut,
		outcome: releaseNotPooled,
		to:      "保持业务持有，永不入池",
		note:    "手工构造的宿主对象连「池」都不该参与；Put 里的同名闸门只是兜底，并累计 PutRejected",
	},
	{
		phase:   phaseArbitration,
		name:    "arbitration/already-in-pool",
		guard:   func(s releaseState) bool { return s.inPool },
		action:  actionNoop,
		outcome: releaseAlready,
		to:      "原地不动（已在池中）",
		note:    "归还早已完成，迟到的作废请求一律幂等丢弃；摘链会把新主人的节点无锁扯走",
	},
	{
		phase:   phaseArbitration,
		name:    "arbitration/tick-in-flight",
		guard:   func(s releaseState) bool { return s.inTick > 0 },
		action:  actionDeferPut,
		outcome: releaseDeferred,
		to:      "置 pendingRelease，待 endTick 收尾",
		note:    "此刻 Put 等于把正在跑回调的对象交给新主人；pendingRelease 粘到池重新发放为止",
	},
	{
		phase:   phaseArbitration,
		name:    "arbitration/put-already-claimed",
		guard:   func(s releaseState) bool { return s.putClaimed },
		action:  actionNoop,
		outcome: releaseAlready,
		to:      "原地不动（另一方已抢占本次归还）",
		note:    "putClaimed 抢占保证 endTick 补做与业务 Remove 不会各 Put 一次，Puts 统计不被污染",
	},
	{
		phase:   phaseArbitration,
		name:    "arbitration/release-now",
		guard:   func(releaseState) bool { return true },
		action:  actionPutNow,
		outcome: releaseNow,
		to:      "由调用方在释放 t.mu 之后执行 Put",
		note:    "CAS 抢占 putClaimed 成功的唯一路径；Put 不得持锁，否则新主人的初始化排在旧主人的锁上",
	},

	// ---- removeFromManagerLocked：先判 inPool，再按 cleanPool 分流 ----
	{
		phase:   phaseRemove,
		name:    "remove/in-pool-pause",
		guard:   func(s releaseState) bool { return s.inPool },
		action:  actionStopOnly,
		outcome: releaseNone,
		to:      "整体忽略，不摘链不归还",
		note:    "cleanPool=false：陈旧主人的 Pause 一律忽略，避免把池 / 新主人的节点扯走",
	},
	{
		phase:   phaseRemove,
		name:    "remove/in-pool-clean",
		guard:   func(s releaseState) bool { return s.inPool },
		action:  actionNoop,
		outcome: releaseAlready,
		to:      "原地不动（重复作废）",
		note:    "cleanPool=true：归还早已完成，幂等 no-op",
	},
	{
		phase:   phaseRemove,
		name:    "remove/active-to-inactive",
		guard:   func(s releaseState) bool { return true },
		action:  actionStopOnly,
		outcome: releaseNone,
		to:      "CAS isActive true→false，推进 gen 并摘链",
		note:    "只有这一次转换允许 nextGen / 扣计数；重复执行会把 timerCount 打成负数",
	},
	{
		phase:   phaseRemove,
		name:    "remove/pause-only",
		guard:   func(s releaseState) bool { return true },
		action:  actionStopOnly,
		outcome: releaseNone,
		to:      "停调度，不归还",
		note:    "cleanPool=false：Pause 语义，业务随后仍可 Remove 正式作废",
	},
	{
		phase:   phaseRemove,
		name:    "remove/clean-delegates-arbitration",
		guard:   func(s releaseState) bool { return true },
		action:  actionPutNow,
		outcome: releaseNone,
		to:      "交给 requestReleaseLocked 决定 releaseNow/Deferred/Already/NotPooled",
		note:    "cleanPool=true：本表 outcome 记为 releaseNone，仅表示此处不自行产出结论",
	},

	// ---- beginTick：代次 / 作废标记决定这次调度项是否作废 ----
	{
		phase:   phaseTickBegin,
		name:    "tick-begin/gen-mismatch-with-pending",
		guard:   func(s releaseState) bool { return s.pendingRelease && !s.released },
		action:  actionDeferPut,
		outcome: releaseDeferred,
		to:      "走 endTick 补做挂起的归还",
		note:    "实例已易主，但窗口内业务 Remove 已把归还挂起；裸减计数会让 pendingRelease 永久悬挂",
	},
	{
		phase:   phaseTickBegin,
		name:    "tick-begin/gen-mismatch-plain",
		guard:   func(s releaseState) bool { return s.released || !s.pendingRelease },
		action:  actionStopOnly,
		outcome: releaseNone,
		to:      "裸减 inTick，放弃回调",
		note:    "released 为真说明 Put 已完成、池中待认领，不介入；未挂起时维持既往裸减语义",
	},
	{
		phase:   phaseTickBegin,
		name:    "tick-begin/abandoned",
		guard:   func(s releaseState) bool { return s.pendingRelease || s.released || s.inPool },
		action:  actionDeferPut,
		outcome: releaseDeferred,
		to:      "走正常 endTick，让挂起的归还请求落地",
		note:    "代次没动却已进入作废流程（abandoned / inPool）",
	},

	// ---- endTick：在飞计数归零后才处理挂起的归还 ----
	{
		phase:   phaseTickEnd,
		name:    "tick-end/still-in-flight",
		guard:   func(s releaseState) bool { return s.inTick > 0 },
		action:  actionNoop,
		outcome: releaseNone,
		to:      "直接返回，不取锁",
		note:    "inTick.Add(-1) 仍 >0：还有并发回调在飞，收尾交给最后一次 endTick",
	},
	{
		phase:   phaseTickEnd,
		name:    "tick-end/no-pending",
		guard:   func(s releaseState) bool { return !s.pendingRelease },
		action:  actionNoop,
		outcome: releaseNone,
		to:      "快路径返回，不取锁",
		note:    "无挂起请求；取锁前快路径读取与临界区之间可能穿插 TimerPool.Get 的认领",
	},
	{
		phase:   phaseTickEnd,
		name:    "tick-end/pending-drained",
		guard:   func(s releaseState) bool { return s.pendingRelease },
		action:  actionPutNow,
		outcome: releaseDeferred,
		to:      "锁内复核 pendingRelease 后交给 requestReleaseLocked",
		note:    "复核失败说明已被认领方清掉，必须放弃，否则会把新主人正持有的实例二次 Put",
	},
}

// expectedReleaseOutcome 按仲裁段表格推导一次仲裁应有的结论。
//
// 只被 debug 断言使用；它与 requestReleaseLocked 的判定顺序逐行对应，
// 生产构建里仲裁代码并不查表，因此不存在改变行为的风险。
func expectedReleaseOutcome(s releaseState) releaseOutcome {
	for _, tr := range releaseTransitions {
		if tr.phase != phaseArbitration || tr.guard == nil {
			continue
		}
		if tr.guard(s) {
			return tr.outcome
		}
	}
	return releaseNone
}

// releaseActionFor 返回 outcome 对应的动作；未知 outcome 返回 false。
func releaseActionFor(o releaseOutcome) (releaseAction, bool) {
	switch o {
	case releaseNone:
		return actionStopOnly, true
	case releaseNow:
		return actionPutNow, true
	case releaseDeferred:
		return actionDeferPut, true
	case releaseAlready:
		return actionNoop, true
	case releaseNotPooled:
		return actionRejectPut, true
	default:
		return actionStopOnly, false
	}
}

// assertReleaseArbitration 由 debug 构建注入运行时不变量检查；生产构建为空操作。
// 表自洽性检查是确定性的，由 debug 构建的 init 直接调用，不经过本钩子。
var assertReleaseArbitration = func(releaseState, releaseOutcome) {}
