# agents.md — db

> 状态：已启用 ｜ 最后同步：2026-11-19 ｜ 依赖层级：L4
>
> 维护口径：目录级事实源；变更同步直接上级索引与 `docs/repo-layout.md`

## 1. 目录职责

对上封装 Redis 缓存 + MySQL/Postgres/Mongo 的读写口径，并对资金/账本类列强制写入口径。包内不区分具体引擎：读走 CacheQuery、写走 CacheSave，强一致读用 NoCache。缓存引擎实现下沉在 dbcache 子包。

## 2. 入口文件

| 文件 | 作用 | 变更敏感度 |
|---|---|---|
| `cache_api.go` | 资金写入的唯一合法路径 | 高 |
| `mysql_api.go` | MySQL / Postgres 门面 | 中 |
| `postgres_api.go` | MySQL / Postgres 门面 | 中 |
| `mongo_crud.go` | Mongo CRUD 与 GridFS | 中 |
| `mongo_gridfs.go` | Mongo CRUD 与 GridFS | 中 |
| `README.md` | 连缓存读写语义与硬约束 | 中 |
| `README_POSTGRES.md` | 连缓存读写语义与硬约束 | 中 |

## 3. 关键文件清单

| 文件 | 职责 | 变更敏感度 |
|---|---|---|
| `cache_api.go` | 类型 CacheEngine/CacheConfig/CacheBindOption/CacheOption/CacheOptions/Cond；函数 CacheConfigFrom/InitCache/DefaultCache/StartCache/StopCache/CloseCache/FlushCache/FlushAllCache | 高 |
| `mongo_crud.go` | 函数 Insert/Update/UpdateInsert/RemoveOne/RemoveOneByCond/RemoveAll/DBFindOne/DBFindAll | 中 |
| `redis.go` | 类型 RedisConfigModel/Redis；函数 NewRedis/FlushAll/Close/Get/GetStandaloneClient/GetClusterClient/IsCluster | 中 |
| `mongo.go` | 类型 DbOperate；函数 GetDbSession/NewMongoDB/DBClose | 中 |
| `mysql_api.go` | 类型 Client/Tx/Session/Error/Kind/Option；函数 NewMySql/NewQuery/IsNotFound/IsDuplicate/IsTimeout/IsCanceled/IsDeadlock/WithSlowQueryThreshold | 中 |
| `postgres_api.go` | 类型 PostgresClient/PostgresTx/PostgresSession/PostgresError/PostgresKind/PostgresOption；函数 NewPostgres/NewPostgresQuery/IsPostgresNotFound/IsPostgresDuplicate/IsPostgresTimeout/IsPostgresCanceled/IsPostgresDeadlock/IsPostgresTableNotExist | 中 |
| `mongo_gridfs.go` | 函数 CreateGridFile/OpenGridFile/BulkInsert/BulkUpdate | 中 |

## 4. 对外接口 / 依赖

- **对外暴露**：`CacheEngine`、`CacheConfig`、`CacheBindOption`、`CacheOption`、`CacheOptions`、`Cond`、`Expr`、`CacheConfigFrom`、`InitCache`、`DefaultCache`、`StartCache`、`StopCache`、`CloseCache`、`FlushCache`、`FlushAllCache`、`FlushKeyCache`、`CacheKeyFromCond`、`ParseCacheOptions` 等
- **允许依赖**：`config`、`db/dbcache`、`db/dbmap`、`db/mysql`、`db/postgres`、`metrics`、`util`、`vars`（内部依赖，由 go list 现算）
- **禁止依赖**：业务方禁止直接用 `CacheSave(mustSave=false)` 写资金/账本列；禁止绕过 `NoCache()` 做强一致读。
- **依赖本目录的模块**：`.`、`task_engine`

## 5. 禁止跨模块直连约束

- CacheSave(mustSave=false) 只写 Redis + 登记脏账，当前唯一消费方是 task_engine 的进度写，资金/账本列一律禁止。
- 缓存刷新失败时框架主动失效该行，下次点查必然回源。
- dbcache 默认引擎是进程级全局单例，相关测试必须串行。
- 跨层依赖只向 `config`、`db/dbcache`、`db/dbmap`、`db/mysql`、`db/postgres`、`metrics` 方向；禁止反向引用上层包。
- 新增/删除/重命名文件必须同步本索引与直接上级索引，否则后续变更会走偏。

## 6. 相关规范文档锚点

- `README.md`「缓存层资金写路径硬约束」
- `db/README.md`、`db/README_POSTGRES.md`
- `docs/repo-layout.md` L4 分层

## 7. 裁决检查项（改码前逐项确认）

- [ ] 是否在资金/账本/状态跃迁列上使用了 `CacheSave(mustSave=false)`？→ 禁止（只能 `Increment`/`CompareAndSwap`）
- [ ] 是否改动缓存刷新失败时的主动失效兜底？→ 必裁（会产生库新缓存旧的静默不一致）
- [ ] 是否改变 `NoCache()` 语义？→ 必裁
- [ ] 是否改变对外暴露的核心类型/函数签名？→ 必裁（本仓被外部业务工程 import，导出面即 API）

## 8. 子目录索引

无子目录。

---

本文件是目录级事实源，随代码演进需同步；细节以实际代码与包注释为准。
