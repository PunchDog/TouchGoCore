# agents.md — syncmap

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L0
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

两种并发 map：全局单锁 `Map`/`MapAny` 与按 key 分片的 `ShardedMap`，都提供 LoadOrStore/Range/List。零内部依赖。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `map.go` | 普通并发 map | 中 |
| `sharded.go` | 分片 map（热点首选） | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `sharded.go` | 类型 ShardedMap；函数 Length/Store/Delete/Clear/ClearAll/LoadOrStore/Load/Range | 中 |
| `map.go` | 类型 Map/MapAny；函数 Length/Store/Delete/Clear/ClearAll/LoadOrStore/Load/Range | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`ShardedMap`、`Length`、`Store`、`Delete`、`Clear`、`ClearAll`、`LoadOrStore`、`Load`、`Range`、`List`、`RangeBySort`、`Map`、`MapAny`、`NewAny` 等
- **允许依赖**：无内部依赖（叶子层）
- **禁止依赖**：禁止 import 任何 `touchgocore/*` 业务包（叶子层）；禁止反向依赖上层。
- **依赖本目录的模块**：`.`、`db/dbmap`、`golua`、`localtimer`、`mapmanager`、`ranking`、`rpc`、`task_engine`、`util`、`websocket`

## 5. 禁止跨模块直连约束

- `Range` 期间持锁，回调内禁止再写同一 map（死锁）。
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
