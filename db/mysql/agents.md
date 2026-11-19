# agents.md — db/mysql

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L2
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

TouchGoCore 自研的 MySQL 客户端：连接池、事务、查询构造器、通用 Repository 与可插拔监控。被 db 门面包装成 NewMySql 系列；错误分类在此定义。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `client.go` | 建客户端与常用 Option | 中 |
| `query.go` | 拼查询条件 | 中 |
| `errors.go` | 错误判定 | 中 |
| `observability.go` | 监控埋点接口 | 中 |
| `tx.go` | 事务与会话 | 高 |
| `session.go` | 事务与会话 | 中 |
| `models_example.go` | 照抄一个 model 写法 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `repository.go` | 类型 Repository；函数 WithContext/Client/Context/TableName/Create/CreateBatch/FindByID/FindOne | 中 |
| `client.go` | 类型 Client/Option/RepositoryFactory；函数 WithSlowQueryThreshold/WithMetricsHook/WithConnMaxLifetime/WithLoggerLevel/WithDSNParam/WithCharset/WithAutoMigrate/NewClient | 中 |
| `models_example.go` | 类型 User/Order/Product/Category/Log/Setting；函数 TableName/BeforeCreate | 中 |
| `errors.go` | 类型 Kind/Error；函数 String/Error/Unwrap/Is/IsNotFound/IsDuplicate/IsTimeout/IsCanceled | 中 |
| `query.go` | 类型 Query；函数 NewQuery/Where/Eq/Ne/Lt/Lte/Gt/Gte | 中 |
| `observability.go` | 类型 MetricsHook/NoopMetrics；函数 OnQuery/OnSlowQuery/OnConnection/NewObservabilityPlugin/Name/Initialize | 中 |
| `tx.go` | 类型 Tx；函数 Commit/Rollback/Context/Repository/WithContext | 高 |
| `factory.go` | 内部实现 | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`Repository`、`WithContext`、`Client`、`Context`、`TableName`、`Create`、`CreateBatch`、`FindByID`、`FindOne`、`FindAll`、`Page`、`Count`、`Exists`、`Update`、`UpdateFields`、`Upsert`、`UpsertBatch`、`Delete` 等
- **允许依赖**：`config`、`db/dbmap`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：禁止 import 不在允许依赖清单中的 `touchgocore/*` 包（当前仅：`config`、`db/dbmap`、`vars`）；禁止反向依赖上层。
- **依赖本目录的模块**：`db`、`db/dbcache`、`task_engine`

## 5. 禁止跨模块直连约束

- 跨层依赖只向 `config`、`db/dbmap`、`vars` 方向；禁止反向引用上层包。
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
