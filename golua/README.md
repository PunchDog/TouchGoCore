# golua - Go 与 Lua 脚本集成模块

## 概述

`golua` 是一个功能完整的 Go 与 Lua 脚本集成模块，基于 `github.com/aarzilli/golua/lua` 封装，提供了强大的双向交互能力。

## 核心特性

1. **Lua 虚拟机管理**
   - 多实例支持
   - 自动生命周期管理
   - 定时更新和垃圾回收

2. **类注册和方法调用**
   - 通过 `ILuaClassInterface` 接口注册 Go 类
   - 自动处理对象生命周期（Init/Delete/Update）
   - 反射调用方法，性能优化（缓存）

3. **类型转换系统**
   - Go → Lua: `PushValue`, `LuaToGoValue`
   - Lua → Go: `LuaToGoReflectValue`
   - 支持基本类型、table、自定义类型

4. **并发安全**
   - `SafeLuaScript` 提供并发安全的封装
   - 使用读写锁保护状态

5. **性能优化**
   - 反射缓存（`MethodCache`）- 支持懒加载和预计算
   - 对象池（`LuaTablePool`）- 高效复用 LuaTable 对象
   - 减少重复反射开销

6. **热重载功能**
   - 监控脚本文件变化
   - 支持依赖文件监控
   - 热重载回调机制

7. **增强的 LuaTable 操作**
   - 支持路径访问（`GetByPath`, `SetByPath`）
   - 支持方括号语法（如 `player['name']`）
   - 丰富的表操作方法（`Filter`, `Map`, `Copy` 等）

## 文件结构

| 文件 | 说明 |
|------|------|
| `luamain.go` | LuaScript 结构体、全局管理、启动/停止 |
| **`manager.go`** | **新增：统一状态管理器（推荐使用）** |
| `luaClass.go` | 类注册、反射调用、userdata 处理 |
| `luatable.go` | LuaTable 类型、table 转换 |
| `convert.go` | 统一类型转换函数 |
| `cache.go` | 反射和方法缓存 |
| `pool.go` | LuaTable 对象池 |
| `safe.go` | 并发安全封装 |
| `consts.go` | 常量定义 |
| `luadefaultfunction.go` | 内置 Lua 函数 |
| `run.go` | 启动/停止接口 |
| `doc.go` | 模块文档 |

## 快速开始

### 1. 定义 Go 类

```go
package main

import (
    "touchgocore/golua"
)

type Player struct {
    lua.ILuaClassObject
    id     int64
    name   string
    health int
}

func (p *Player) Init(id int64, script *lua.LuaScript) {
    p.id = id
    p.name = "Player"
    p.health = 100
}

func (p *Player) GetName() string {
    return p.name
}

func (p *Player) TakeDamage(damage int) {
    p.health -= damage
}

func (p *Player) GetHealth() int {
    return p.health
}

func (p *Player) Delete() {
    // 清理资源
}
```

### 2. 注册类

```go
func init() {
    lua.RegisterLuaClass(&Player{})
}
```

### 3. 启动 Lua 服务

```go
func main() {
    lua.RunLua()
}
```

### 4. Lua 脚本使用

```lua
-- 创建玩家对象
local player = Player()

-- 调用 Go 方法
print("Player name:", player:GetName())

-- 修改属性
player:TakeDamage(20)
print("Player health:", player:GetHealth())
```

## 高级功能

### 1. 热重载功能

```go
// 启用热重载
script, err := lua.NewLuaScript("script.lua")
if err != nil {
    log.Fatal(err)
}

// 启用热重载
watcher := lua.EnableHotReload(script)

// 添加依赖文件监控
watcher.AddDependency("config.lua")
watcher.AddDependency("utils.lua")

// 添加热重载回调
watcher.AddCallback(func(script *lua.LuaScript, success bool, err error) {
    if success {
        fmt.Println("脚本热重载成功")
    } else {
        fmt.Printf("脚本热重载失败: %v\n", err)
    }
})

// 关闭时禁用热重载
defer lua.DisableHotReload(watcher)
```

### 2. 增强的 LuaTable 操作

```go
// 创建 LuaTable
tbl := lua.NewLuaTablePooled(nil)
defer lua.PutLuaTable(tbl)

// 设置值
tbl.Set("name", "Test")
tbl.Set("age", 25)

// 使用路径访问
tbl.SetByPath("stats.health", 100)
tbl.SetByPath("stats.mana", 50)

// 获取值
if health, ok := tbl.GetByPath("stats.health"); ok {
    fmt.Println("Health:", health)
}

// 支持方括号语法的路径
tbl.SetByPath("players[1].name", "Alice")
tbl.SetByPath("players[2].name", "Bob")

// 表操作
filtered := tbl.Filter(func(key, value interface{}) bool {
    if age, ok := value.(int); ok {
        return age > 18
    }
    return false
})

// 深拷贝
copied := tbl.Copy()
```

### 3. 性能优化

```go
// 使用对象池
tbl := lua.GetLuaTable()
defer lua.PutLuaTable(tbl)

// 预分配容量
tbl := lua.GetLuaTableWithCapacity(100)
defer lua.PutLuaTable(tbl)
```

## API 参考

### 🆕 推荐：LuaManager（统一状态管理）

```go
// 创建管理器（替代分散的全局变量）
manager := lua.NewLuaManager()

// 创建脚本实例
script, err := manager.NewScript("path.lua")

// 注册全局函数/类
manager.RegisterFunc("goFunc", func(L *lua.State) int { return 0 })
manager.RegisterClass(&MyClass{})

// 调用函数
results, err := manager.Call("luaFunc", args...)

// 统计信息
stats := manager.Stats()
fmt.Printf("活跃实例: %d, 总调用: %d\n", 
    stats["scripts_active"], stats["calls_total"])

// 关闭所有
manager.CloseAll()
```

### 全局函数（向后兼容）

#### RegisterLuaFunc
```go
func RegisterLuaFunc(funcName string, function func(L *lua.State) int) error
```
注册全局函数到所有 Lua 实例。

#### RegisterLuaClass
```go
func RegisterLuaClass(class ILuaClassInterface) error
```
注册一个类到所有 Lua 实例。

#### Call
```go
func Call(funcName string, args ...interface{}) ([]interface{}, error)
```
调用默认 Lua 实例的函数。

#### RunLua / StopLua
```go
func RunLua() error
func StopLua()
```
启动和停止 Lua 服务。

### LuaScript 方法

```go
func (ls *LuaScript) Call(funcName string, args ...interface{}) ([]interface{}, error)
func (ls *LuaScript) Close()
```

### 并发安全 API

```go
type SafeLuaScript struct { *LuaScript }

func NewSafeLuaScript(initScriptPath string) (*SafeLuaScript, error)
func (s *SafeLuaScript) Call(funcName string, args ...interface{}) ([]interface{}, error)
```

## 性能优化

### 反射缓存

使用 `MethodCache` 缓存类的方法反射信息，避免每次调用时重新反射：

```go
type MethodCache struct {
    Methods map[string]reflect.Method
}
```

### 对象池

使用 `sync.Pool` 复用 `LuaTable` 对象：

```go
tbl := GetLuaTable()
defer PutLuaTable(tbl)
```

## 错误处理

所有注册和调用函数现在返回 `error` 类型，便于错误处理：

```go
if err := lua.RegisterLuaClass(&MyClass{}); err != nil {
    log.Fatal("注册类失败:", err)
}

results, err := lua.Call("MyFunction", arg1, arg2)
if err != nil {
    log.Println("调用失败:", err)
} else {
    fmt.Println("结果:", results)
}
```

## 常量

```go
const (
    UpdateIntervalMs    = 1000  // 更新间隔（毫秒）
    GCCollectionMinutes = 30    // 垃圾回收间隔（分钟）
    GCTickCount        = 1800  // 垃圾回收 tick 计数
    LuaFileExt         = ".lua" // Lua 文件扩展名
)
```

## 🚀 新架构优势（v0.03+）

### 统一状态管理
- **旧问题**：7个分散的全局变量，难以追踪和调试
- **新方案**：`LuaManager` 单点管理所有状态
- **收益**：更好的并发控制、内存管理、监控能力

### 改进的热重载
- **旧问题**：失败恢复不完整，状态可能不一致
- **新方案**：与管理器集成，支持完整状态恢复
- **收益**：更可靠的热更新，减少生产环境事故

### 向后兼容
- 所有现有 API 完全兼容
- 渐进式迁移支持
- 详细的迁移指南：`MIGRATION.md`

## 注意事项

1. **CGO 依赖**: 本模块基于 `github.com/aarzilli/golua`，需要 CGO 支持
   - 必须启用 CGO：`set CGO_ENABLED=1` (Windows) 或 `export CGO_ENABLED=1` (Linux/Mac)
   - 需要安装 Lua C 库和编译器
   - Windows 上需要安装 MinGW-w64 和 Lua 静态库

2. **编译命令**:
   - **Windows**: `build_windows.bat` 或
     ```batch
     set CGO_ENABLED=1
     go build -tags "!lua52,!lua53,!lua54"
     ```
   - **Linux/Mac**:
     ```bash
     export CGO_ENABLED=1
     go build -tags "!lua52,!lua53,!lua54"
     ```
   - 指定 Lua 版本：使用 `-tags lua52`, `-tags lua53`, 或 `-tags lua54`

3. **Table Key**: Lua table 的 key 目前只支持 string 类型

4. **定时更新**: 每 1 秒触发一次注册对象的 `Update()` 方法

5. **垃圾回收**: 每 30 分钟执行一次 Lua 垃圾回收

6. **并发安全**: 在多 goroutine 环境下使用 `SafeLuaScript`

## 编译依赖安装

### Windows
1. 安装 MinGW-w64: https://www.mingw-w64.org/
2. 下载 Lua C 源码: https://www.lua.org/download.html
3. 编译 Lua 静态库或使用预编译版本
4. 确保 MinGW 和 Lua 的 bin 目录在 PATH 环境变量中

### Linux
```bash
# Ubuntu/Debian
sudo apt-get install build-essential liblua5.1-dev

# CentOS/RHEL
sudo yum install gcc lua-devel

# 或者使用 Lua 5.2/5.3/5.4
sudo apt-get install liblua5.4-dev  # 或 liblua5.3-dev, liblua5.4-dev
```

### macOS
```bash
brew install lua
```

## 编译问题排查

如果遇到编译错误，请检查：
1. CGO 是否启用 (`go env CGO_ENABLED` 应该是 `1`)
2. 是否安装了 GCC 编译器 (`gcc --version`)
3. Lua C 库是否正确安装 (`pkg-config --cflags lua5.1` 或类似)
4. 构建标签是否正确

详细错误排查请参见 `ERRORS.md` 文件。

## 示例项目

参见 `example/` 目录获取完整的使用示例。

## 许可证

本模块遵循 TouchGoCore 项目的许可证。
