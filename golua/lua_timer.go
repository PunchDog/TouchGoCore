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
func (lt *luaTimer) Tick() {
	tick := lt.tick.Add(1)

	// 定期触发垃圾回收
	if tick%GCTickCount == 0 {
		runtime.GC()
	}

	// 使用工作池并发更新对象
	lt.updateObjects(lt.ctx)
}

func (lt *luaTimer) updateObjects(ctx context.Context) {
	lt.luaScript.registeredObjects.Range(func(key, value interface{}) bool {
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
