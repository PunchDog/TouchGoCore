# agents.md — db/dbcache

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L4
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

统一读写门面：Redis「一行一 hash」（{prefix:table}:r:<key>）+ 脏 ZSET 记录待写主键，每表一个周期定时器批量回写数据库；删除用墓碑阻挡在途脏账把行写回来。支持 MySQL / Postgres / Mongo 三种落库源。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `engine.go` | 引擎起停与手工 flush | 高 |
| `doc.go` | 读写语义细节与已知限制 | 中 |
| `semantics.go` | 读写语义细节与已知限制 | 中 |
| `expr.go` | 条件表达式与查询构造 | 中 |
| `query.go` | 条件表达式与查询构造 | 中 |
| `insert.go` | 写路径：插入/更新/删除/事务 | 中 |
| `mutate.go` | 写路径：插入/更新/删除/事务 | 高 |
| `store.go` | Redis 原语与 Lua 写入 | 中 |
| `lua_write.go` | Redis 原语与 Lua 写入 | 中 |
| `source_mysql.go` | 三种落库源实现 | 中 |
| `source_postgres.go` | 三种落库源实现 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `spec.go` | 内部实现 | 中 |
| `txstate.go` | 函数 RunTx | 高 |
| `semantics.go` | 内部实现 | 中 |
| `meta.go` | 内部实现 | 中 |
| `lua_write.go` | 内部实现 | 中 |
| `store.go` | 函数 HGetAll/HSetAll/ApplyWrite/ApplyTx/ZAdd/DueMembers/Claim/ZRem | 中 |
| `engine.go` | 类型 Engine；函数 Init/Default/Close/Start/Stop/Flush/FlushAll/FlushKey | 高 |
| `query.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`RunTx`、`HGetAll`、`HSetAll`、`ApplyWrite`、`ApplyTx`、`ZAdd`、`DueMembers`、`Claim`、`ZRem`、`ZScore`、`HDel`、`SetTomb`、`DelTomb`、`TombAlive`、`KeySetScan`、`IndexRange`、`IndexCard`、`RowsGet` 等
- **允许依赖**：`config`、`db/mysql`、`db/postgres`、`localtimer`、`metrics`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`db/mysql`、`db/postgres`、`localtimer`、`metrics`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`db`

## 5. 禁止跨模块直连约束

- 列表查询不走 Redis（行 hash 无二级索引）；条件只支持等值与 IN。
- 生产必须开 appendonly everysec，否则 Redis 宕机会丢脏 ZSET。
- 跨层依赖只向 `config`、`db/mysql`、`db/postgres`、`localtimer`、`metrics`、`vars` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `db/agents.md`（存储门面总索引）
- `README.md`「缓存层资金写路径硬约束」

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否新增/删除/重命名文件或导出符号？→ 必裁，并同步上级索引
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
