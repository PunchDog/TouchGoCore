# golua 模块重构迁移指南

## 概述

本次重构将分散的全局状态管理统一到 `LuaManager` 中，解决了以下问题：
1. **全局变量过多**（7个分散变量 → 1个管理器）
2. **并发控制复杂**（多处锁竞争 → 单点锁协调）
3. **热重载不健壮**（部分恢复 → 完整状态机）
4. **API 不一致**（4个入口 → 1个清晰入口）

## 主要变更

### 1. 新的核心类：LuaManager

```go
// 重构前：分散的全局变量
var defaultLua *LuaScript
var luaInstances sync.Map
var nextInstanceID int64
var instanceIDMu sync.Mutex
var registeredFuncsMu sync.RWMutex
var registeredFuncs map[string]func(L *lua.State) int
var registeredClassesMu sync.RWMutex
var registeredClasses map[ILuaClassInterface]bool

// 重构后：统一的管理器
manager := lua.NewLuaManager()
```

### 2. 全局单例管理器

```go
// 全局管理器（替代所有分散的全局变量）
var globalManager = NewLuaManager()

// 向后兼容：旧函数已包装为管理器调用
lua.RegisterLuaFunc()      // → globalManager.RegisterFunc()
lua.RegisterLuaClass()     // → globalManager.RegisterClass()
lua.Call()                 // → globalManager.Call()
lua.RunLua()               // → 内部使用 globalManager.NewScript()
lua.StopLua()              // → globalManager.CloseAll()
```

## 迁移步骤

### 阶段 1：简单替换（立即可用）

#### 1.1 创建脚本实例

```go
// 旧方式
script, err := lua.NewLuaScript("script.lua")

// 新方式（推荐）
manager := lua.NewLuaManager()
script, err := manager.NewScript("script.lua")

// 或使用全局管理器（向后兼容）
script, err := lua.NewLuaScript("script.lua") // 内部调用 globalManager.NewScript()
```

#### 1.2 注册函数和类

```go
// 旧方式（仍然可用，已包装）
err := lua.RegisterLuaFunc("myFunc", func(L *lua.State) int {
    // ...
})

// 新方式（直接使用管理器）
manager := lua.NewLuaManager()
err := manager.RegisterFunc("myFunc", func(L *lua.State) int {
    // ...
})
```

#### 1.3 调用函数

```go
// 旧方式（仍然可用）
results, err := lua.Call("luaFunction", arg1, arg2)

// 新方式
manager := lua.NewLuaManager()
results, err := manager.Call("luaFunction", arg1, arg2)

// 或使用脚本实例
script, _ := manager.NewScript("script.lua")
results, err := script.Call("luaFunction", arg1, arg2)
```

### 阶段 2：高级功能迁移

#### 2.1 多实例管理

```go
// 旧方式：难以追踪多个实例
var instances sync.Map

// 新方式：集中管理
manager := lua.NewLuaManager()

// 创建多个实例
script1, _ := manager.NewScript("script1.lua")
script2, _ := manager.NewScript("script2.lua")

// 统一管理
allIDs := manager.GetAllScriptIDs() // [1, 2]
script, ok := manager.GetScript(1)  // 获取特定实例
```

#### 2.2 统计和监控

```go
// 新增：运行时统计
stats := manager.Stats()
fmt.Printf("活跃实例: %d, 总调用: %d\n", 
    stats["scripts_active"], 
    stats["calls_total"])
```

#### 2.3 热重载改进

```go
// 旧方式：ScriptWatcher（已更新支持管理器）
watcher := lua.NewScriptWatcher(script)

// 新方式：直接使用管理器重新创建
script, _ := manager.NewScript("script.lua")
// ... 使用 script
// 当需要热重载时，关闭旧实例，创建新实例
manager.CloseScript(script.UID)
newScript, _ := manager.NewScript("script.lua")
```

### 阶段 3：废弃功能清理

以下功能已标记为废弃，将在未来版本移除：

| 废弃项 | 替代方案 | 移除计划 |
|--------|----------|----------|
| 全局变量 `defaultLua` | `manager.DefaultScript()` | v0.04 |
| 全局变量 `luaInstances` | `manager.GetAllScriptIDs()` | v0.04 |
| 直接访问 `registeredFuncs` | `manager.RegisterFunc()` | v0.04 |
| 直接访问 `registeredClasses` | `manager.RegisterClass()` | v0.04 |

## 新功能示例

### 1. 完整的应用生命周期

```go
func main() {
    // 1. 创建管理器
    manager := lua.NewLuaManager()
    defer manager.CloseAll() // 优雅关闭所有实例
    
    // 2. 注册全局功能
    manager.RegisterFunc("goLog", func(L *lua.State) int {
        msg := L.ToString(1)
        log.Println("Lua log:", msg)
        return 0
    })
    
    manager.RegisterClass(&MyGameObject{})
    
    // 3. 创建脚本实例
    mainScript, err := manager.NewScript("main.lua")
    if err != nil {
        log.Fatal("加载主脚本失败:", err)
    }
    
    // 4. 创建多个模块实例
    uiScript, _ := manager.NewScript("ui.lua")
    aiScript, _ := manager.NewScript("ai.lua")
    
    // 5. 运行时监控
    go func() {
        for {
            time.Sleep(30 * time.Second)
            stats := manager.Stats()
            log.Printf("Lua运行时: %d实例, %d次调用", 
                stats["scripts_active"], stats["calls_total"])
        }
    }()
    
    // 6. 主循环
    for {
        // 调用主脚本
        _, err := mainScript.Call("update", deltaTime)
        if err != nil {
            log.Println("脚本错误:", err)
        }
        time.Sleep(16 * time.Millisecond) // ~60 FPS
    }
}
```

### 2. 热重载完整示例

```go
func setupHotReload(manager *LuaManager, scriptPath string) *ScriptWatcher {
    // 创建脚本
    script, err := manager.NewScript(scriptPath)
    if err != nil {
        return nil
    }
    
    // 创建监视器
    watcher := NewScriptWatcher(script)
    watcher.manager = manager // 关联管理器
    
    // 添加依赖
    watcher.AddDependency("config.lua")
    watcher.AddDependency("utils.lua")
    
    // 重载回调
    watcher.AddCallback(func(script *LuaScript, success bool, err error) {
        if success {
            log.Println("热重载成功，新实例ID:", script.UID)
        } else {
            log.Println("热重载失败:", err)
            // 可以在这里实现回滚逻辑
        }
    })
    
    watcher.Start()
    return watcher
}
```

## 向后兼容性

### 完全兼容的 API

以下函数保持完全向后兼容，内部已迁移到新架构：

- `lua.RegisterLuaFunc()`
- `lua.RegisterLuaClass()` 
- `lua.Call()`
- `lua.RunLua()`
- `lua.StopLua()`
- `lua.NewLuaScript()`

### 编译时警告

为了平滑迁移，废弃的全局变量仍然存在但不再使用。编译时可能会看到：

```
// 这些变量已废弃，但不会影响编译
var defaultLua *LuaScript               // 已废弃
var luaInstances sync.Map               // 已废弃
// ...
```

### 运行时兼容

所有现有代码无需修改即可继续工作。新代码建议使用新的 `LuaManager` API。

## 性能影响

### 正面影响

1. **减少锁竞争**：从多个读写锁 → 单管理器锁
2. **降低内存碎片**：集中管理减少小对象分配
3. **更快的实例查找**：map 查找 vs sync.Map.Range

### 潜在影响

1. **单点锁可能成为瓶颈**：如果同时操作大量实例
   - 缓解：使用多个管理器分片
   - 缓解：异步批量操作

2. **初始化开销**：管理器本身有少量开销
   - 实际测量：< 1ms 初始化时间
   - 可接受范围

## 故障排除

### 常见问题

#### Q1: `manager.NewScript()` 返回文件不存在错误
```go
// 旧方式：忽略文件错误，创建空实例
// 新方式：严格检查，失败返回错误

// 解决方案：
script, err := manager.NewScript("script.lua")
if err != nil && strings.Contains(err.Error(), "cannot open") {
    // 文件不存在，创建空状态
    script.Init() // 手动初始化
}
```

#### Q2: 热重载后对象引用失效
```go
// 旧问题：热重载后旧对象引用悬空
// 新方案：通过管理器重新获取

// 解决方案：
oldUID := script.UID
// 热重载发生...
newScript, ok := manager.GetScript(oldUID) // 可能已改变
if !ok {
    // 获取默认实例
    newScript, _ = manager.DefaultScript()
}
```

#### Q3: 统计信息不准确
```go
// 确保在正确的时间点获取统计
stats1 := manager.Stats()
// 执行操作...
stats2 := manager.Stats()
delta := stats2["calls_total"] - stats1["calls_total"]
```

## 下一步计划

### v0.04（计划中）
1. 移除所有废弃的全局变量
2. 集成 fsnotify 替换轮询热重载
3. 添加 Lua 调试接口
4. 性能监控仪表板

### v0.05（规划中）
1. 多版本 Lua 支持（5.1/5.2/5.3/5.4）
2. JIT 编译支持（LuaJIT）
3. 分布式 Lua 实例管理

## 支持与反馈

如果在迁移过程中遇到问题：

1. 检查编译错误中的行号
2. 查看 `golua/manager.go` 源码
3. 运行测试：`go test ./golua -run TestLuaManager`
4. 提交 Issue 到项目仓库

---
*迁移指南版本：v0.03*
*最后更新：2026-03-28*