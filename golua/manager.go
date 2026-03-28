package lua

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/aarzilli/golua/lua"
	"touchgocore/localtimer"
	"touchgocore/syncmap"
	"touchgocore/util"
	"touchgocore/vars"
)

// LuaManager 统一管理所有 Lua 状态和资源
type LuaManager struct {
	mu sync.RWMutex // 保护所有内部状态

	// 实例管理
	instances map[int64]*LuaScript // 活跃的 Lua 实例
	defaultID int64                // 默认实例 ID（-1 表示无默认实例）
	nextID    int64                // 下一个实例 ID（原子递增）

	// 注册表
	funcs   map[string]func(L *lua.State) int // 全局函数注册表
	classes map[ILuaClassInterface]bool       // 已注册的类接口

	// 统计信息
	stats struct {
		scriptsCreated atomic.Int64
		scriptsClosed  atomic.Int64
		callsMade      atomic.Int64
	}
}

// NewLuaManager 创建新的 Lua 管理器
func NewLuaManager() *LuaManager {
	return &LuaManager{
		instances: make(map[int64]*LuaScript),
		funcs:     make(map[string]func(L *lua.State) int),
		classes:   make(map[ILuaClassInterface]bool),
		defaultID: -1,
		nextID:    1, // 从 1 开始
	}
}

// NewScript 创建新的 Lua 脚本实例
func (m *LuaManager) NewScript(initScriptPath string) (*LuaScript, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 分配唯一 ID
	id := m.nextID
	m.nextID++

	// 创建脚本实例（内部构造函数）
	script, err := m.newScriptInternal(id, initScriptPath)
	if err != nil {
		return nil, err
	}

	// 注册到管理器
	m.instances[id] = script

	// 如果是第一个实例，设为默认实例
	if m.defaultID == -1 {
		m.defaultID = id
	}

	m.stats.scriptsCreated.Add(1)
	return script, nil
}

// newScriptInternal 内部创建脚本实例
func (m *LuaManager) newScriptInternal(id int64, initScriptPath string) (*LuaScript, error) {
	p := &LuaScript{
		state:             nil,
		returnValues:      make([]interface{}, 0),
		initScriptPath:    initScriptPath,
		registeredObjects: &syncmap.Map{},
		nextObjectID:      0,
		UID:               id,
		manager:           m, // 反向引用管理器
	}
	p.Init()

	// 注册全局函数（从管理器获取）
	for funcName, function := range m.funcs {
		p.state.Register(funcName, function)
	}

	// 注册类（从管理器获取）
	for class := range m.classes {
		if err := registerClass(class, p); err != nil {
			vars.Error("注册 Lua 类失败: %v", err)
		}
	}

	// 加载脚本文件
	if err := p.state.DoFile(p.initScriptPath); err != nil {
		return nil, fmt.Errorf("加载 Lua 脚本失败: %w", err)
	}

	// 创建定时器
	timerImpl := &luaTimer{}
	tmr, err := localtimer.NewTimer(UpdateIntervalMs, -1, timerImpl)
	if err != nil {
		return nil, fmt.Errorf("创建定时器失败: %w", err)
	}
	p.timer = tmr.(*luaTimer)
	p.timer.luaScript = p
	p.timer.tick = 0
	localtimer.AddTimer(p.timer)

	return p, nil
}

// GetScript 获取指定 ID 的脚本实例
func (m *LuaManager) GetScript(id int64) (*LuaScript, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	script, ok := m.instances[id]
	return script, ok
}

// DefaultScript 获取默认脚本实例
func (m *LuaManager) DefaultScript() (*LuaScript, error) {
	m.mu.RLock()
	id := m.defaultID
	m.mu.RUnlock()

	if id == -1 {
		return nil, fmt.Errorf("没有可用的默认 Lua 实例")
	}

	script, ok := m.GetScript(id)
	if !ok {
		return nil, fmt.Errorf("默认 Lua 实例不存在")
	}
	return script, nil
}

// CloseScript 关闭指定的脚本实例
func (m *LuaManager) CloseScript(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	script, ok := m.instances[id]
	if !ok {
		return fmt.Errorf("Lua 实例 %d 不存在", id)
	}

	// 关闭脚本
	script.Close()

	// 从管理器中移除
	delete(m.instances, id)

	// 如果关闭的是默认实例，清空默认 ID
	if id == m.defaultID {
		m.defaultID = -1
	}

	m.stats.scriptsClosed.Add(1)
	return nil
}

// RegisterFunc 注册全局函数到所有 Lua 实例
func (m *LuaManager) RegisterFunc(funcName string, function func(L *lua.State) int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查是否已存在
	if _, ok := m.funcs[funcName]; ok {
		return fmt.Errorf("函数 %s 已注册", funcName)
	}

	// 注册到管理器
	m.funcs[funcName] = function

	// 立即应用到所有活跃实例
	for _, script := range m.instances {
		if script.state != nil {
			script.state.Register(funcName, function)
		}
	}

	return nil
}

// RegisterClass 注册一个类到所有 Lua 实例
func (m *LuaManager) RegisterClass(class ILuaClassInterface) error {
	className, _ := util.GetClassName(class)
	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查是否已存在
	if _, ok := m.classes[class]; ok {
		return fmt.Errorf("类 %s 已注册", className)
	}

	// 注册到管理器
	m.classes[class] = true

	// 立即应用到所有活跃实例
	for _, script := range m.instances {
		if script.state != nil {
			if err := registerClass(class, script); err != nil {
				vars.Error("注册 Lua 类失败: %v", err)
			}
		}
	}

	return nil
}

// Call 调用默认实例的函数（向后兼容的快捷方式）
func (m *LuaManager) Call(funcName string, args ...interface{}) ([]interface{}, error) {
	script, err := m.DefaultScript()
	if err != nil {
		return nil, err
	}
	m.stats.callsMade.Add(1)
	return script.Call(funcName, args...)
}

// CloseAll 关闭所有 Lua 实例
func (m *LuaManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, script := range m.instances {
		script.Close()
		delete(m.instances, id)
	}

	// 清空注册表（可选，取决于是否允许重启）
	m.funcs = make(map[string]func(L *lua.State) int)
	m.classes = make(map[ILuaClassInterface]bool)
	m.defaultID = -1
	m.nextID = 1
}

// Stats 返回统计信息
func (m *LuaManager) Stats() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]int64{
		"scripts_total":   m.stats.scriptsCreated.Load(),
		"scripts_closed":  m.stats.scriptsClosed.Load(),
		"scripts_active":  int64(len(m.instances)),
		"calls_total":     m.stats.callsMade.Load(),
		"funcs_registered": int64(len(m.funcs)),
		"classes_registered": int64(len(m.classes)),
	}
}

// GetAllScriptIDs 返回所有活跃实例的 ID
func (m *LuaManager) GetAllScriptIDs() []int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]int64, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	return ids
}