package lua

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	rt "github.com/arnodel/golua/runtime"
	"touchgocore/syncmap"
	"touchgocore/vars"
)

// ScriptReloadCallback 热重载回调函数类型（新版本）
type ScriptReloadCallback func(ctx context.Context, script *LuaScript, success bool, err error)

// ScriptReloadCallbackOld 热重载回调函数类型（旧版本，向后兼容）
type ScriptReloadCallbackOld func(script *LuaScript, success bool, err error)

// ScriptWatcher 监控 Lua 脚本文件变化
type ScriptWatcher struct {
	scriptPath   string
	script       *LuaScript
	fileModTime  time.Time
	running      bool
	mu           sync.RWMutex
	stopChan     chan struct{}
	dependencies map[string]time.Time
	callbacks    []ScriptReloadCallback
	callbacksOld []ScriptReloadCallbackOld // 旧版本回调列表
	parentCtx    context.Context          // 原始父上下文，用于 Stop 后 Start 重建 ctx
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewScriptWatcher 创建脚本监视器（向后兼容）
func NewScriptWatcher(script *LuaScript) *ScriptWatcher {
	return NewScriptWatcherWithContext(context.Background(), script)
}

// NewScriptWatcherWithContext 使用上下文创建脚本监视器
func NewScriptWatcherWithContext(ctx context.Context, script *LuaScript) *ScriptWatcher {
	watcherCtx, cancel := context.WithCancel(ctx)
	return &ScriptWatcher{
		scriptPath:   script.initScriptPath,
		script:       script,
		stopChan:     make(chan struct{}),
		dependencies: make(map[string]time.Time),
		callbacks:    make([]ScriptReloadCallback, 0),
		callbacksOld: make([]ScriptReloadCallbackOld, 0),
		parentCtx:    ctx,
		ctx:          watcherCtx,
		cancel:       cancel,
	}
}

// AddDependency 添加依赖文件
func (sw *ScriptWatcher) AddDependency(depPath string) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	sw.dependencies[depPath] = time.Time{}
	if info, err := os.Stat(depPath); err == nil {
		sw.dependencies[depPath] = info.ModTime()
	}
}

// AddCallback 添加热重载回调（旧版本）
func (sw *ScriptWatcher) AddCallback(callback ScriptReloadCallbackOld) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.callbacksOld = append(sw.callbacksOld, callback)
}

// AddCallbackWithContext 添加热重载回调（新版本）
func (sw *ScriptWatcher) AddCallbackWithContext(callback ScriptReloadCallback) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.callbacks = append(sw.callbacks, callback)
}

// Start 开始监控脚本变化。支持 Stop 后重新 Start：会重建已取消的 ctx 和已关闭的 stopChan。
//
// 并发约束：ctx/cancel/stopChan/fileModTime 全部只在 mu 临界区内读写；
// watch 协程只使用 Start 捕获并传入的本代 ctx/stopChan 快照，不回读字段——
// 这样 Stop→Start 换代时旧协程仍握着已关闭的旧通道，必定退出，新协程不受影响。
func (sw *ScriptWatcher) Start() {
	sw.mu.Lock()
	if sw.running {
		sw.mu.Unlock()
		return
	}
	// 重启支持：Stop 会 cancel ctx 并 close stopChan，再次 Start 时必须重建，
	// 否则 watch 协程会立即退出（running=true 但无人监控）。
	parent := sw.parentCtx
	if parent == nil {
		parent = context.Background()
	}
	// 释放构造期派生的子 ctx，避免 context 泄漏（重复调用 cancel 是安全的 no-op）
	if sw.cancel != nil {
		sw.cancel()
	}
	sw.ctx, sw.cancel = context.WithCancel(parent)
	sw.stopChan = make(chan struct{})
	sw.running = true
	ctx, stopCh := sw.ctx, sw.stopChan

	// 初始化修改时间
	if info, err := os.Stat(sw.scriptPath); err == nil {
		sw.fileModTime = info.ModTime()
	}

	// 初始化依赖文件的修改时间
	for depPath := range sw.dependencies {
		if info, err := os.Stat(depPath); err == nil {
			sw.dependencies[depPath] = info.ModTime()
		}
	}
	depCount := len(sw.dependencies)
	sw.mu.Unlock()

	// 启动监控 goroutine（只持有本代快照）
	go sw.watch(ctx, stopCh)
	vars.Info("started watching Lua script: %s", sw.scriptPath)
	if depCount > 0 {
		vars.Info("watching %d dependency files", depCount)
	}
}

// Stop 停止监控
func (sw *ScriptWatcher) Stop() {
	sw.mu.Lock()
	if !sw.running {
		sw.mu.Unlock()
		return
	}
	sw.running = false
	// 捕获本代的 cancel/stopChan：锁外若与 Start 交错，sw.cancel 已指向新一代，
	// 直接读字段会把新一代误 cancel 掉
	cancel, stopCh := sw.cancel, sw.stopChan
	sw.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	close(stopCh)
	vars.Info("stopped watching Lua script")
}

// watch 监控文件变化。ctx/stopCh 为启动本协程那一代的快照，全程不回读
// sw.ctx/sw.stopChan——换代后本协程靠旧通道退出，天然与新一代隔离。
func (sw *ScriptWatcher) watch(ctx context.Context, stopCh chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if sw.checkFileChange() {
				sw.reloadScript(ctx)
			}
		case <-stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// checkFileChange 检查文件是否修改
func (sw *ScriptWatcher) checkFileChange() bool {
	// 检查主脚本文件
	info, err := os.Stat(sw.scriptPath)
	if err != nil {
		return false
	}

	sw.mu.RLock()
	modTime := sw.fileModTime
	sw.mu.RUnlock()
	if info.ModTime().After(modTime) {
		return true
	}

	// 检查依赖文件
	sw.mu.RLock()
	defer sw.mu.RUnlock()
	for depPath, modTime := range sw.dependencies {
		depInfo, err := os.Stat(depPath)
		if err != nil {
			return true // 依赖文件不存在，视为变化
		}
		if depInfo.ModTime().After(modTime) {
			return true
		}
	}

	return false
}

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

		oldScript.ctx = ctx
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

	// 更新脚本引用
	sw.mu.Lock()
	sw.script = newScript
	sw.mu.Unlock()

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

// GetScript 获取当前脚本
func (sw *ScriptWatcher) GetScript() *LuaScript {
	sw.mu.RLock()
	defer sw.mu.RUnlock()
	return sw.script
}

// EnableHotReload 启用热重载功能（向后兼容）
func EnableHotReload(script *LuaScript) *ScriptWatcher {
	return EnableHotReloadWithContext(context.Background(), script)
}

// EnableHotReloadWithContext 使用上下文启用热重载功能
func EnableHotReloadWithContext(ctx context.Context, script *LuaScript) *ScriptWatcher {
	watcher := NewScriptWatcherWithContext(ctx, script)
	watcher.Start()
	return watcher
}

// DisableHotReload 禁用热重载
func DisableHotReload(watcher *ScriptWatcher) {
	if watcher != nil {
		watcher.Stop()
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

	// 重新初始化
	ls.ctx = ctx
	if err := ls.initLocked(); err != nil {
		return err
	}

	// 重新注册函数：绑定方式必须与 NewLuaScriptWithContext 一致，
	// 否则重载之后宿主函数的第 2 个之后的实参会凭空消失
	registeredFuncsMu.RLock()
	for funcName, function := range registeredFuncs {
		ls.runtime.SetEnvGoFunc(ls.env, funcName, function, 1, true)
	}
	registeredFuncsMu.RUnlock()

	// 重新注册类
	registeredClassesMu.RLock()
	for _, class := range registeredClasses {
		if err := registerClassWithContext(ctx, class, ls); err != nil {
			vars.Error("register Lua class failed: %v", err)
		}
	}
	registeredClassesMu.RUnlock()

	// 读取并编译脚本文件
	source, err := os.ReadFile(ls.initScriptPath)
	if err != nil {
		return fmt.Errorf("read Lua script file failed: %w", err)
	}

	chunk, err := ls.runtime.CompileAndLoadLuaChunk("main", source, rt.TableValue(ls.env))
	if err != nil {
		return fmt.Errorf("compile Lua script failed: %w", err)
	}

	// 执行主脚本
	if _, err = rt.Call1(ls.thread, rt.FunctionValue(chunk)); err != nil {
		return fmt.Errorf("execute Lua script failed: %w", err)
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

// WatchMultipleFiles 监控多个 Lua 文件
type MultiFileWatcher struct {
	watchers  map[string]*ScriptWatcher
	running   bool
	mu        sync.RWMutex
	stopChan  chan struct{}
	parentCtx context.Context // 原始父上下文，用于 Stop 后 Start 重建
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewMultiFileWatcher 创建多文件监视器（向后兼容）
func NewMultiFileWatcher() *MultiFileWatcher {
	return NewMultiFileWatcherWithContext(context.Background())
}

// NewMultiFileWatcherWithContext 使用上下文创建多文件监视器
func NewMultiFileWatcherWithContext(ctx context.Context) *MultiFileWatcher {
	watcherCtx, cancel := context.WithCancel(ctx)
	return &MultiFileWatcher{
		watchers:  make(map[string]*ScriptWatcher),
		stopChan:  make(chan struct{}),
		parentCtx: ctx,
		ctx:       watcherCtx,
		cancel:    cancel,
	}
}

// AddScript 添加要监控的脚本。
// 每个子 watcher 持有独立的 cancel（派生自父 ctx），单个 watcher 的 Stop
// 不会影响其他兄弟 watcher。
// mfw 已在运行时会立即启动新 watcher，不必等下一次整体 Start。
func (mfw *MultiFileWatcher) AddScript(script *LuaScript) {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()
	// NewScriptWatcherWithContext 已从 mfw.ctx 派生独立的子 ctx + cancel，
	// 不再覆盖为父级的 cancel，避免单个 Stop 全局取消。
	watcher := NewScriptWatcherWithContext(mfw.ctx, script)
	mfw.watchers[script.initScriptPath] = watcher
	if mfw.running {
		// 锁序保持 mfw.mu → watcher.mu 单向（与 Start 一致），不会死锁
		watcher.mu.Lock()
		watcher.parentCtx = mfw.ctx
		watcher.mu.Unlock()
		watcher.Start()
	}
}

// Start 开始监控。支持 Stop 后重新 Start：会重建已取消的 ctx 和已关闭的 stopChan。
func (mfw *MultiFileWatcher) Start() {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()

	if mfw.running {
		return
	}

	// 重启支持：Stop 会 cancel ctx 并 close stopChan，再次 Start 时必须重建。
	parent := mfw.parentCtx
	if parent == nil {
		parent = context.Background()
	}
	// 释放构造期/上一代派生的子 ctx，避免 context 泄漏
	if mfw.cancel != nil {
		mfw.cancel()
	}
	mfw.ctx, mfw.cancel = context.WithCancel(parent)
	mfw.stopChan = make(chan struct{})
	mfw.running = true

	// 子 watcher 需要更新 parentCtx 为新的 mfw.ctx，否则它们派生的 ctx 仍来自已取消的旧父级。
	// 锁序单向：mfw.mu → watcher.mu，不会死锁。
	for _, watcher := range mfw.watchers {
		watcher.mu.Lock()
		watcher.parentCtx = mfw.ctx
		watcher.mu.Unlock()
		watcher.Start()
	}
	vars.Info("started watching %d Lua script files", len(mfw.watchers))
}

// Stop 停止监控
func (mfw *MultiFileWatcher) Stop() {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()

	if !mfw.running {
		return
	}
	mfw.running = false

	if mfw.cancel != nil {
		mfw.cancel()
	}
	for _, watcher := range mfw.watchers {
		watcher.Stop()
	}
	close(mfw.stopChan)
	vars.Info("stopped watching Lua script files")
}

// ReloadAll 重新加载所有脚本（向后兼容）
func (mfw *MultiFileWatcher) ReloadAll() error {
	return mfw.ReloadAllWithContext(context.Background())
}

// ReloadAllWithContext 使用上下文重新加载所有脚本
func (mfw *MultiFileWatcher) ReloadAllWithContext(ctx context.Context) error {
	mfw.mu.RLock()
	defer mfw.mu.RUnlock()

	for _, watcher := range mfw.watchers {
		if err := watcher.script.ReloadScriptWithContext(ctx); err != nil {
			return err
		}
	}
	return nil
}

// GetScriptByPath 根据路径获取脚本
func (mfw *MultiFileWatcher) GetScriptByPath(path string) (*LuaScript, bool) {
	mfw.mu.RLock()
	defer mfw.mu.RUnlock()

	watcher, ok := mfw.watchers[path]
	if !ok {
		return nil, false
	}
	return watcher.GetScript(), true
}
