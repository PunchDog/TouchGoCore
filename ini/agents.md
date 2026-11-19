# agents.md — ini

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

零依赖的 INI 文件解析器，配置加载链的最底层。被 config 引用，不得反向依赖业务包。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `ini.go` | 解析一个文件 | 中 |
| `GetString / GetInt32 / GetUint32 / GetInt64 / GetUint64 / GetFloat32 / GetFloat64 / GetAll` | 取值 | 中 |
| `IniParserError` | 解析错误 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `ini.go` | 类型 IniParser/IniParserError；函数 Error/Load/GetString/GetInt32/GetUint32/GetInt64/GetUint64/GetFloat32 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`IniParser`、`IniParserError`、`Error`、`Load`、`GetString`、`GetInt32`、`GetUint32`、`GetInt64`、`GetUint64`、`GetFloat32`、`GetFloat64`、`GetAll`、`LoadConfigByNoSectionName` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import 任何 `touchgocore/*` 业务包（叶子层）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`config`

## 5. 禁止跨模块直连约束

- 解析失败必须返回错误而非静默取零值。
- 保持叶子层（零/极少内部依赖）：新增任何内部依赖都会把本包的约束传染给上层。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`（框架总说明）
- `docs/repo-layout.md`（依赖分层与包清单）
- 包内 `doc.go` 与文件头注释

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增内部依赖？→ 必裁（叶子层纪律）
- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
