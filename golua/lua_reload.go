package lua

import (
	"context"
	"os"

	"touchgocore/syncmap"
	"touchgocore/vars"
)

// reloadScript 重新加载脚本。ctx 为本代 watch 的快照，不回读 sw.ctx（换代隔离）。
func (sw *ScriptWatcher) reloadScript(ctx context.Context) {
	vars.Info("detected Lua script modification, reloading...")

	sw.mu.Lock()
	oldScript := sw.script
	sw.mu.Unlock()

	// 拷贝旧实例的登记表（对象本体共享，不复制）
	objectsCopy := copyRegisteredObjects(oldScript.registeredObjects)

	// 停止旧脚本：只拆运行时，对象马上要拷给新实例，走 Close() 会把它们 Delete 掉
	oldScript.closeKeepingObjects()

	// 创建新脚本
	newScript, err := NewLuaScriptWithContext(ctx, sw.scriptPath)
	if err != nil {
		vars.Error("reload Lua script failed: %v", err)
		// 恢复旧脚本
		sw.mu.Lock()
		sw.script = oldScript
		sw.mu.Unlock()

		// ls.ctx 由 rtMu 保护：CallWithContext 在读锁下读它，initLocked 在写锁下
		// 写它。这里裸写会与并发调用构成数据竞争，必须同样持写锁。
		oldScript.rtMu.Lock()
		oldScript.ctx = ctx
		oldScript.rtMu.Unlock()
		// Close 已把旧脚本的 update 定时器作废归还，不重建就再没人驱动它；
		// Init 失败时不能起表——那时 runtime 可能是 nil，update 会直接踩空。
		if err := oldScript.Init(); err != nil {
			vars.Error("回退恢复旧脚本时重新初始化失败: %v", err)
		} else if err := oldScript.startTimer(); err != nil {
			vars.Error("回退恢复旧脚本时定时器重建失败: %v", err)
		}

		// 触发回调
		sw.mu.RLock()
		callbacks := sw.callbacks
		callbacksOld := sw.callbacksOld
		sw.mu.RUnlock()

		for _, callback := range callbacks {
			callback(ctx, oldScript, false, err)
		}
		for _, callback := range callbacksOld {
			callback(oldScript, false, err)
		}
		return
	}

	// 恢复注册的对象（深拷贝）
	restoreRegisteredObjects(newScript, objectsCopy)

	// 旧实例已被新实例取代、且不再回退：从全局登记表摘除，否则每次热重载
	// 都会在 luaInstances 里多钉住一代旧实例（表长随代数无界增长，无法 GC）。
	// 只删旧代：新实例有自己的 UID，deleteInstance 还会校验 map 里的归属。
	unregisterLuaInstance(oldScript)

	// 更新脚本引用
	sw.mu.Lock()
	sw.script = newScript
	sw.mu.Unlock()

	// 默认实例跟着换代：本路径是「换对象」，旧实例的 runtime 已被拆掉，
	// 不跟着换的话 lua.Call 之后一直打到空壳上。
	// 回退分支不需要处理——defaultLua 从未被换走，仍是 oldScript。
	swapDefaultLua(oldScript, newScript)

	// 更新修改时间
	if info, err := os.Stat(sw.scriptPath); err == nil {
		sw.mu.Lock()
		sw.fileModTime = info.ModTime()
		sw.mu.Unlock()
	}

	// 更新依赖文件的修改时间
	sw.mu.Lock()
	for depPath := range sw.dependencies {
		if info, err := os.Stat(depPath); err == nil {
			sw.dependencies[depPath] = info.ModTime()
		}
	}
	sw.mu.Unlock()

	vars.Info("Lua script reloaded successfully")

	// 触发回调
	sw.mu.RLock()
	callbacks := sw.callbacks
	callbacksOld := sw.callbacksOld
	sw.mu.RUnlock()

	for _, callback := range callbacks {
		callback(ctx, newScript, true, nil)
	}
	for _, callback := range callbacksOld {
		callback(newScript, true, nil)
	}
}

// copyRegisteredObjects 复制登记表的键值对。对象本体是共享指针，不复制
func copyRegisteredObjects(src *syncmap.MapAny) *syncmap.MapAny {
	dst := syncmap.NewAny()
	src.Range(func(key, value interface{}) bool {
		dst.Store(key, value)
		return true
	})
	return dst
}

// restoreRegisteredObjects 把重载前存活的对象挂回新脚本，并把对象号水位推到
// 已有键之上——否则新脚本的计数器从 1 开始，新 userdata 会顶掉旧对象的注册项。
//
// 回挂用 LoadOrStore：重建实例的路径会先把主块跑一遍才走到这里，主块已经用
// 同样的对象号建了新对象。直接 Store 等于让旧对象盖掉新对象的登记项，新 userdata
// 还在 Lua 侧活着，之后所有方法调用都落在旧对象上。
func restoreRegisteredObjects(dst *LuaScript, src *syncmap.MapAny) {
	var maxUID int64
	src.Range(func(key, value interface{}) bool {
		if _, loaded := dst.registeredObjects.LoadOrStore(key, value); loaded {
			vars.Warning("重载时对象号 %v 已被新脚本的实例占用，旧对象不再回挂", key)
		}
		if uid, ok := key.(int64); ok && uid > maxUID {
			maxUID = uid
		}
		return true
	})
	for {
		cur := dst.nextObjectID.Load()
		if cur >= maxUID || dst.nextObjectID.CompareAndSwap(cur, maxUID) {
			break
		}
	}
}

// ReloadScript 手动重新加载脚本（向后兼容）
func (ls *LuaScript) ReloadScript() error {
	return ls.ReloadScriptWithContext(context.Background())
}

// ReloadScriptWithContext 使用上下文手动重新加载脚本。
//
// 重载与调用互斥：整个重载过程持有 rtMu 写锁，期间所有 CallWithContext 调用方阻塞
// 等待重载完成。选择阻塞而非失败返回，因为重载是短暂事件（仅文件变更时触发），
// 调用方无需实现重试逻辑。
func (ls *LuaScript) ReloadScriptWithContext(ctx context.Context) error {
	vars.Info("manually reloading Lua script: %s", ls.initScriptPath)

	// 拷贝登记表快照（对象本体是共享指针，不复制）：主块重跑后按号回挂，
	// 同时给 restoreRegisteredObjects 一个「重载前就存在」的判据
	objectsCopy := copyRegisteredObjects(ls.registeredObjects)

	// 取写锁保护整个重载过程：阻止并发调用访问半初始化的 runtime
	ls.rtMu.Lock()
	defer ls.rtMu.Unlock()

	// 关闭旧运行时
	ls.closeRuntimeLocked()

	// 重新初始化：注册表回放与方法回调捕获 regCtx，与 NewLuaScriptWithContext 同口径
	ls.ctx = ctx
	ls.regCtx = ctx
	if err := ls.initLocked(); err != nil {
		return err
	}

	// 读取并编译脚本文件，执行主脚本
	if err := ls.runMainChunkLocked(ls.initScriptPath); err != nil {
		return err
	}

	// 恢复注册的对象
	restoreRegisteredObjects(ls, objectsCopy)

	// 重建 update 定时器：Close 已把它彻底作废归还，不重新起表的话本实例重载后
	// 就再也没有人驱动 update()
	if err := ls.startTimer(); err != nil {
		return err
	}

	vars.Info("Lua script reloaded successfully")
	return nil
}
