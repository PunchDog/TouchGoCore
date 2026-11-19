# agents.md — ranking

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L2
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

两种内存排行结构（SkipList 与 RankTree）加一套落库/读档封装。主模块内零 importer，作为对外能力包保留。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `skiplist.go` | 跳表 | 中 |
| `rank_tree.go` | 排名树与区间查询 | 中 |
| `ranking_store.go` | 持久化 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `skiplist.go` | 类型 SkipListLevel/SkipListNode/SkipList | 中 |
| `ranking_store.go` | 类型 DbRankInfo；函数 LoadRanking/SaveRanking/LoadRankTrees/Load/Save/GetRankTree/GetAllRankTree/HasRankTree | 中 |
| `rank_tree.go` | 类型 RankInfo/RankTree；函数 Compare/NewRankTree/AddRankInfo/RemoveRankInfo/UpdateRankInfo/QueryRankInfo/QueryByRankRange/QueryByRank | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`SkipListLevel`、`SkipListNode`、`SkipList`、`SkipListMaxLevel`、`SkipListProbability`、`DbRankInfo`、`LoadRanking`、`SaveRanking`、`LoadRankTrees`、`Load`、`Save`、`GetRankTree`、`GetAllRankTree`、`HasRankTree`、`ResetRankTree`、`RankInfo`、`RankTree`、`Compare` 等
- **允许依赖**：`random`、`syncmap`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`random`、`syncmap`）；禁止反向依赖上层。
- **依赖本目录的模块**：—

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `random`、`syncmap` 方向；禁止反向引用上层包。
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
