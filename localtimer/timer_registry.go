package localtimer

import (
	"context"
	"sync"
	"sync/atomic"

	"touchgocore/syncmap"
	"touchgocore/vars"
)

// notifyTimerLost 上报「续期彻底失败」的定时器，并把它强制归还对象池。
//
// 重试仍失败说明这条调度项已经永久消失：节点不在任何轮里、isActive 也已回滚，
// 业务侧表现为该定时器的 Tick 再也不触发，且从外部完全无法察觉。这里主动作废
// 实例（此刻 inTick 仍为 1，归还会由 endTick 落地），把「静默失联」变成一次
// 显式回收 + 一条可接管的回调，业务据此重建定时器而不是等一个永远不来的 Tick。
func (m *TimerManager) notifyTimerLost(timer TimerInterface, parent *Timer, cause error) {
	info := TimerLostInfo{
		Manager:   m,
		UID:       timer.GetUID(),
		Interval:  parent.GetInterval(),
		Remaining: parent.GetRemainingCount(),
		Cause:     cause,
	}
	vars.Error("定时器续期失败已强制回收: uid=%d, 间隔=%dms, 剩余次数=%d, 原因=%v",
		info.UID, info.Interval, info.Remaining, cause)

	if fn := onTimerLost.Load(); fn != nil {
		// 回调来自业务，绝不能让它把收尾（下面的 retire 与 endTick）打断
		func() {
			defer func() {
				if r := recover(); r != nil {
					vars.Error("OnTimerLost 回调发生panic: %v", r)
				}
			}()
			(*fn)(info)
		}()
	}
	m.retireAfterExhaustion(parent)
}

// retireAfterExhaustion 作废执行完最后一拍的定时器并归还对象池。
//
// 此刻 inTick 仍为 1，归还请求会被挂起、由 executeTimer 尾部的 endTick 落地，
// 因此本函数返回时实例还没有交给池，调用链上后续的状态读取都是安全的。
func (m *TimerManager) retireAfterExhaustion(parent *Timer) {
	parent.mu.Lock()
	parent.removeFromManagerLocked(true)
	parent.mu.Unlock()
	// 不在这里补 Put：结论必为 releaseDeferred（inTick 仍为 1），
	// 由 executeTimer 尾部的 endTick 独占落地，避免两头争同一次归还。
}

// timerRuntime 是一次 Run → TimeStop 生命周期的全部可变状态。
//
// 整体用 atomic.Pointer 替换，而不是把 closech / tickWG / ctx 摊成三个裸全局：
// Run 写、TimeStop 读并关闭、TimeTick 在 select 里读，逐字段访问必然竞态，
// 且 TimeStop 对 closech「先查后关」不是原子的，两个并发调用会双重 close
// 直接触发 fatal 的 "close of closed channel"。Swap 保证只有一方拿到非 nil
// 实例，closeOnce 再做一层幂等兜底。
type timerRuntime struct {
	closech   chan struct{}
	closeOnce sync.Once
	tickWG    sync.WaitGroup
	ctx       context.Context

	// degraded：本轮生命周期里有分片消费协程 panic 退出（S68）。置位后其余分片
	// 照常消费，但健康检查报告「未运行」——半死不活的状态不该对外报正常。
	degraded atomic.Bool
}

// shutdown 关闭本轮生命周期，幂等且并发安全
func (rt *timerRuntime) shutdown() {
	rt.closeOnce.Do(func() { close(rt.closech) })
}

// TimerLostInfo 描述一个彻底失联（续期重试仍失败、已被强制回收）的定时器。
type TimerLostInfo struct {
	Manager   *TimerManager // 归属管理器
	UID       int64         // 定时器唯一ID
	Interval  int64         // 执行间隔（毫秒）
	Remaining int64         // 剩余执行次数，-1 表示无限
	Cause     error         // 续期失败的最终原因
}

// onTimerLost 业务注册的失联回调（copy-on-write，读写不竞争）
var onTimerLost atomic.Pointer[func(TimerLostInfo)]

// SetOnTimerLost 注册「定时器彻底失联」回调；传 nil 取消。
//
// 正常路径下该回调永不触发；一旦触发说明下游 Tick 太慢导致调度项被丢弃，
// 业务必须据此重建定时器。回调在 timeTick 消费协程上执行，禁止阻塞。
func SetOnTimerLost(fn func(TimerLostInfo)) {
	if fn == nil {
		onTimerLost.Store(nil)
		return
	}
	onTimerLost.Store(&fn)
}

// 全局状态（定时器系统单例相关）
var (
	// timerRegistryMu 冻结注册表的「批量关闭」与「单个注册」：Run/TimeStop 取待关
	// 清单的窗口内，并发 NewTimerManager 必须排在它后面。否则刚建好的管理器会
	// 落在清单外却被随后的清空抹掉，且从未 Close —— 它名下 5 个时间轮协程 +
	// 5 个 ticker 永久泄漏。清单内的管理器各自 Close 时自摘，不碰这段临界区。
	timerRegistryMu     sync.Mutex
	timerManagerMap     = syncmap.NewMap[*TimerManager, bool]() // map[*TimerManager]bool
	globalTimerUID      atomic.Int64                            // 定时器ID：跨管理器全局唯一
	defaultTimerManager atomic.Pointer[TimerManager]
	timerRT             atomic.Pointer[timerRuntime]

	// timerChannels 刻意保持全局、跨生命周期共享：
	// NewTimerManager 可以在未 Run 的情况下独立调用，此时也得有地方派发。
	// S68 起为分片数组：下标即分片号，一条分片一个消费协程，同一 uid 恒定落同一分片。
	timerChannels     []chan timerTask
	timerChannelMu    sync.Mutex
	timerChannelReady bool
)

// registerTimerManager 把管理器登记进注册表
func registerTimerManager(mgr *TimerManager) {
	timerRegistryMu.Lock()
	timerManagerMap.Store(mgr, true)
	timerRegistryMu.Unlock()
}

// unregisterTimerManager 从注册表摘除管理器（只由该管理器自己的 Close 调用）
func unregisterTimerManager(mgr *TimerManager) {
	timerRegistryMu.Lock()
	timerManagerMap.Delete(mgr)
	timerRegistryMu.Unlock()
}

// snapshotTimerManagers 在冻结注册表的窗口内取出当前存量的全部管理器。
//
// 返回后立刻放开锁：Close 内部会反过来申请这把锁做自摘，持锁等待退出会自锁。
func snapshotTimerManagers() []*TimerManager {
	timerRegistryMu.Lock()
	defer timerRegistryMu.Unlock()

	mgrs := make([]*TimerManager, 0, timerManagerMap.Length())
	timerManagerMap.Range(func(mgr *TimerManager, _ bool) bool {
		mgrs = append(mgrs, mgr)
		return true
	})
	return mgrs
}
