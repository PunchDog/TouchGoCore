package lua

import (
	"context"
	"os"
	"sync"
	"time"

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
	parentCtx    context.Context           // 原始父上下文，用于 Stop 后 Start 重建 ctx
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

// watch 监控文件变化。ctx/stopCh 为启动本协程那一代的快照，不回读
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

// WatchMultipleFiles 监控多个 Lua 文件
type MultiFileWatcher struct {
	watchers  map[string]*ScriptWatcher
	running   bool
	mu        sync.RWMutex
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

// Start 开始监控。支持 Stop 后重新 Start：会重建已取消的 ctx。
func (mfw *MultiFileWatcher) Start() {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()

	if mfw.running {
		return
	}

	// 重启支持：Stop 会 cancel ctx，再次 Start 时必须重建。
	parent := mfw.parentCtx
	if parent == nil {
		parent = context.Background()
	}
	// 释放构造期/上一代派生的子 ctx，避免 context 泄漏
	if mfw.cancel != nil {
		mfw.cancel()
	}
	mfw.ctx, mfw.cancel = context.WithCancel(parent)
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
	vars.Info("stopped watching Lua script files")
}

// ReloadAll 重新加载所有脚本（向后兼容）
func (mfw *MultiFileWatcher) ReloadAll() error {
	return mfw.ReloadAllWithContext(context.Background())
}

// ReloadAllWithContext 使用上下文重新加载所有脚本。
//
// 本路径是「原地重建」（ReloadScriptWithContext 复用同一个 *LuaScript 对象），
// 所以 defaultLua 天然仍指向可用实例，不需要 swapDefaultLua。
// 与之相对，ScriptWatcher.reloadScript 走的是「换新对象」路径，必须换代
// defaultLua —— 两条路径别混淆。
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
