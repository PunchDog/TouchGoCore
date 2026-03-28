package lua

import (
	"fmt"
	"os"
	"time"

	"touchgocore/vars"
)

// ScriptReloadCallback 热重载回调函数类型
type ScriptReloadCallback func(script *LuaScript, success bool, err error)

// ScriptWatcher 监控 Lua 脚本文件变化
type ScriptWatcher struct {
	scriptPath   string
	script       *LuaScript
	fileModTime  time.Time
	running      bool
	stopChan     chan struct{}
	dependencies map[string]time.Time // 依赖文件路径 -> 修改时间
	callbacks    []ScriptReloadCallback
	manager      *LuaManager          // 关联的 Lua 管理器（用于重新创建脚本）
}

// NewScriptWatcher 创建脚本监视器
func NewScriptWatcher(script *LuaScript) *ScriptWatcher {
	return &ScriptWatcher{
		scriptPath:   script.initScriptPath,
		script:       script,
		stopChan:     make(chan struct{}),
		dependencies: make(map[string]time.Time),
		callbacks:    make([]ScriptReloadCallback, 0),
		manager:      globalManager, // 默认使用全局管理器
	}
}

// AddDependency 添加依赖文件
func (sw *ScriptWatcher) AddDependency(depPath string) {
	sw.dependencies[depPath] = time.Time{}
	// 初始化依赖文件的修改时间
	if info, err := os.Stat(depPath); err == nil {
		sw.dependencies[depPath] = info.ModTime()
	}
}

// AddCallback 添加热重载回调
func (sw *ScriptWatcher) AddCallback(callback ScriptReloadCallback) {
	sw.callbacks = append(sw.callbacks, callback)
}

// Start 开始监控脚本变化
func (sw *ScriptWatcher) Start() {
	if sw.running {
		return
	}

	sw.running = true

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

	// 启动监控 goroutine
	go sw.watch()
	vars.Info("开始监控 Lua 脚本: %s", sw.scriptPath)
	if len(sw.dependencies) > 0 {
		vars.Info("监控 %d 个依赖文件", len(sw.dependencies))
	}
}

// Stop 停止监控
func (sw *ScriptWatcher) Stop() {
	if !sw.running {
		return
	}

	sw.running = false
	close(sw.stopChan)
	vars.Info("停止监控 Lua 脚本")
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
	for depPath, modTime := range sw.dependencies {
		depInfo, err := os.Stat(depPath)
		if err != nil {
			// 依赖文件不存在，视为变化
			return true
		}

		if depInfo.ModTime().After(modTime) {
			return true
		}
	}

	return false
}

// reloadScript 重新加载脚本
func (sw *ScriptWatcher) reloadScript() {
	vars.Info("检测到 Lua 脚本修改，准备重新加载...")

	// 备份当前状态
	oldScript := sw.script
	registeredObjectsCopy := oldScript.registeredObjects
	oldUID := oldScript.UID

	// 停止旧脚本
	oldScript.Close()

	// 使用管理器重新创建脚本（保持相同的 UID）
	var newScript *LuaScript
	var err error
	
	if sw.manager != nil {
		// 通过管理器重新创建
		newScript, err = sw.manager.NewScript(sw.scriptPath)
	} else {
		// 回退到旧方式（不推荐）
		newScript, err = NewLuaScript(sw.scriptPath)
	}
	
	if err != nil {
		vars.Error("重新加载 Lua 脚本失败: %v", err)
		
		// 尝试恢复旧脚本（有限恢复）
		if oldScript.state == nil {
			oldScript.Init()
			// 注意：这里无法恢复函数/类注册，需要调用者处理回滚
		}
		sw.script = oldScript

		// 触发回调
		for _, callback := range sw.callbacks {
			callback(oldScript, false, err)
		}
		return
	}

	// 恢复注册的对象（复制到新脚本）
	registeredObjectsCopy.Range(func(key, value interface{}) bool {
		newScript.registeredObjects.Store(key, value)
		return true
	})

	// 保持相同的 UID（如果可能）
	// 注意：新脚本有自己的 UID，但我们可以更新引用
	sw.script = newScript

	// 更新修改时间
	if info, err := os.Stat(sw.scriptPath); err == nil {
		sw.fileModTime = info.ModTime()
	}

	// 更新依赖文件的修改时间
	for depPath := range sw.dependencies {
		if info, err := os.Stat(depPath); err == nil {
			sw.dependencies[depPath] = info.ModTime()
		}
	}

	vars.Info("Lua 脚本重新加载成功 (旧UID: %d, 新UID: %d)", oldUID, newScript.UID)

	// 触发回调
	for _, callback := range sw.callbacks {
		callback(newScript, true, nil)
	}
}

// GetScript 获取当前脚本
func (sw *ScriptWatcher) GetScript() *LuaScript {
	return sw.script
}

// EnableHotReload 启用热重载功能
func EnableHotReload(script *LuaScript) *ScriptWatcher {
	watcher := NewScriptWatcher(script)
	watcher.Start()
	return watcher
}

// DisableHotReload 禁用热重载
func DisableHotReload(watcher *ScriptWatcher) {
	if watcher != nil {
		watcher.Stop()
	}
}

// ReloadScript 手动重新加载脚本
func (ls *LuaScript) ReloadScript() error {
	vars.Info("手动重新加载 Lua 脚本: %s", ls.initScriptPath)

	// 保存注册的对象
	registeredObjectsCopy := ls.registeredObjects
	oldUID := ls.UID

	// 关闭旧状态
	ls.Close()

	// 重新初始化
	ls.Init()

	// 重新注册函数（从管理器获取）
	if ls.manager != nil {
		// 使用管理器重新注册函数和类
		// 注意：这里需要访问管理器的内部状态，暂时简化处理
		// 实际应该通过管理器重新创建整个脚本
		vars.Warning("ReloadScript 使用旧模式，建议使用 ScriptWatcher 进行热重载")
	} else {
		// 回退到旧的全局变量方式
		for funcName, function := range registeredFuncs {
			ls.state.Register(funcName, function)
		}

		for class := range registeredClasses {
			if err := registerClass(class, ls); err != nil {
				vars.Error("注册 Lua 类失败: %v", err)
			}
		}
	}

	// 加载脚本
	if err := ls.state.DoFile(ls.initScriptPath); err != nil {
		return fmt.Errorf("加载 Lua 脚本失败: %w", err)
  }

	// 恢复注册的对象
	registeredObjectsCopy.Range(func(key, value interface{}) bool {
		ls.registeredObjects.Store(key, value)
		return true
	})

	vars.Info("Lua 脚本重新加载成功 (UID: %d)", oldUID)
	return nil
}

// WatchMultipleFiles 监控多个 Lua 文件
type MultiFileWatcher struct {
	watchers map[string]*ScriptWatcher
	running  bool
	stopChan chan struct{}
}

// NewMultiFileWatcher 创建多文件监视器
func NewMultiFileWatcher() *MultiFileWatcher {
	return &MultiFileWatcher{
		watchers: make(map[string]*ScriptWatcher),
		stopChan: make(chan struct{}),
	}
}

// AddScript 添加要监控的脚本
func (mfw *MultiFileWatcher) AddScript(script *LuaScript) {
	watcher := NewScriptWatcher(script)
	mfw.watchers[script.initScriptPath] = watcher
}

// Start 开始监控
func (mfw *MultiFileWatcher) Start() {
	if mfw.running {
		return
	}

	mfw.running = true
	for _, watcher := range mfw.watchers {
		watcher.Start()
	}
	vars.Info("开始监控 %d 个 Lua 脚本文件", len(mfw.watchers))
}

// Stop 停止监控
func (mfw *MultiFileWatcher) Stop() {
	if !mfw.running {
		return
	}

	mfw.running = false
	for _, watcher := range mfw.watchers {
		watcher.Stop()
	}
	vars.Info("停止监控 Lua 脚本文件")
}

// ReloadAll 重新加载所有脚本
func (mfw *MultiFileWatcher) ReloadAll() error {
	for _, watcher := range mfw.watchers {
		if err := watcher.script.ReloadScript(); err != nil {
			return err
		}
	}
	return nil
}

// GetScriptByPath 根据路径获取脚本
func (mfw *MultiFileWatcher) GetScriptByPath(path string) (*LuaScript, bool) {
	watcher, ok := mfw.watchers[path]
	if !ok {
		return nil, false
	}
	return watcher.GetScript(), true
}
