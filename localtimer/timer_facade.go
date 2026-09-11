package localtimer

import (
	"context"
	"sync"

	"touchgocore/syncmap"
	"touchgocore/vars"
)

// ==================== 定时器系统全局门面 ====================

// Run 启动定时器系统。ctx 取消时 TimeTick 退出。
func Run(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	timerRunCtx = ctx
	vars.Info("启动计时器系统")
	timerManagerMap = syncmap.NewMap[*TimerManager, bool]()
	defaultTimerManager = NewTimerManager()
	closech = make(chan any)
	tickWG.Add(1)
	go func() {
		defer tickWG.Done()
		TimeTick()
	}()
	vars.Info("计时器系统启动完成")
}

// TimeStop 停止定时器系统。ctx 可用于限制等待时间。
func TimeStop(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	vars.Info("正在停止计时器系统...")
	if closech != nil {
		select {
		case <-closech:
		default:
			close(closech)
		}
	}

	// 关闭所有定时器管理器
	if timerManagerMap == nil {
		return
	}
	timerManagerMap.Range(func(mgr *TimerManager, value bool) bool {
		vars.Info("关闭定时器管理器，当前定时器数量: %d", mgr.GetTimerCount())
		mgr.Close()
		// timerManagerMap.Delete(key)
		return true
	})
	timerManagerMap.Clear()

	done := make(chan struct{})
	go func() {
		tickWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		vars.Error("等待定时器协程退出超时: %v", ctx.Err())
	}

	if timerChannel != nil {
		close(timerChannel)
		timerChannel = nil
	}

	defaultTimerManager = nil
	managerInitOnce = sync.Once{}
	vars.Info("计时器系统已停止")
}

// AddTimer 向默认管理器添加定时器
func AddTimer(timer TimerInterface) error {
	if defaultTimerManager == nil {
		return ErrTimerSystemNotReady
	}

	if timer == nil || timer.GetParent() == nil {
		return ErrTimerNilParent
	}

	return defaultTimerManager.AddTimer(timer)
}

// TimeTick 处理定时器滴答
func TimeTick() {
	defer func() {
		if err := recover(); err != nil {
			vars.Error("定时器滴答处理发生panic错误: %v", err)
		}
	}()

	for {
		select {
		case timer, ok := <-timerChannel:
			if !ok {
				// 通道已关闭
				return
			}

			// recover 放在循环内：避免用户 Tick 内 panic 杀死消费协程，导致所有定时器失效
			func() {
				defer func() {
					if err := recover(); err != nil {
						vars.Error("定时器滴答处理发生panic错误: %v", err)
					}
				}()

				timer.Tick()
				if timer.HasNext() {
					if err := AddTimer(timer); err != nil {
						vars.Error("重新调度定时器失败: %v", err)
					}
				}
			}()
		case <-closech:
			return
		case <-timerRunCtx.Done():
			return
		}
	}
}

// GetDefaultManager 返回默认定时器管理器
func GetDefaultManager() *TimerManager {
	return defaultTimerManager
}

// IsSystemRunning 检查定时器系统是否正在运行
func IsSystemRunning() bool {
	return defaultTimerManager != nil && !defaultTimerManager.isClosed.Load()
}

// GetSystemStats 返回定时器系统统计信息
func GetSystemStats() (totalTimers int64, stats TimerStats) {
	if defaultTimerManager != nil {
		totalTimers = defaultTimerManager.GetTimerCount()
		stats = defaultTimerManager.GetStats()
	}
	return
}
