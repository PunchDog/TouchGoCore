package localtimer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/list"
	"touchgocore/util"
	"touchgocore/vars"
)

// TimerWheel 表示时间轮结构
type TimerWheel struct {
	wheelConfig  int64          // 时间轮精度（毫秒）
	tickWheel    *slotRing      // 定时器桶环（S67：一 tick 只扫到期桶）
	wheelLock    sync.RWMutex   // 时间轮锁（读写优化）
	addTimerChan chan timerTask // 定时器添加通道
	isRunning    atomic.Bool    // 是否运行
	timerCount   atomic.Int64   // 定时器计数（用于监控）

	// mgr 反向归属：摘链发生在 Timer 一侧（RemoveFromManager），那里只有 wheel 指针，
	// 没有它便无法把「移除」计入所属管理器的统计。
	mgr *TimerManager

	// lastBackpressureWarn 上次背压告警时间（毫秒）。按轮限频：一条全局限频线会让
	// 繁忙的毫秒轮把安静的小时轮的告警一并吞掉。
	lastBackpressureWarn atomic.Int64
}

// executeTimer 执行定时器并在需要时重新调度。
//
// 调用前提：不得持有任何 wheelLock。
// Tick 内部允许调用 Remove / Pause / AddTimer（业务最常见的写法），
// 因此本函数必须在完全无锁的上下文中运行，否则会自锁死锁。
func (m *TimerManager) executeTimer(task timerTask) {
	if !task.isValid() {
		return // 已被移除或被复用的过期调度项，直接丢弃
	}
	parent := task.timer.GetParent()

	// 在飞标记：Tick 执行期间禁止把实例归还对象池，否则新主人会和这个回调同时
	// 写同一块内存。beginTick 内部会复核代次与作废状态，校验通过后到真正打上
	// 在飞标记之间不存在可被池认领的窗口；失败说明实例已易主，直接放弃本次回调。
	// endTick 必须是本函数最后一个动作——它返回后实例随时已易主，
	// 再读一次 task.timer 就是踩别人的对象。
	if !parent.beginTick(task.gen) {
		return
	}
	defer parent.endTick()

	func() {
		defer func() {
			if err := recover(); err != nil {
				vars.Error("定时器执行发生panic: %v", err)
			}
		}()
		task.timer.Tick()
	}()
	m.stats.timersExecuted.Add(1)

	// Tick 内部可能已经移除/复用该定时器，或系统已关闭，重新校验后再续期
	if m.isClosed.Load() || !task.isValid() {
		return
	}
	if !task.timer.HasNext() {
		// HasNext 为 false 有两种成因，必须用活跃标记区分：
		//   1) 有限次数自然耗尽——定时器仍是活跃的，本条就是它的最后一次执行，
		//      就此作废并归还对象池，否则实例永久滞留在无人管理的状态；
		//   2) 业务在 Tick 里自己 Pause()/Remove() 了——收尾已由那两次调用做完，
		//      这里再动手会把「只是暂停」的定时器送进池，把所有权交出去。
		if parent.IsActive() {
			m.retireAfterExhaustion(parent)
		}
		return
	}
	if !task.isValid() {
		return
	}
	if err := m.addTimerWithRetry(task.timer, task.gen); err != nil {
		if errors.Is(err, ErrTimerCanceled) {
			return // 续期窗口内业务已 Pause/Remove 或已自行重新注册：本次作废，不算失败
		}
		m.stats.timersRescheduleFailed.Add(1)
		m.notifyTimerLost(task.timer, parent, err)
	}
}

// defaultWheelConfigs 每档轮的精度（毫秒）：毫秒/秒/分钟/10分钟/小时。
// 只读约定：桶数、档序断言与测试都按这张表推导，改表即改协议。
var defaultWheelConfigs = []int64{
	1, // 毫秒级
	util.MILLISECONDS_OF_SECOND,
	util.MILLISECONDS_OF_MINUTE,
	util.MILLISECONDS_OF_10_MINUTE,
	util.MILLISECONDS_OF_HOUR,
}

// slotCountFor 取第 i 档轮的桶数（S67）：等于「本档滞留时长占几格」，即下一档精度
// 除以本档精度。毫秒轮滞留 [1,1000)ms → 1000 格、秒轮 60、分轮 10、十分轮 6；
// 最后一档无上界，取 24（一圈一天）。
//
// 按滞留时长取格数是为了让同一圈内不出现两个不同时刻挤进同一桶：被提前看到的节点
// 只是原地再等一圈（判定仍按 nextTime 与墙钟复核，不会错发），多一圈的冗余检查
// 换来的是「绝不在到期前被吞掉」，但滞留时长内不撞桶能让这份冗余归零。
// 精度表若被改动，这里会跟着变，不需要另维护一张常量表。
func slotCountFor(configs []int64, i int) int {
	if i+1 < len(configs) && configs[i] > 0 {
		if n := configs[i+1] / configs[i]; n >= 1 && n <= 4096 {
			return int(n)
		}
	}
	return 24
}

// runWheel 运行时间轮循环
func (m *TimerManager) runWheel(wheel *TimerWheel, wheelType TimerType) {
	ticker := time.NewTicker(time.Duration(wheel.wheelConfig) * time.Millisecond)
	defer ticker.Stop()

	vars.Info("启动时间轮: %s (精度: %dms)", wheelType.String(), wheel.wheelConfig)

	for wheel.isRunning.Load() {
		// recover 放在循环内：避免单次 panic 直接杀死时间轮协程，导致后续所有定时器失效
		func() {
			defer func() {
				if err := recover(); err != nil {
					vars.Error("时间轮运行发生panic错误: %v, 类型: %s", err, wheelType.String())
				}
			}()

			select {
			case <-m.closeChan:
				wheel.isRunning.Store(false)
				m.cleanupWheel(wheel, wheelType)
				vars.Info("时间轮停止: %s", wheelType.String())

			case task, ok := <-wheel.addTimerChan:
				// 通道永不关闭（resetTimerChannel 刻意不 close，见其注释），
				// 此分支只是防御性兜底：真收到 ok=false 说明上游被改坏了，
				// 继续循环只会对着 nil 通道空转，直接退出把这个轮暴露出来。
				if !ok {
					wheel.isRunning.Store(false)
					return
				}
				m.handleTimerAdd(wheel, task)

			case <-ticker.C:
				m.processWheelTick(wheel, wheelType)
			}
		}()
	}
}

// migratingMarker 是「节点已离开原轮、调度项正在投递途中」的占位归属。
//
// 投递必须先把归属改成该标记、再往目标通道发送：原先是「先投通道、后置 wheel=nil」，
// 目标轮的消费协程可能在本协程改写归属之前就跑完 handleTimerAdd 的校验，看到旧轮引用
// 仍在便判定「已入链」而丢弃这条调度项；本协程随后置 nil，节点于是两头无归属、
// 原轮计数已扣减、目标轮永不接收 —— 定时器永久停摆，且计数毫无漂移可查。
// handleTimerAdd 的认领条件因此放宽为「无归属或持有该标记」。
var migratingMarker = &TimerWheel{}

// ownedWheel 返回节点真正归属的时间轮；处于在途标记状态时视为「无归属」。
func (t *Timer) ownedWheel() *TimerWheel {
	if w := t.wheel.Load(); w != migratingMarker {
		return w
	}
	return nil
}

// processWheelTick 处理时间轮滴答事件
//
// 背压原则：通道满就把定时器留在当前轮等下一个 tick 重试，绝不新开协程。
// 原先的「go executeTimer」在毫秒轮上是 1ms 一次的无限 fork 源，
// 而消费端同步执行业务 Tick，下游一慢就会堆积到 OOM。
// 消费端可以是多个分片协程（S68），但每一片只有一个消费者，且满不满按片判定。
//
// 本函数自身不持任何轮锁：扫描判定在 tickWheelSection 里以无锁快照进行，
// 只有命中的节点会进 commitDispatch / unlinkStale 的短临界区，两处都以 defer
// 释放 wheelLock，因此即便临界区内 panic 也不会把这个轮永久扣死（Close 随之挂死）。
func (m *TimerManager) processWheelTick(wheel *TimerWheel, wheelType TimerType) {
	dropped := m.tickWheelSection(wheel, wheelType)

	// 告警必须在解锁之后：vars 异步日志缓冲满时会阻塞最长 5 秒
	if dropped > 0 {
		m.stats.timersDropped.Add(dropped)
		m.notifyBackpressure(wheel, wheelType, dropped)
	}
}

// tickWheelSection 扫描时间轮，派发到期项并下沉到更精确的轮。
//
// S67 起只扫「游标到此刻」之间的到期桶（slotRing.RangeWindow），一次 tick 的判定
// 代价随到期桶大小而非在链总数增长；落后超过一圈时退化为整环扫描，绝不漏派发。
//
// 判定全程不持 wheelLock：List.Range 只在链表自己的读锁里取快照，回调在锁外跑，
// 于是一次长扫描不再把业务侧的 Remove 整段排在后面（detachFromWheelLocked 要
// 同一把锁）。只有真正命中的节点才回锁，且锁内只剩「复核 → 认领 → 投递 → 摘链」
// 这几步无阻塞动作（投递用非阻塞 select，满了立刻放手）。
//
// 扫描范围由桶环收窄（S67）：RangeWindow 只看游标推进到的那几格，未到期桶整个跳过。
//
// 认领必须和摘链在同一把轮锁里做：业务移除是「持锁读归属 → 摘链 → 扣计数」的
// check-then-act，若 CAS 漂在锁外，两边能各自读到同一个归属并各扣一次计数
// （不变前的整轮持锁恰好挡住了这个交错，实测会让 timerCount 打成负数）。
func (m *TimerManager) tickWheelSection(wheel *TimerWheel, wheelType TimerType) (dropped int64) {
	currentTime := util.CurrentMS()
	// 通道数组快照取一次：到期节点按各自 uid 落到不同分片（S68），而本轮扫描期间
	// 分片数不可能变（改分片只在 TimeStop→Run 之间生效）。
	channels := currentTimerChannels()

	wheel.tickWheel.RangeWindow(currentTime, func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil {
			return true
		}
		// 已被迁移到别的轮或正在投递途中：归目标轮处理
		if parent.ownedWheel() != wheel {
			return true
		}
		if !parent.IsActive() {
			// 业务已移除，但 RemoveFromManager 与 handleTimerAdd 竞态：
			// 节点在 wheel 引用写入前就被判为「无轮」而漏摘，随后又被入链。
			// 它既不会被调度也不会被回收，计数还永远多 1 —— 顺手清掉。
			m.unlinkStale(wheel, parent, node)
			return true
		}

		task := timerTask{timer: timer, gen: parent.gen.Load(), mgr: m}
		if parent.nextTime.Load() <= currentTime {
			// 时间到达：只有派发成功才摘链，失败则留在轮里下个 tick 重试
			if m.commitDispatch(wheel, parent, node, task, shardOf(channels, parent.uid.Load())) == dispatchFull {
				dropped++
			}
			return true
		}

		// 检查是否需要迁移到更精确的时间轮
		remaining := parent.nextTime.Load() - currentTime - TimerMigrationOffset
		newType := parent.calculateType(remaining)
		if newType == wheelType || int(newType) >= len(m.wheels) {
			return true
		}
		switch m.commitDispatch(wheel, parent, node, task, m.wheels[newType].addTimerChan) {
		case dispatchSent:
			m.stats.wheelMigrations.Add(1)
		case dispatchFull:
			dropped++
		}
		return true
	})

	return dropped
}

// commitDispatch 的三种结论。
const (
	dispatchSent    int8 = iota // 已投递，节点已摘链并扣减计数
	dispatchSkipped             // 归属被别的路径拿走，本次什么都没做
	dispatchFull                // 目标通道满，归属已还原，留在本轮等下个 tick
)

// commitDispatch 在短临界区内完成复核、认领、投递与摘链扣计数。
//
// 先取 parent.mu 再取 wheelLock（与 AddTimer、handleTimerAdd、Remove 同序）：
// 目标轮的消费协程入链前也要这把 mu 锁，于是它不可能在本协程还挂着这个节点时
// 抢先 detach 走它 —— 那样本轮的摘链会错摘到目标链上，计数两头漂移。
//
// 先置「投递途中」再发送：反向顺序会让消费端在归属仍指向本轮时校验入链条件，
// 把这条调度项当重复入链丢弃。
func (m *TimerManager) commitDispatch(wheel *TimerWheel, parent *Timer, node list.INode, task timerTask, dst chan<- timerTask) int8 {
	parent.mu.Lock()
	defer parent.mu.Unlock()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 从快照判定到抢到锁的这段时间里节点可能已易主：归属仍是本轮才动手
	if parent.ownedWheel() != wheel || !parent.claimFromWheel(wheel) {
		return dispatchSkipped
	}
	select {
	case dst <- task:
		node.GetNode().Remove()
		wheel.timerCount.Add(-1)
		return dispatchSent
	default:
		// 通道满：归属必须原样还给本轮，否则本节点被 ownedWheel 判给「无轮」，
		// 后续每个 tick 都会跳过它，等价于永久停摆。
		parent.wheel.Store(wheel)
		// 节点留在原桶不动，但要告诉桶环「这一格里有东西没送走」：桶化后原地不动
		// 意味着下一拍不再覆盖这一格，重试会拖到一整圈之后（见 markResidue）。
		wheel.tickWheel.markResidue()
		return dispatchFull
	}
}

// unlinkStale 回锁清理「已不活跃却仍挂在轮上」的节点：摘链、扣计数、断开归属。
// 归属复核、认领与摘链同在锁内，理由同 commitDispatch。
//
// parent.mu 不能省：只挂 wheelLock 时，本协程认领完（归属已是 migratingMarker）
// 之后、摘链之前，并发的 AddTimer 复活能走完 mu → 目标轮锁把节点入进另一个轮
// ——handleTimerAdd 认 migratingMarker 为「无归属」，正是为迁移项放行的一条路。
// 随后本协程的 Remove() 摘的是新主的链、扣的却是本轮计数，最后的 Store(nil)
// 又把新主的归属抹掉：新轮永久幻影 +1，定时器彻底脱离调度。
func (m *TimerManager) unlinkStale(wheel *TimerWheel, parent *Timer, node list.INode) {
	parent.mu.Lock()
	defer parent.mu.Unlock()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	if parent.ownedWheel() != wheel || !parent.claimFromWheel(wheel) {
		return
	}
	node.GetNode().Remove()
	wheel.timerCount.Add(-1)
	parent.wheel.Store(nil)
	m.stats.timersRemoved.Add(1)
}

// cleanupWheel 清理时间轮
//
// 关键：业务回调（Tick）必须在完全释放 wheelLock 之后执行。
// Tick 内部调用 Remove / AddTimer 会再次申请同一把 wheelLock，
// 若持锁执行会立即自锁，导致 TimeStop 永久挂起（进程停不下来）。
//
// 收尾预算来自 CloseCtx：每个待补跑的回调执行前都复查一次 ctx，超时就放弃剩余
// 回调并把实例强制归还对象池。一个卡在下游锁上的 Tick 不该拖死整个进程退出。
func (m *TimerManager) cleanupWheel(wheel *TimerWheel, wheelType TimerType) {
	// 阶段一：持锁摘链，收集已过期的定时器（临界区独立成方法，panic 也不会扣住轮锁）
	expired := m.drainWheelLocked(wheel)

	ctx := context.Background()
	if p := m.closeCtx.Load(); p != nil {
		ctx = *p
	}

	// 阶段二：无锁执行（此时 Tick 内 Remove/AddTimer 都不会再触碰 wheelLock）
	for i, task := range expired {
		if ctx.Err() != nil {
			m.abandonExpired(wheelType, expired[i:])
			return
		}
		m.executeTimer(task)
	}
}

// abandonExpired 收尾超时时强制作废剩余定时器并归还对象池。
//
// 此刻没有回调在飞（inTick==0），作废结论为 releaseNow，因此必须在解锁后自行
// 归还——这一步不能省，否则实例既离开了时间轮又留在池外，永久占着内存。
// drainWheelLocked 已把这批定时器计入 timersRemoved，这里不重复计数。
func (m *TimerManager) abandonExpired(wheelType TimerType, tasks []timerTask) {
	for _, task := range tasks {
		if task.timer == nil {
			continue
		}
		parent := task.timer.GetParent()
		if parent == nil {
			continue
		}
		parent.mu.Lock()
		outcome := parent.removeFromManagerLocked(true)
		parent.mu.Unlock()
		if outcome == releaseNow {
			parent.releaseToPool()
		}
	}
	vars.Warning("收尾超时，强制归还定时器: 轮=%s, 数量=%d, 未执行到期回调",
		wheelType.String(), len(tasks))
	// 监控口径：这些定时器连最后一次 Tick 都没补上
	m.stats.timersDropped.Add(int64(len(tasks)))
}

// drainWheelLocked 持锁清空时间轮，返回需要补跑一次到期回调的调度项。
func (m *TimerManager) drainWheelLocked(wheel *TimerWheel) (expired []timerTask) {
	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	currentTime := util.CurrentMS()

	// 清空必须放在 defer 里：下面 Range 的回调会调进业务实现的 GetParent/IsActive，
	// 一旦 panic 逃出 Range，本函数后半段的 Clear 与归零都不执行，
	// 「收尾后轮空、计数归零」的口径（阶段12 S66 用例）当场破掉，
	// 而调用方 cleanupWheel 只 recover 到一次 panic，轮却永久带着节点。
	removedCount := wheel.timerCount.Load()
	defer func() {
		wheel.tickWheel.Clear()
		wheel.timerCount.Store(0)
		m.stats.timersRemoved.Add(removedCount)
	}()

	wheel.tickWheel.Range(func(node list.INode) bool {
		timer, ok := node.(TimerInterface)
		if !ok {
			return true
		}
		parent := timer.GetParent()
		if parent == nil {
			return true
		}
		// 已投递途中的节点不归本轮收尾：它的归属已是在途标记，
		// 由接收方或续期方负责，这里再动它会把别人的调度项打断。
		if parent.ownedWheel() != wheel {
			return true
		}
		// 无论是否到期都要断开 wheel 引用：本函数随后会 Clear 整个轮并把计数归零，
		// 残留引用会让业务侧后续的 Remove 误判节点仍在轮里，把计数减成负数。
		parent.wheel.Store(nil)
		if parent.IsActive() && parent.nextTime.Load() <= currentTime {
			node.GetNode().Remove()
			expired = append(expired, timerTask{timer: timer, gen: parent.gen.Load(), mgr: m})
		}
		return true
	})

	return expired
}
