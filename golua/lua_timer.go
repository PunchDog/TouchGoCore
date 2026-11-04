package lua

import (
	"context"
	"runtime"
	"sync/atomic"

	"touchgocore/localtimer"
)

// luaTimer 定时器结构
type luaTimer struct {
	localtimer.Timer
	tick      atomic.Int64
	luaScript *LuaScript
	ctx       context.Context
}

// Tick 执行定时更新
func (tmr *luaTimer) Tick() {
	tick := tmr.tick.Add(1)

	// 定期触发垃圾回收
	if tick%luaGCTickCountValue() == 0 {
		runtime.GC()
	}

	// 使用工作池并发更新对象
	tmr.updateObjects(tmr.ctx)
}

func (tmr *luaTimer) updateObjects(ctx context.Context) {
	tmr.luaScript.registeredObjects.Range(func(key, value interface{}) bool {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return false
			default:
			}
		}
		obj, ok := value.(ILuaClassInterface)
		if !ok {
			return true
		}
		obj.Update()
		return true
	})
}
