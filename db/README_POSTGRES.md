# PostgreSQL 封装与 Redis 多级缓存集成

`db/postgres` 子包在 GORM v2 + pgx 之上复刻了 `db/mysql` 的全部能力：泛型
`Repository[T]`、链式 `Query`、事务 `Tx`、错误分类 `Kind`、可观测性插件，并接入
既有的 Redis 两级缓存架构（脏账本 / 分布式锁 / Seq CAS 全部复用，无需额外配置）。

对外统一走 `db` 门面（`db/postgres_api.go`），与 MySQL 一样带 `Postgres` 前缀，
调用方无需关心实现位置。

## 配置

在启动 JSON 里加 `postgres` 段（对应 `config.PostgreDBConfig`）：

```json
{
  "postgres": {
    "pg_host": "127.0.0.1",
    "pg_port": 5432,
    "pg_username": "app",
    "pg_password": "secret",
    "pg_dbname": "game",
    "pg_sslmode": "disable",
    "pg_timezone": "UTC",
    "pg_schema": "public",
    "pg_max_idle_conns": 10,
    "pg_max_open_conns": 100,
    "pg_conn_max_lifetime": 86400
  }
}
```

`App` 启动时若 `Cfg.Postgre != nil` 会自动拨号并挂到 `app.Postgres`
（见 `app.go: initDatabase`），关闭时随 `closeDatabase` 释放。
`pg_port`/`pg_conn_max_lifetime` 留空或 `<=0` 时回落默认（5432 / 24h）。

## 基础 CRUD

```go
type Player struct {
    ID    int64  `gorm:"primaryKey"`
    Name  string
    Extra db.PGJSONB `gorm:"type:jsonb"`
}

client := app.Postgres                 // 或 db.NewPostgres(cfg, opts...)
repo := db.NewPostgresRepository[Player](client)
ctx := context.Background()

_ = repo.Create(ctx, &Player{ID: 1, Name: "a"})
p, err := repo.FindByID(ctx, int64(1))
if db.IsPostgresNotFound(err) { /* ... */ }

q := db.NewPostgresQuery().Eq("name", "a").OrderDesc("id").Limit(20)
rows, _ := repo.FindAll(ctx, q)

// Upsert 必须点名冲突列（PG ON CONFLICT 需要冲突目标），否则退化为 Create
_ = repo.Upsert(ctx, p, []string{"id"})
```

事务：

```go
tx, _ := client.BeginTx(ctx)
trepo := db.NewPostgresTxRepository[Player](tx)
if err := trepo.Create(ctx, &Player{ID: 2}); err != nil {
    _ = tx.Rollback()
    return err
}
return tx.Commit()
```

## PG 特有类型

`db/postgres/types.go` 提供了带 `Scanner/Valuer` 的包装类型，模型字段直接引用：

| 类型 | PG 列 | 说明 |
|------|-------|------|
| `db.PGJSONB` | `jsonb` / `json` | 空值存 SQL NULL |
| `db.PGStringArray` | `text[]` | 一维文本数组，自动转义引号/逗号 |
| `db.PGInt64Array` | `bigint[]` / `integer[]` | 一维整型数组 |
| `db.PGHStore` | `hstore` | 需服务端启用 `hstore` 扩展 |

## 接入 Redis 两级缓存

回源/落库适配器在 `db/cache/postgres_source.go`，与 MySQL 版同构。缓存实例本身的
脏账本、分布式锁、Seq CAS、写缓冲全部由 `db/cache` 框架提供，Postgres 侧零额外配置。

```go
// WithLoader/WithSaver 是泛型函数，门面无法别名转发，须直接引入子包：
import cachepkg "touchgocore/db/cache"

repo := db.NewPostgresRepository[Player](app.Postgres)
// 业务键 = 玩家名；name 必须有唯一索引（回源按它 IN 查，落库按它冲突）
keyToID := func(k string) any { return k }        // 业务键 → 主键列取值
idOf := func(p *Player) string { return p.Name }  // 实体 → 业务键（回填映射用）

c, err := db.OpenCache[string, Player](
    app.Cache, "player",
    cachepkg.WithLoader[string, Player](
        db.NewPostgresCacheBatchSource(repo, keyToID, "name", nil, idOf)),
    cachepkg.WithSaver[string, Player](
        db.NewPostgresCacheBatchSink(repo, keyToID, "name")), // 冲突列与回源列必须一致
)
if err != nil {
    return err
}

// 读线：Redis 命中直接返回，未命中回源 Postgres 并回填
p, _ := c.GetOrLoad(ctx, "alice")
// 写线：同步写 Redis + 入写缓冲，定时器批量 UpsertBatch 落 Postgres
_ = c.Write(ctx, "alice", &Player{Name: "alice"})
```

> `idCol`（回源 `IN` 的列）与 sink 的 `idCols`（`UpsertBatch` 冲突目标）必须指向
> 同一个唯一列。回源按 `name` 查、落库按 `id` 冲突，会让更新落在错误的行上。
> 第 4 参 `keyFn` 传 `nil` 即用框架默认的键序列化，与该 Cache 的 `WithKeyFunc`
> 不一致时必须显式传同一个函数。

资金类只读表沿用框架口径：只挂 `Loader` 不挂 `Saver`，事务 COMMIT 成功后再
`InvalidateOnCommit` 失效缓存，不要走 `Write`。

## 错误分类

`db.IsPostgresNotFound / IsPostgresDuplicate / IsPostgresTimeout /
IsPostgresCanceled / IsPostgresDeadlock / IsPostgresTableNotExist`，底层按 SQLSTATE
归类（见 `db/postgres/errors.go`）。DSN 密码在进入 `Error.SQL` 前会被脱敏为
`password=***`，绝不进日志。

## 可观测性

**`db.NewPostgres()` 单独调用时不上报任何指标**——子包默认值是
`NoopMetrics{}`（见 `db/postgres/client.go: defaultOptions`），三个 hook 全是空实现。

要接指标必须显式注入 Option：

```go
// 应用级：把连接池水位上报到 metrics.DB（label "postgres"）
client, _ := db.NewPostgres(cfg, db.WithPostgresMetricsHook(db.NewPostgresMetricsAdapter()))

// 或者带自己的 hook
client, _ = db.NewPostgres(cfg, db.WithPostgresMetricsHook(myHook))
```

App 启动时走的是前一种：`app.go: initDatabase` 显式追加
`db.WithPostgresMetricsHook(db.NewPostgresMetricsAdapter())`，所以 `app.Postgres`
是有指标的，但**你自己 `db.NewPostgres(...)` 出来的实例没有**。

注意 `PostgresMetricsAdapter` 只实现了 `OnConnection`（
`metrics.DB.SetConnections("postgres", state, n)`），`OnQuery` / `OnSlowQuery`
目前是空实现——查询耗时与慢查询不进 `metrics.DB`，要上报得自己写 hook。

慢查询阈值默认 200ms（`defaultSlowThreshold`），可用
`db.WithPostgresSlowQueryThreshold(d)` 覆盖；该阈值只影响慢查询 hook 的触发，
不影响查询本身。

`WithLoggerLevel(logger.LogLevel)` 与 `WithPostgresAutoMigrate(bool)` 一样**仅子包可用**，
未通过 `db` 门面暴露（要开自动建表走子包 `pgdb.NewClient`）。
