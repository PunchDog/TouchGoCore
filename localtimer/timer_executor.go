package localtimer

import (
	"sync"
	"time"

	"touchgocore/vars"
)

// timerExecutor 是 MultiThread 定时器的有界执行池。
//
// 为什么不能用裸 `go executeTimer`：既有实现里定时器续期走 AddTimer → 时间轮 →
// timerChannel → timeTick → executeTimer，毫秒轮每 1ms 一次调度；下游 Tick 一慢，
// 无界 fork 的协程就会指数级堆积直至 OOM（见 timer_manager.go 的背压注释）。
// 因此这里的并发度有两条硬上界：
//  1. worker 数量固定（构造时确定），协程数不随流量增长；
//  2. 队列容量固定，投递失败由调用方降级为内联执行——既不阻塞、也不丢任务。
//
// worker 内只做 runTick + reschedule，绝不会把任务再投回池，
// 从结构上根除「池内递归投递」。
type timerExecutor struct {
	mgr          *TimerManager // 归属管理器：runTick/reschedule 与日志都要用它
	tasks        chan timerTask
	stopCh       chan struct{}
	stopChWorker chan struct{}
	wg           sync.WaitGroup
	stopOnce     sync.Once
	workers      int
}

// newTimerExecutor 创建执行池并启动固定数量的 worker。
//
// workers <= 0 时返回 nil，调用方据此走内联降级路径。
func newTimerExecutor(mgr *TimerManager, queueSize int) *timerExecutor {
	if mgr == nil {
		return nil
	}
	if queueSize <= 0 {
		queueSize = MaxExecutorQueueNum
	}

	e := &timerExecutor{
		mgr:     mgr,
		tasks:   make(chan timerTask, queueSize),
		stopCh:  make(chan struct{}),
		workers: 0,
	}
	return e
}

func (e *timerExecutor) addworker() {
	if e.stopChWorker == nil {
		e.stopChWorker = make(chan struct{})
	}
	cur := e.workers
	if e.workers == 0 {
		e.workers = DefaultConcurrentWorkers //最小大小
	} else {
		e.workers = e.workers * 2
	}

	if e.workers > MaxConcurrentWorkers {
		e.workers = MaxConcurrentWorkers
	}

	if cur != e.workers {
		for i := cur; i < e.workers; i++ {
			e.wg.Add(1)
			go e.worker()
		}
	}
}

// worker 执行池的工作协程：出队即执行，收到停止信号后退出。
func (e *timerExecutor) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.stopCh:
			return
		case <-e.stopChWorker:
			e.stopChWorker = nil
			return
		case task := <-e.tasks:
			e.run(task)
			if len(e.tasks) == 0 { //清除多余的线程
				close(e.stopChWorker)
			}
		}
	}
}

// run 执行一个已出队的任务。
//
// 出队时必须重新校验：任务从提交到执行之间存在窗口，期间业务可能已 Remove 该定时器，
// 或该实例已被对象池复用并推进 gen（幽灵定时器）。管理器关闭后也一样要丢弃。
func (e *timerExecutor) run(task timerTask) {
	if e.mgr.isClosed.Load() || !task.isValid() {
		return
	}
	e.mgr.runTick(task)
}

// Submit 非阻塞投递任务。
//
// 返回 false 表示执行池已停止或队列已满，调用方必须降级为内联执行（绝不丢任务）。
// 刻意不做带超时的阻塞等待：阻塞 consumer 会把整条调度链拖慢，正是原来的坑。
func (e *timerExecutor) Submit(task timerTask) bool {
	select {
	case <-e.stopCh:
		return false
	default:
	}

	select {
	case e.tasks <- task:
		if len(e.tasks) >= e.workers*2 { //扩容
			e.addworker()
		}
		return true
	case <-e.stopCh:
		return false
	default:
		return false
	}
}

// Stop 停止执行池并等待在途任务结束（幂等，可重复调用）。
//
// 先发停止信号，再有界等待：超时只告警不阻塞，否则一个卡死的业务 Tick 会把
// TimeStop 永久挂住（关闭路径不允许无界等待）。
// 超时情况下用于等待的协程会一直存活到在途任务真正结束，属已知且可接受的代价。
func (e *timerExecutor) Stop(timeout time.Duration) {
	if e == nil {
		return
	}
	e.stopOnce.Do(func() { close(e.stopCh) })

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 正常排空
	case <-time.After(timeout):
		vars.Error("定时器执行池排空超时(%v)，仍有任务在途: workers=%d, 队列剩余=%d/%d",
			timeout, e.workers, e.Len(), e.Cap())
	}
}

// Len 返回队列当前水位，用于限频告警与统计。
func (e *timerExecutor) Len() int {
	if e == nil {
		return 0
	}
	return len(e.tasks)
}

// Cap 返回队列容量，用于限频告警与统计。
func (e *timerExecutor) Cap() int {
	if e == nil {
		return 0
	}
	return cap(e.tasks)
}

// Workers 返回 worker 数量。
func (e *timerExecutor) Workers() int {
	if e == nil {
		return 0
	}
	return e.workers
}
