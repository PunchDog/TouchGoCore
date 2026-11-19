# agents.md — golua

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L5
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

内嵌 Lua VM 与 Go 互操作层：脚本加载/热更、Go 函数注册、Go↔Lua 值转换、Lua 类与 Table 扩展、定时器与文件监听自动重载。目录名 golua 但声明 `package lua`，引用方写 `lua "touchgocore/golua"`。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `lua_facade.go` | 对外主入口 | 中 |
| `lua_call.go` | 执行一段脚本 | 中 |
| `lua_script.go` | 执行一段脚本 | 中 |
| `lua_gofunc.go` | 注册 Go 函数/类型 | 中 |
| `lua_register.go` | 注册 Go 函数/类型 | 中 |
| `lua_convert.go` | Go↔Lua 类型转换 | 中 |
| `lua_class.go` | Lua 类与 Table | 中 |
| `lua_table.go` | Lua 类与 Table | 中 |
| `lua_config.go` | 配置、安全、错误 | 高 |
| `lua_safe.go` | 配置、安全、错误 | 中 |
| `lua_reload.go` | 热重载与定时器 | 中 |
| `lua_watcher.go` | 热重载与定时器 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `lua_table.go` | 类型 LuaTable；函数 HasData/Append/Set/ToTable/ToTableWithContext/PushTable/SubTable/Length | 中 |
| `lua_table_ext.go` | 类型 KeyType；函数 GetByPath/SetByPath/GetInt64/GetFloat64/GetBool/HasKey/Delete/ForEach | 中 |
| `lua_convert.go` | 函数 GoToLuaValue/GoToLuaValueWithContext/GoToLuaValueErr/LuaToGoValue/LuaToGoValueWithContext/LuaToReflectValue/LuaToReflectValueWithContext | 中 |
| `lua_watcher.go` | 类型 ScriptReloadCallback/ScriptReloadCallbackOld/ScriptWatcher/MultiFileWatcher；函数 NewScriptWatcher/NewScriptWatcherWithContext/AddDependency/AddCallback/AddCallbackWithContext/Start/Stop/GetScript | 中 |
| `lua_script.go` | 类型 LuaScript；函数 Init/Close/IsPoisoned/RangeRegisteredObjects/NewLuaScript/NewLuaScriptWithContext | 中 |
| `lua_class.go` | 类型 ILuaClassInterface/ILuaClassObject/ClassRegistry；函数 Delete/Update | 中 |
| `lua_gofunc.go` | 函数 RegisterGoFunc | 中 |
| `lua_reload.go` | 函数 ReloadScript/ReloadScriptWithContext | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`LuaTable`、`HasData`、`Append`、`Set`、`ToTable`、`ToTableWithContext`、`PushTable`、`SubTable`、`Length`、`Get`、`GetInt`、`GetString`、`GetTable`、`GetArray`、`Keys`、`Values`、`ToMap`、`Merge` 等
- **允许依赖**：`config`、`corectx`、`localtimer`、`metrics`、`syncmap`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`corectx`、`localtimer`、`metrics`、`syncmap`、`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`mapmanager`

## 5. 禁止跨模块直连约束

- 目录名与包名不一致是刻意的（改名属破坏性变更）；包内文件统一为 lua_*.go 蛇形命名。
- 跨层依赖只向 `config`、`corectx`、`localtimer`、`metrics`、`syncmap`、`util` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`（框架总说明）
- `docs/repo-layout.md`（依赖分层与包清单）
- 包内 `doc.go` 与文件头注释

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
