package localtimer

import (
	"errors"
	"runtime"

	"touchgocore/list"
	"touchgocore/vars"
)

// addTimerWarning 一次待发的背压告警。
//
// addTimerLocked 全程持有 parent.mu，而 vars 的异步日志在缓冲写满时最长阻塞 5 秒：
// 持锁写日志等于把这把定时器的 AddTimer/Remove/handleTimerAdd 全部排在这条日志后面。
// 因此临界区里只登记告警，真正的输出由解锁后的调用方发出。
type addTimerWarning struct {
	wheel     *TimerWheel
	wheelType TimerType
	dropped   int64 // >0 表示确有调度项被丢弃；0 表示仅水位过高
	used      int64
	capacity  int64
}

// emit 在锁外输出告警
func (m *TimerManager) emit(w *addTimerWarning) {
	if w == nil {
		return
	}
	if w.dropped > 0 {
		m.notifyBackpressure(w.wheel, w.wheelType, w.dropped)
		return
	}
	m.notifyHighWatermark(w.wheel, w.wheelType, w.used, w.capacity)
}

// AddTimer 向管理器添加定时器（业务首次注册与「先 Pause 再 AddTimer」复活都走这里；
// 已 Remove 作废的实例会被拒绝并返回 ErrTimerReleased）
//
// S67 桶化后它还是「改变一个在链定时器到期时刻」的唯一合法入口：先摘干净旧归属，
// 再按当前 nextTime 归桶入链（跨档迁移照旧发生）。直接改 nextTime 不换桶，
// 节点会留在一个与它无关的桶里，最坏等游标绕完一整圈才被重看。
func (m *TimerManager) AddTimer(timer TimerInterface) error {
	if timer == nil {
		return ErrTimerNilParent
	}
	parent := timer.GetParent()
	if parent == nil {
		return ErrTimerNilParent
	}

	// 定时器私有锁：把「清理旧调度 → 推进代次 → 恢复活跃 → 入队」整体与
	// handleTimerAdd 的「校验 → 入链」串行化。若不加锁，handleTimerAdd 校验
	// 通过后、入链执行前，本函数可完整跑完一轮并让新 task 先入链，对方随后
	// 拿着过期校验结果把同一节点二次入链，旧轮 timerCount 永久多 1（幻影 +1）。
	// 加锁顺序恒为 mu → wheelLock（handleTimerAdd 同序），不存在逆序死锁。
	parent.mu.Lock()
	err, warn := m.addTimerLocked(timer, parent, 0)
	parent.mu.Unlock()
	m.emit(warn)
	return err
}

// addTimerLocked 入队一个调度项，调用方必须已持有 parent.mu。
// 返回的告警必须在解锁后由调用方 emit，临界区内绝不写日志。
//
// renewGen == 0 表示业务注册/复活；非 0 表示到期续期，此时必须携带被派发那条
// 调度项的代次：代次纹丝未动才继续调度，否则返回 ErrTimerCanceled。少了这道
// 校验，与 Pause/Remove 竞态的续期会把「刚被业务停掉」的定时器重新置为活跃并
// 入链（rpc 重连成功后仍在不停重连）。只比代次不比 IsActive：上一次入队失败时
// 是我们自己把活跃标记回滚成 false 的，要求活跃会让重试永远作废，而业务停表对
// 一个活跃定时器必定推进代次，代次比对足以拦下复活。
func (m *TimerManager) addTimerLocked(timer TimerInterface, parent *Timer, renewGen uint64) (err error, warn *addTimerWarning) {
	if m.isClosed.Load() {
		return ErrTimerManagerClosed, nil
	}

	if renewGen != 0 {
		if parent.gen.Load() != renewGen {
			return ErrTimerCanceled, nil
		}
		// 代次没动却已进入作废流程：只能是移除时定时器本就不活跃（未推进代次）
		// 的边角场景。续期方是本协程自己，返回取消即可，不算业务复活失败。
		if parent.abandoned() {
			return ErrTimerCanceled, nil
		}
	} else if parent.abandoned() {
		// 该实例已永久归还对象池（或正排队等待 endTick 归还），契约上旧主人已放弃
		// 它，池随时可能把它发给下一个 NewTimer。实例自身分不清「刚归还的我」和
		// 「已易主的我」，因此一律拒绝复活，宁可让业务显式新建。
		timerPool.reviveRefused.Add(1)
		return ErrTimerReleased, nil
	}

	// 先选轮再改状态：类型非法时保持定时器原样返回，不必回滚活跃标记。
	wheelType := parent.GetType()
	if int(wheelType) >= len(m.wheels) {
		return ErrTimerInvalidType, nil
	}
	wheel := m.wheels[wheelType]

	// 没有分配时，分配唯一ID
	if parent.uid.Load() == 0 {
		parent.uid.CompareAndSwap(0, globalTimerUID.Add(1))
	}

	// 清理现有定时器：内部会推进代次，使在途的旧调度项失效，避免重复调度。
	// 此处已持有 parent.mu，直接调用无锁版本，避免重复加锁死锁。
	parent.removeFromManagerLocked(false)
	// RemoveFromManager 只在「原本是活跃」时才推进代次；定时器被业务临时置为
	// inactive（先 Remove 再 AddTimer 复活）时它会提前返回，此时在途的旧调度项
	// 仍持有同一个 gen，会被 isValid 误判为有效，导致同一实例被重复调度。
	// 因此这里无条件再推进一次，保证「每次 AddTimer 产生的调度项 gen 全局唯一」。
	// 直接取 Add 的返回值作为本代次：原子加法返回的新值天然全局唯一。若这里丢弃
	// 返回值、入队前再 Load，两个并发 AddTimer 会读到同一个更大的 gen，各自入队
	// 一份相同 gen 的 task，isValid 全部放行 → 同一节点被二次入链、计数多加，
	// 后续配对减完就出现 -1。这正是一次性修复引用计数出现 -1 的主因。
	newGen := parent.gen.Add(1)
	// RemoveFromManager 会把 isActive 置为 false，这里恢复活跃状态重新进入调度
	parent.isActive.Store(true)

	chanLen := int64(len(wheel.addTimerChan))
	chanCap := int64(cap(wheel.addTimerChan))

	// 使用上面 Add 返回的唯一代次入队，杜绝并发 AddTimer 的重复 gen
	task := timerTask{timer: timer, gen: newGen, mgr: m}

	select {
	case wheel.addTimerChan <- task:
		m.stats.timersAdded.Add(1)
		if float64(chanLen) >= float64(chanCap)*0.9 {
			return nil, &addTimerWarning{wheel: wheel, wheelType: wheelType, used: chanLen, capacity: chanCap}
		}
		return nil, nil
	default:
		// 通道满：丢弃本次调度。入队失败即意味着节点不在任何轮里，
		// 必须把 isActive 撤回去：留着 true 就是「幻影活跃」——IsActive()
		// 谎报在调度，而 processWheelTick 的失活清理分支也永远不会回收它。
		// 绝不阻塞、绝不新开协程——原先的「阻塞 100ms 后 go executeTimer」
		// 会与 executeTimer 内部的续期 AddTimer 构成递归 fork，
		// 下游 Tick 一旦变慢就会指数级堆积协程直至 OOM。
		parent.isActive.Store(false)
		m.stats.timersDropped.Add(1)
		// 这条调度项到此为止：既没入链也不会有人再执行，计入「脱离调度」
		m.stats.timersRemoved.Add(1)
		return ErrTimerChannelFull, &addTimerWarning{wheel: wheel, wheelType: wheelType, dropped: 1}
	}
}

// addTimerWithRetry 有界重试重新入队。
//
// 这是「不丢任务」的最后一道关卡：AddTimer 失败时节点已经离开时间轮，
// 不复投就会永久消失，业务侧表现为该定时器的 Tick 再也不触发。
// 重试次数刻意很小且不 sleep —— 续期跑在 timeTick 消费协程的关键路径上，
// 任何额外等待都会放大整条链路的延迟。
func (m *TimerManager) addTimerWithRetry(timer TimerInterface, expectGen uint64) error {
	const maxRetry = 3
	parent := timer.GetParent()
	if parent == nil {
		return ErrTimerNilParent
	}

	var err error
	for i := 0; i < maxRetry; i++ {
		var warn *addTimerWarning
		err, warn = m.addTimerWithLock(timer, parent, expectGen)
		if warn != nil {
			// 续期跑在 timeTick 消费协程上：限频告警每秒最多一条，
			// 且此时 parent.mu 已释放，可以安全输出。
			m.emit(warn)
		}
		if err == nil || !errors.Is(err, ErrTimerChannelFull) {
			return err
		}
		// 上一次尝试已烧掉一个代次却没入队，重试的期望值改为「当前代次」：
		// 语义从「还是我这条被派发的调度项」放宽为「期间无人再改动该定时器」。
		expectGen = parent.gen.Load()
		runtime.Gosched()
	}
	return err
}

// addTimerWithLock 取定时器私有锁后入队（续期路径不走 AddTimer 公共入口，
// 以便把期望代次一路传进临界区）。
func (m *TimerManager) addTimerWithLock(timer TimerInterface, parent *Timer, expectGen uint64) (error, *addTimerWarning) {
	parent.mu.Lock()
	err, warn := m.addTimerLocked(timer, parent, expectGen)
	parent.mu.Unlock()
	return err, warn
}

// handleTimerAdd 处理定时器添加到时间轮
func (m *TimerManager) handleTimerAdd(wheel *TimerWheel, task timerTask) {
	if !task.isValid() {
		return // 过期调度项：定时器已被移除或复用，丢弃
	}

	node, ok := task.timer.(list.INode)
	if !ok {
		vars.Error("定时器未实现 list.INode，无法加入时间轮")
		return
	}

	parent := task.timer.GetParent()
	// 先取定时器私有锁、再取时间轮锁（与 AddTimer 加锁顺序一致）：
	// 把「校验 + 入链 + 计数」与 AddTimer 的「摘链 + 推进代次 + 入队」串行化。
	// gen 唯一 ≠ 只有一份 task 会入链——不加锁时，本校验通过后、入链执行前，
	// 并发 AddTimer 可让更新的 task 先入链，本函数再把同一节点二次入链，
	// list.Add 的 detach+relink 使节点物理迁移到新链表，旧轮 timerCount 永远
	// 多 1（count 与 len 漂移：幻影 +1 / -1）。
	parent.mu.Lock()
	defer parent.mu.Unlock()

	wheel.wheelLock.Lock()
	defer wheel.wheelLock.Unlock()

	// 加锁后再次校验：并发 Remove 可能刚好发生在这之前
	if !task.isValid() {
		return
	}

	// 已进入作废流程的实例绝不入链：在途调度项可能晚于 Put 才被本轮消化，
	// 此刻 isActive/gen 都可能已被下一个主人重新用起，光靠 isValid 分不出来。
	// 持锁路径禁止打日志（vars 异步缓冲满时最长阻塞 5 秒，会拖死整个时间轮）。
	if parent.abandoned() {
		return
	}

	// 节点已真实归属某个轮（不是「投递途中」的占位标记）才丢弃本次入链：能走到
	// 这里且归属非空，说明节点已带着当前代次正确在链（或已易主到别的轮），
	// 再次入链只会让计数多加。处于 migratingMarker 的正是刚从原轮摘链、投给本轮
	// 的迁移项，必须认领，否则原轮计数已扣、目标轮不接手，定时器永久停摆。
	if cur := parent.wheel.Load(); cur != nil && cur != migratingMarker {
		return
	}

	// 入链失败必须回滚归属且不加计数：list.Add 内部 recover 后返回 false 时
	// 节点并不在链表里，留着归属或计数都会形成永久幻影 +1。
	if !wheel.tickWheel.Add(node) {
		parent.wheel.Store(nil)
		return
	}
	parent.wheel.Store(wheel)
	// 计数只在「节点真正入链」处自增，与 processWheelTick / cleanupWheel 的
	// 自减严格配对。放在 AddTimer 里统计的是入队次数，迁移到达不经过那里，
	// 会让计数每次迁移净减一。
	wheel.timerCount.Add(1)
}
