# golua - Go 与 Lua 脚本集成模块

## 概述

`golua` 是一个功能完整的 Go 与 Lua 脚本集成模块，基于 `github.com/arnodel/golua` 封装，提供了强大的双向交互能力。

**重要更新**：本模块已从 `github.com/aarzilli/golua`（C 绑定）迁移到 `github.com/arnodel/golua`（纯 Go 实现）。

## 核心特性

1. **无 CGO 依赖**
   - 纯 Go 实现，无需安装 Lua C 库
   - 跨平台编译更容易
   - 使用 Go 的垃圾回收机制

2. **Lua 虚拟机管理**
   - 多实例支持
   - 自动生命周期管理
   - 定时更新和垃圾回收

3. **类注册和方法调用**
   - 通过 `ILuaClassInterface` 接口注册 Go 类
   - 自动处理对象生命周期（Init/Delete/Update）
   - 反射调用方法，性能优化（缓存）

4. **类型转换系统**
   - Go → Lua: `GoToLuaValue`, `LuaToGoValue`
   - Lua → Go: `LuaToReflectValue`
   - 支持基本类型、table、自定义类型

5. **并发安全**
   - `SafeLuaScript` 提供并发安全的封装
   - 使用读写锁保护状态

6. **性能优化**
   - 反射缓存（`MethodCache`）- 支持懒加载和预计算
   - 对象池（`LuaTablePool`）- 高效复用 LuaTable 对象
   - 减少重复反射开销

7. **热重载功能**
   - 监控脚本文件变化
   - 支持依赖文件监控
   - 热重载回调机制

8. **增强的 LuaTable 操作**
   - 支持路径访问（`GetByPath`, `SetByPath`）
   - 支持方括号语法（如 `player['name']`）
   - 丰富的表操作方法（`Filter`, `Map`, `Copy` 等）

## 迁移说明

### 从 aarzilli/golua 到 arnodel/golua 的主要变化

#### 1. 类型系统

**旧版本 (aarzilli/golua):**
```go
import "github.com/aarzilli/golua/lua"

L := lua.NewState()
L.PushString("hello")
str := L.ToString(1)
```

**新版本 (arnodel/golua):**
```go
import rt "github.com/arnodel/golua/runtime"

r := rt.New(os.Stdout)
value := rt.StringValue("hello")
str := value.AsString()
```

#### 2. 函数注册

**旧版本:**
```go
func myFunction(L *lua.State) int {
    arg := L.ToString(1)
    L.PushString("result")
    return 1
}

L.Register("myFunc", myFunction)
```

**新版本:**
```go
func myFunction(t *rt.Thread, c *rt.GoCont) (rt.Cont, error) {
    arg, _ := c.StringArg(0)
    next := c.Next()
    t.Push1(next, rt.StringValue("result"))
    return next, nil
}

r.SetEnvGoFunc(r.GlobalEnv(), "myFunc", myFunction, 1, false)
```

#### 3. Lua 脚本调用

**旧版本:**
```go
L.GetGlobal("functionName")
L.PushString("arg")
L.Call(1, 1)
result := L.ToString(-1)
```

**新版本:**
```go
funcVal := r.GetEnv(r.GlobalEnv(), rt.StringValue("functionName"))
result, err := rt.Call1(r.MainThread(), funcVal, rt.StringValue("arg"))
```

### 向后兼容性

本模块保持了对外 API 的向后兼容性，从 Lua 脚本的角度看，API 保持一致。所有变更都在 Go 代码内部。

## 文件结构

| 文件 | 说明 |
|------|------|
| `luamain.go` | LuaScript 结构体、全局管理、启动/停止 |
| `luaClass.go` | 类注册、反射调用、userdata 处理 |
| `luatable.go` | LuaTable 类型、table 转换 |
| `convert.go` | 统一类型转换函数 |
| `cache.go` | 反射和方法缓存 |
| `pool.go` | LuaTable 对象池 |
| `safe.go` | 并发安全封装 |
| `consts.go` | 常量定义 |
| `luadefaultfunction.go` | 内置 Lua 函数 |
| `run.go` | 启动/停止接口 |
| `extended.go` | LuaTable 扩展方法 |
| `table_api.go` | LuaTable 基础 API |
| `reload.go` | 热重载功能 |

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

### 全局函数

#### RegisterLuaFunc
```go
func RegisterLuaFunc(funcName string, function func(t *rt.Thread, c *rt.GoCont) (rt.Cont, error)) error
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

## 类型转换 API

### Go 到 Lua
```go
func GoToLuaValue(val interface{}) rt.Value
```
将 Go 值转换为 arnodel/golua 的 rt.Value。

### Lua 到 Go
```go
func LuaToGoValue(v rt.Value) interface{}
func LuaToReflectValue(v rt.Value, targetType reflect.Type) (reflect.Value, error)
```
将 Lua 值转换为 Go 值。

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

## 注意事项

1. **无 CGO 依赖**: 本模块基于 `github.com/arnodel/golua`，是纯 Go 实现
   - 不需要安装 Lua C 库
   - 跨平台编译更容易
   - 使用 Go 的垃圾回收

2. **编译命令**:
   ```bash
   go build
   ```
   不再需要设置 CGO_ENABLED 或指定 Lua 版本标签。

3. **Table Key**: Lua table 的 key 目前只支持 string 类型

4. **定时更新**: 每 1 秒触发一次注册对象的 `Update()` 方法

5. **垃圾回收**: 使用 Go 的 GC，定期触发清理

6. **并发安全**: 在多 goroutine 环境下使用 `SafeLuaScript`

## Lua 版本

本模块使用 `github.com/arnodel/golua`，实现的是 **Lua 5.5** 版本。

## 示例项目

参见 `example/` 目录获取完整的使用示例。

## 迁移指南

如果您从旧版本（aarzilli/golua）迁移，请注意：

1. ✅ Lua 脚本无需修改
2. ✅ Go 类接口保持不变
3. ✅ 对外 API 保持兼容
4. ⚠️ 内部实现完全改变
5. ✅ 编译更简单（无需 CGO）

详细迁移指南请参阅 `golua_migration_plan.md`。

## 许可证

本模块遵循 TouchGoCore 项目的许可证。
