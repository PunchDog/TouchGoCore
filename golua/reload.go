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
		scriptPath:    script.initScriptPath,
		script:        script,
		stopChan:      make(chan struct{}),
		dependencies:  make(map[string]time.Time),
		callbacks:     make([]ScriptReloadCallback, 0),
		callbacksOld: make([]ScriptReloadCallbackOld, 0),
		ctx:           watcherCtx,
		cancel:        cancel,
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

// Start 开始监控脚本变化
func (sw *ScriptWatcher) Start() {
	sw.mu.Lock()
	if sw.running {
		sw.mu.Unlock()
		return
	}
	sw.running = true
	sw.mu.Unlock()

	// 初始化修改时间
	if info, err := os.Stat(sw.scriptPath); err == nil {
		sw.fileModTime = info.ModTime()
	}

	// 初始化依赖文件的修改时间
	sw.mu.Lock()
	for depPath := range sw.dependencies {
		if info, err := os.Stat(depPath); err == nil {
			sw.dependencies[depPath] = info.ModTime()
		}
	}
	sw.mu.Unlock()

	// 启动监控 goroutine
	go sw.watch()
	vars.Info("started watching Lua script: %s", sw.scriptPath)
	sw.mu.RLock()
	depCount := len(sw.dependencies)
	sw.mu.RUnlock()
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
	sw.mu.Unlock()

	if sw.cancel != nil {
		sw.cancel()
	}
	close(sw.stopChan)
	vars.Info("stopped watching Lua script")
}

// watch 监控文件变化
func (sw *ScriptWatcher) watch() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if sw.checkFileChange() {
				sw.reloadScript()
			}
		case <-sw.stopChan:
			return
		case <-sw.ctx.Done():
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

	if info.ModTime().After(sw.fileModTime) {
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

// reloadScript 重新加载脚本
func (sw *ScriptWatcher) reloadScript() {
	vars.Info("detected Lua script modification, reloading...")

	sw.mu.Lock()
	oldScript := sw.script
	sw.mu.Unlock()

	// 深拷贝注册的对象
	objectsCopy := copyRegisteredObjects(oldScript.registeredObjects)

	// 停止旧脚本（但不删除对象）
	oldScript.Close()

	// 创建新脚本
	newScript, err := NewLuaScriptWithContext(sw.ctx, sw.scriptPath)
	if err != nil {
		vars.Error("reload Lua script failed: %v", err)
		// 恢复旧脚本
		sw.mu.Lock()
		sw.script = oldScript
		sw.mu.Unlock()
		
		oldScript.ctx = sw.ctx
		oldScript.Init()

		// 触发回调
		sw.mu.RLock()
		callbacks := sw.callbacks
		callbacksOld := sw.callbacksOld
		sw.mu.RUnlock()
		
		for _, callback := range callbacks {
			callback(sw.ctx, oldScript, false, err)
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
		sw.fileModTime = info.ModTime()
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
		callback(sw.ctx, newScript, true, nil)
	}
	for _, callback := range callbacksOld {
		callback(newScript, true, nil)
	}
}

// copyRegisteredObjects 深拷贝注册对象
func copyRegisteredObjects(src *syncmap.MapAny) *syncmap.MapAny {
	dst := syncmap.NewAny()
	src.Range(func(key, value interface{}) bool {
		dst.Store(key, value)
		return true
	})
	return dst
}

// restoreRegisteredObjects 恢复注册对象
func restoreRegisteredObjects(dst *LuaScript, src *syncmap.MapAny) {
	src.Range(func(key, value interface{}) bool {
		dst.registeredObjects.Store(key, value)
		return true
	})
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

// ReloadScriptWithContext 使用上下文手动重新加载脚本
func (ls *LuaScript) ReloadScriptWithContext(ctx context.Context) error {
	vars.Info("manually reloading Lua script: %s", ls.initScriptPath)

	// 深拷贝注册的对象
	objectsCopy := copyRegisteredObjects(ls.registeredObjects)

	// 关闭旧状态
	ls.Close()

	// 重新初始化
	ls.ctx = ctx
	if err := ls.Init(); err != nil {
		return err
	}

	// 重新注册函数
	for funcName, function := range registeredFuncs {
		ls.runtime.SetEnvGoFunc(ls.env, funcName, function, 1, false)
	}

	// 重新注册类
	for class := range registeredClasses {
		if err := registerClassWithContext(ctx, class, ls); err != nil {
			vars.Error("register Lua class failed: %v", err)
		}
	}

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

	vars.Info("Lua script reloaded successfully")
	return nil
}

// WatchMultipleFiles 监控多个 Lua 文件
type MultiFileWatcher struct {
	watchers map[string]*ScriptWatcher
	running  bool
	mu       sync.RWMutex
	stopChan chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewMultiFileWatcher 创建多文件监视器（向后兼容）
func NewMultiFileWatcher() *MultiFileWatcher {
	return NewMultiFileWatcherWithContext(context.Background())
}

// NewMultiFileWatcherWithContext 使用上下文创建多文件监视器
func NewMultiFileWatcherWithContext(ctx context.Context) *MultiFileWatcher {
	watcherCtx, cancel := context.WithCancel(ctx)
	return &MultiFileWatcher{
		watchers: make(map[string]*ScriptWatcher),
		stopChan: make(chan struct{}),
		ctx:      watcherCtx,
		cancel:   cancel,
	}
}

// AddScript 添加要监控的脚本
func (mfw *MultiFileWatcher) AddScript(script *LuaScript) {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()
	watcher := NewScriptWatcher(script)
	watcher.ctx = mfw.ctx
	watcher.cancel = mfw.cancel
	mfw.watchers[script.initScriptPath] = watcher
}

// Start 开始监控
func (mfw *MultiFileWatcher) Start() {
	mfw.mu.Lock()
	defer mfw.mu.Unlock()
	
	if mfw.running {
		return
	}
	mfw.running = true

	for _, watcher := range mfw.watchers {
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
