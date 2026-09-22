package localtimer

import (
	"context"
	"sync"
	"sync/atomic"
)

// timerTask 是调度通道中传递的定时器项。
//
// gen 记录入队瞬间的定时器代次；出队时若代次已变，说明该定时器已被移除或被
// 对象池复用给另一个业务，这条在途的旧调度项必须丢弃，否则会对同一个实例
// 重复调度、甚至操作已被别人复用的对象（即「幽灵定时器 / 串号」问题）。
//
// mgr 记录归属管理器。调度通道是全局共享的（分片后各管理器仍共用同一套分片），
// 若不带上归属，消费端只能一律交给默认管理器执行并续期，
// 非默认管理器的定时器首次触发后就被改嫁了。
type timerTask struct {
	timer TimerInterface
	gen   uint64
	mgr   *TimerManager
}

// isValid 判断该调度项是否仍然有效
//
// 刻意不检查 mgr.isClosed：收尾路径（cleanupWheel）正是靠关闭管理器时补跑最后一拍
// 来把定时器交回对象池，一旦在这里判废，在途实例既不被执行也无人回收，永久滞留。
// 「陈旧调度项跨 Run 复活」由全局通道消费端 timeTick 拦住（见该函数）。
func (t timerTask) isValid() bool {
	if t.timer == nil || t.mgr == nil {
		return false
	}
	parent := t.timer.GetParent()
	if parent == nil {
		return false
	}
	return parent.IsActive() && parent.gen.Load() == t.gen
}

// TimerManager 管理所有时间轮
type TimerManager struct {
	closeChan chan struct{}      // 关闭信号
	wheels    []*TimerWheel      // 时间轮数组
	isClosed  atomic.Bool        // 是否关闭
	stats     timerStatsCounters // 内部原子计数
	wheelWG   sync.WaitGroup

	// closeCtx 收尾预算：由 CloseCtx 在发出关闭信号前写入，供 cleanupWheel 逐节点
	// 判定「还能不能继续等业务回调」。用指针快照而非字段直改，避免与读取方竞态。
	closeCtx atomic.Pointer[context.Context]
}

// NewTimerManager 创建新的定时器管理器
func NewTimerManager() *TimerManager {
	ensureTimerChannel()

	// 拷一份精度表：直接把包级切片交给 mgr 等于让任何一处对 defaultWheelConfigs 的
	// 改写（测试为造档而临时改表是常见写法）同时改掉此后所有管理器的档位与桶数推导。
	wheelConfigs := make([]int64, len(defaultWheelConfigs))
	copy(wheelConfigs, defaultWheelConfigs)

	mgr := &TimerManager{
		closeChan: make(chan struct{}),
		wheels:    make([]*TimerWheel, len(wheelConfigs)),
	}

	// 初始化时间轮
	for i, config := range wheelConfigs {
		wheel := &TimerWheel{
			wheelConfig: config,
			// 每档轮内部再按 nextTime 分桶（S67），一次 tick 只扫到期桶。
			// 桶内仍是无索引链表：时间轮只按顺序遍历与摘除节点，从不按 ID 查节点。
			tickWheel:    newSlotRing(config, slotCountFor(wheelConfigs, i)),
			addTimerChan: make(chan timerTask, MaxAddTimerChannelNum),
			mgr:          mgr,
		}
		wheel.isRunning.Store(true)
		mgr.wheels[i] = wheel

		// 启动时间轮协程
		mgr.wheelWG.Add(1)
		go func(w *TimerWheel, t TimerType) {
			defer mgr.wheelWG.Done()
			mgr.runWheel(w, t)
		}(wheel, TimerType(i))
	}

	registerTimerManager(mgr)
	return mgr
}
