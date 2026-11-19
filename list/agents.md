# agents.md — list

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L2
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

通用双向链表 List（可选索引）+ 节点生命周期 + 对象池。localtimer 的调度链表依赖它。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `list.go` | 建表与增删查 | 中 |
| `node.go` | 节点级操作 | 中 |
| `pool.go` | 复用节点 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `list.go` | 类型 List；函数 NewList/NewUnindexedList/Length/Add/Head/Tail/Get/Range | 中 |
| `node.go` | 类型 INode/Node；函数 GetNode/GetId/GetData/SetData/InsertAfter/InsertBefore/Remove/Detach | 中 |
| `pool.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`List`、`NewList`、`NewUnindexedList`、`Length`、`Add`、`Head`、`Tail`、`Get`、`Range`、`Clear`、`INode`、`Node`、`GetNode`、`GetId`、`GetData`、`SetData`、`InsertAfter`、`InsertBefore` 等
- **允许依赖**：`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`util`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`localtimer`、`task_engine`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `util`、`vars` 方向；禁止反向引用上层包。
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
