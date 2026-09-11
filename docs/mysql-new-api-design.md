# MySQL 新封装 API 设计

> 状态：设计中　|　目标：**破坏性替换**现有 `db/mysql*.go`，仅保留 gorm 作为内部引擎

## 一、调用方与影响面

| 项 | 现状 |
|---|---|
| 真实生产调用方 | 仅 `app.go`（1 处 NewDbMysql + 1 处 Close） |
| 文档示例 | `db/README.md`（将随代码删除） |
| example/ 目录 | 完全未引用任何 db.* |
| gorm 版本 | v2（已用 `gorm.io/gorm`） |

**结论**：爆炸半径仅限 `app.go` + `db/` 包内，**不需要 shim**，可直接破坏性替换。

## 二、目录组织

```
db/
├── client.go              # New + Client + Option + 池配置
├── errors.go              # Kind 枚举 + Error 结构 + sentinel + classify
├── query.go               # Query 链式构建器（terminal: First/All/Count/Build）
├── tx.go                  # Tx 类型（实现与 Client 一致的最小接口）
├── repository.go          # Repository[T any] 泛型 CRUD
├── observability.go       # gorm.Plugin 拦截器 + MetricsHook 接口
├── redis.go               # 保持原样
├── mongo.go / mongo_crud.go / mongo_gridfs.go  # 保持原样
├── map.go                 # 保持原样
├── client_test.go         # sqlmock 单测
├── query_test.go
├── repository_test.go
└── observability_test.go
```

**不**新建子包 `db/mysql/`：保持在 `db` 包内简化路径，旧文件直接删除（user 确认破坏性替换）。

## 三、核心类型

### 3.1 `Client`（db/client.go）

```go
type Client struct {
    cfg       *config.MySqlDBConfig
    engine    *gorm.DB                // 内部引擎，唯一
    opts      options                  // 不可变 Option 集合
    metrics   MetricsHook
    slowLog   *time.Ticker            // 可选周期刷新 pool stats
}

// 构造与生命周期
func NewClient(cfg *config.MySqlDBConfig, opts ...Option) (*Client, error)
func (c *Client) Close() error
func (c *Client) Ping(ctx context.Context) error
func (c *Client) Stats() sql.DBStats

// 入口
func (c *Client) Repository[T any]() *Repository[T]
func (c *Client) WithContext(ctx context.Context) *Session
func (c *Client) BeginTx(ctx context.Context, opts ...*sql.TxOptions) (*Tx, error)
func (c *Client) Raw(ctx context.Context, fn func(tx *gorm.DB) error) error  // 显式白名单逃生舱
```

### 3.2 `Tx`（db/tx.go）

实现与 `Client` 一致的最小接口（`Repository[T]()`、`WithContext`、`Ping`），但**禁止** `BeginTx`（防嵌套）。

```go
type Tx struct { ... }
func (t *Tx) Commit(ctx context.Context) error
func (t *Tx) Rollback(ctx context.Context) error
func (t *Tx) Repository[T any]() *Repository[T]
```

### 3.3 `Repository[T any]`（db/repository.go）

```go
type Repository[T any] struct { ... }

// 增
func (r *Repository[T]) Create(ctx context.Context, entity *T) error
func (r *Repository[T]) CreateBatch(ctx context.Context, entities []*T) error

// 查
func (r *Repository[T]) FindByID(ctx context.Context, id any) (*T, error)
func (r *Repository[T]) FindOne(ctx context.Context, q *Query) (*T, error)
func (r *Repository[T]) FindAll(ctx context.Context, q *Query) ([]*T, error)
func (r *Repository[T]) Page(ctx context.Context, q *Query, page, size int) ([]*T, int64, error)
func (r *Repository[T]) Count(ctx context.Context, q *Query) (int64, error)
func (r *Repository[T]) Exists(ctx context.Context, q *Query) (bool, error)   // 优化为 SELECT 1 LIMIT 1

// 改
func (r *Repository[T]) Update(ctx context.Context, entity *T) error
func (r *Repository[T]) UpdateFields(ctx context.Context, id any, fields map[string]any) error
func (r *Repository[T]) Upsert(ctx context.Context, entity *T, conflictColumns []string) error

// 删
func (r *Repository[T]) Delete(ctx context.Context, id any) error              // 软删
func (r *Repository[T]) DeleteHard(ctx context.Context, id any) error         // 硬删

// 元信息
func (r *Repository[T]) TableName() string
func (r *Repository[T]) AutoMigrate(ctx context.Context) error
```

### 3.4 `Query`（db/query.go）

```go
type Query struct { ... }

func NewQuery() *Query

// 条件
func (q *Query) Where(expr string, args ...any) *Query
func (q *Query) Eq(col string, v any) *Query
func (q *Query) Ne(col string, v any) *Query
func (q *Query) Lt/Lte/Gt/Gte/Like(col string, v any) *Query
func (q *Query) In(col string, vs ...any) *Query
func (q *Query) NotIn(col string, vs ...any) *Query
func (q *Query) Between(col string, lo, hi any) *Query
func (q *Query) IsNull(col string) *Query
func (q *Query) IsNotNull(col string) *Query

// 排序
func (q *Query) OrderAsc(col string) *Query
func (q *Query) OrderDesc(col string) *Query
func (q *Query) OrderBy(raw string) *Query

// 限制
func (q *Query) Limit(n int) *Query
func (q *Query) Offset(n int) *Query

// 投影
func (q *Query) Select(cols ...string) *Query

// 分组
func (q *Query) GroupBy(cols ...string) *Query
func (q *Query) Having(expr string, args ...any) *Query

// 终端：返回 GORM query 供 Repository 内部消费
func (q *Query) build() (string, []any)
```

### 3.5 `Error` 与 `Kind`（db/errors.go）

```go
type Kind uint8

const (
    KindUnknown      Kind = iota
    KindConnFailed       // 网络/DNS/握手失败
    KindAuth             // 用户名密码错误
    KindNotFound         // gorm.ErrRecordNotFound
    KindDuplicate        // 唯一键冲突（1062）
    KindTimeout          // context deadline / driver timeout
    KindCanceled         // ctx.Done()
    KindDeadlock         // 1213
    KindLockWait         // 1205
    KindSyntax           // SQL 语法
    KindConstraint       // 外键/检查
    KindSlowQuery        // 阈值超时
    KindInvalidColumn    // 非法列名（反射白名单）
    KindPoolExhausted    // 连接池满 + 超时
)

type Error struct {
    Kind     Kind
    Op       string          // "Create"/"FindByID"/"BeginTx"...
    SQL      string          // 已脱敏
    Args     []any
    Duration time.Duration
    Err      error           // 用 %w 包装原始
}

func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool   // 支持 errors.Is(*Error, sentinel)

// 工厂
func newError(op string, err error, kind Kind, sql string, args []any, dur time.Duration) *Error

// classify 根据 gorm/driver error 推断 Kind
func classify(err error) Kind

// sentinel（用于 errors.Is）
var (
    ErrRecordNotFound = errors.New("record not found")
    ErrPoolExhausted   = errors.New("connection pool exhausted")
    ErrSlowQuery       = errors.New("slow query")
)

// 便捷断言
func IsNotFound(err error) bool  { return kindOf(err) == KindNotFound }
func IsDuplicate(err error) bool { return kindOf(err) == KindDuplicate }
func IsTimeout(err error) bool   { return kindOf(err) == KindTimeout }
func IsDeadlock(err error) bool  { return kindOf(err) == KindDeadlock }
```

### 3.6 `Option` 与 `options`（db/client.go）

```go
type options struct {
    slowThreshold   time.Duration        // 默认 200ms
    metricsHook     MetricsHook          // 默认 NoopMetrics
    connMaxLifetime time.Duration        // 默认 24h
    logger          logger.Interface     // 默认 Silent
    extraDSNParams  map[string]string
    charset         string               // 默认 utf8mb4
    collation       string               // 默认 utf8mb4_unicode_ci
}

type Option func(*options)

func WithSlowQueryThreshold(d time.Duration) Option
func WithMetricsHook(h MetricsHook) Option
func WithConnMaxLifetime(d time.Duration) Option
func WithLogger(l logger.Interface) Option
func WithDSNParam(k, v string) Option
func WithCharset(charset, collation string) Option
```

### 3.7 `MetricsHook` 与可观测性（db/observability.go）

```go
// MetricsHook 解耦 db 包与 metrics 包
type MetricsHook interface {
    OnQuery(op string, dur time.Duration, kind Kind)
    OnSlowQuery(op string, sql string, dur time.Duration)
    OnConnection(state string, n float64)
}

// NoopMetrics 默认实现
type NoopMetrics struct{}
func (NoopMetrics) OnQuery(string, time.Duration, Kind)    {}
func (NoopMetrics) OnSlowQuery(string, string, time.Duration) {}
func (NoopMetrics) OnConnection(string, float64)           {}

// gorm.Plugin 实现：拦截 Callback（Before/After/AfterError）
type observabilityPlugin struct {
    client  *Client
    metrics MetricsHook
    slowThr time.Duration
}
func (o *observabilityPlugin) Name() string { return "mysql-observability" }
func (o *observabilityPlugin) Initialize(db *gorm.DB) error
```

**配套**：将 `metrics.DB` 从 `struct` 升级为 `interface`，保留 `dbMetrics` 实现：

```go
// metrics/metrics.go 改造
type DBMetrics interface {
    SetConnections(dbType, state string, n float64)
    IncQuery(dbType, op string)
    ObserveQueryDuration(dbType, op string, dur time.Duration)
    IncQueryError(dbType, op string, kind string)
}
// dbMetrics 实现上述接口
// var DB DBMetrics = dbMetrics{}
```

## 四、配置扩展

`config.MySqlDBConfig` 保持现有 6 字段不变（不破坏 JSON 兼容），新增字段以 omitempty 形式追加（**不强制**）：

```go
type MySqlDBConfig struct {
    Host         string `json:"db_host"`
    Username     string `json:"db_username"`
    Password     string `json:"db_password"`
    DBName       string `json:"db_name"`
    MaxIdleConns int    `json:"db_max_idle_conns"`
    MaxOpenConns int    `json:"db_max_open_conns"`
    // 可选：扩展字段
    Timezone     string `json:"db_timezone,omitempty"`
}
```

新功能通过 `Option` 提供（`WithSlowQueryThreshold`、`WithCharset` 等），避免污染配置结构。

## 五、迁移影响（爆炸半径）

| 位置 | 修改 |
|---|---|
| `app.go:39` | `MySQL *db.DbMysql` → `MySQL *db.Client` |
| `app.go:180-188` | `db.NewDbMysql(cfg)` → `db.NewClient(cfg, db.WithMetricsHook(newMetricsHook()))` |
| `app.go:303-307` | `app.MySQL.Close()` → `app.MySQL.Close()`（API 兼容） |
| `app.go:281-289`（Mongo 调用） | 保持原样 |
| `app.go:127-130`（Redis） | 保持原样 |
| `db/README.md` | 删除（随旧文件） |
| `db/models_example.go` | 保留或迁移到 `db/example_models_test.go` |

**example/ 目录**：无需任何修改（不引用 db）。

## 六、测试策略

| 层级 | 工具 | 覆盖 |
|---|---|---|
| 单元 | `go-sqlmock` | Repository 全部方法的 happy path + 错误分类（1062/1213/ErrRecordNotFound/ctx 取消） |
| 集成 | 真实 MySQL 容器（CI） | AutoMigrate / 事务回滚 / 软删 |
| Benchmark | testing.B | Create/FindByID/FindAll/Update/Paginate 吞吐 |
| 慢查询 | 注入 1ms 阈值 | 断言 `OnSlowQuery` 被调用 |
| ctx 取消 | 立即取消的 ctx | 断言返回 `KindCanceled`，不触发 DB 调用 |

## 七、与现有 metrics 包的集成

`metrics.DB` 升级为 `interface` 后：

```go
// db/observability.go
type metricsHookAdapter struct{ m metrics.DBMetrics }
func (a metricsHookAdapter) OnQuery(op string, dur time.Duration, k Kind) {
    a.m.IncQuery("mysql", op)
    a.m.ObserveQueryDuration("mysql", op, dur)
    if k != KindUnknown && k != KindNotFound {
        a.m.IncQueryError("mysql", op, k.String())
    }
}
func (a metricsHookAdapter) OnSlowQuery(op, sql string, dur time.Duration) { ... }
func (a metricsHookAdapter) OnConnection(state string, n float64) {
    a.m.SetConnections("mysql", state, n)
}
```

## 八、风险

1. **metrics.DB 升级为 interface 涉及全量替换**：仅 `db/` 内部使用 + 1 个测试调用，爆炸半径小。
2. **sqlx/ent 未来切换**：通过 `Repository` 抽象与 `Client.Raw()` 逃生舱，未来替换引擎时不影响调用方。
3. **反射列名校验**：依赖 `reflect.TypeOf((*T)(nil)).Elem()`，性能可接受（仓库方法 1 次缓存）。
4. **慢查询阈值与生产配置差异**：默认值 200ms 通过 Option 可覆盖，配置文件透传阈值。
