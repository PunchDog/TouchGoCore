# TouchGoCore DB

`db` 是 TouchGoCore 的数据库门面包：**实现全部下沉到子包，调用方只需 import `touchgocore/db`**。
根包只放门面（类型别名 + 工厂函数）与 Redis / MongoDB 的兼容实现。

| 后端 | 门面入口 | 实现位置 | 详细文档 |
|---|---|---|---|
| MySQL | `db.NewMySql` → `*db.Client` + 泛型 `db.Repository[T]` | `db/mysql/` | [mysql/README.md](mysql/README.md) |
| PostgreSQL | `db.NewPostgres` → `*db.PostgresClient` + `db.PostgresRepository[T]` | `db/postgres/` | [README_POSTGRES.md](README_POSTGRES.md) |
| MongoDB | `db.NewMongoDB` → `*db.DbOperate` | `db/`（mongo.go、mongo_crud.go、mongo_gridfs.go） | – |
| Redis | `db.NewRedis` → `*db.Redis` | `db/`（redis.go） | – |
| 统一缓存 | `db.CacheQuery` / `db.CacheSave` | `db/dbcache/` | 本文件「统一缓存」一节 |

MySQL 与 PostgreSQL 风格对齐（泛型 Repository、链式 Query、Tx、错误分类、可观测性插件）；
MySQL 相关符号不加前缀，Postgres 相关符号带 `Postgres` 前缀。

## 特性

- ✅ 泛型 `Repository[T]`：类型安全的 CRUD / 分页 / 统计 / Upsert
- ✅ 链式 `Query` 查询构建器
- ✅ 事务封装（`Tx`：显式 `Commit` / `Rollback`）
- ✅ 结构化错误 `*db.Error` + 14 类 `Kind` 分类与 `IsNotFound` 等断言
- ✅ 软删除（`DeletedAt`）与硬删并存
- ✅ 连接池复用（connectOnly：同配置多次 `NewMySql` 共享同一 socket 池）
- ✅ 可观测性插拔（`MetricsHook`：慢查询 / 连接池水位）
- ✅ 表不存在自动建表（默认关闭，`WithAutoMigrate(true)` 开启）
- ✅ DSN 密码不进错误对象与日志（`redactDSN`）

## 快速开始

### 1. 连接

```go
import (
    "context"
    "time"

    "touchgocore/config"
    "touchgocore/db"
)

client, err := db.NewMySql(&config.MySqlDBConfig{
    Host:         "127.0.0.1:3306",
    Username:     "app",
    Password:     "secret",
    DBName:       "mydb",
    MaxIdleConns: 10,
    MaxOpenConns: 100,
},
    db.WithSlowQueryThreshold(300*time.Millisecond),
    db.WithMetricsHook(db.NewMetricsAdapter()),
)
if err != nil {
    log.Fatalf("connect mysql: %v", err)
}
defer client.Close()

ctx := context.Background()
if err := client.Ping(ctx); err != nil {
    log.Fatalf("mysql unhealthy: %v", err)
}
```

App 已接入时直接用 `app.MySQL`（`initDatabase` 在 `Cfg.MySql != nil` 时装配，并已注入 metrics hook）。

### 2. 定义模型

```go
type User struct {
    ID        uint64           `gorm:"primaryKey;autoIncrement"`
    Name      string           `gorm:"size:100;not null"`
    Email     string           `gorm:"size:100;uniqueIndex;not null"`
    Age       int              `gorm:"default:0"`
    Status    string           `gorm:"size:20;default:'active';index"`
    CreatedAt time.Time        `gorm:"autoCreateTime"`
    UpdatedAt time.Time        `gorm:"autoUpdateTime"`
    DeletedAt gorm.DeletedAt   `gorm:"index"` // 启用软删除
}

func (User) TableName() string { return "t_user" }
```

更多模型示例见 `mysql/models_example.go`（User / Order / Product / Category / Profile / Tag /
Comment / Permission·Role 等）。

### 3. 建表

```go
repo := db.NewRepository[User](client)

// 显式建表（推荐，生产环境走这一条）
if err := repo.AutoMigrate(ctx); err != nil {
    log.Fatal(err)
}

// 或建表期开关（默认关闭，生产慎用，见 mysql/README.md §16）：
// client, _ = db.NewMySql(cfg, db.WithAutoMigrate(true))
// 效果：表不存在时自动 AutoMigrate 并重试一次，之后该 Repository 实例内不再检测。
```

### 4. 基本操作

```go
// 创建
user := &User{Name: "John", Email: "john@example.com", Age: 20}
if err := repo.Create(ctx, user); err != nil {
    log.Fatal(err)
}

// 按主键查询
got, err := repo.FindByID(ctx, user.ID)

// 局部字段更新
if err := repo.UpdateFields(ctx, user.ID, map[string]any{"age": 21}); err != nil {
    log.Fatal(err)
}

// 条件查询
rows, err := repo.FindAll(ctx, db.NewQuery().Eq("status", "active").OrderDesc("id").Limit(20))

// 分页（items, total, err）
items, total, err := repo.Page(ctx, db.NewQuery().Gte("age", 18), 1, 20)

// 统计 / 存在性
n, _ := repo.Count(ctx, db.NewQuery().Eq("status", "active"))
exists, _ := repo.Exists(ctx, db.NewQuery().Eq("email", "john@example.com"))

// 删除（模型带 DeletedAt 即软删；DeleteHard 恒为物理删除）
_ = repo.Delete(ctx, user.ID)
_ = repo.DeleteHard(ctx, user.ID)
```

### 5. 事务

```go
import "database/sql"

tx, err := client.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
if err != nil {
    return err
}
committed := false
defer func() {
    if !committed {
        _ = tx.Rollback()
    }
}()

userRepo := db.NewTxRepository[User](tx)
if err := userRepo.UpdateFields(ctx, 1, map[string]any{"balance": 100}); err != nil {
    return err
}
if err := tx.Commit(); err != nil {
    return err
}
committed = true
return nil
```

### 6. 原生 SQL 与逃生舱

```go
// Raw：拿到绑定 ctx 的 *gorm.DB，可自由组装（含 Unscoped / Debug / 自定义 clause）
var users []User
err := client.Raw(ctx, func(gtx *gorm.DB) error {
    return gtx.Select("id", "name", "email").Limit(100).Find(&users).Error
})

// Engine：直接拿底层 *gorm.DB（客户端已关闭时为 nil，调用方判空）
_ = client.Engine()
```

## API 索引

| 类型 / 函数 | 说明 |
|---|---|
| `db.NewMySql(cfg, opts...) (*Client, error)` | 连接（同配置复用连接池） |
| `db.NewRepository[T](c *Client)` | 泛型 CRUD 引擎（方法上不能声明类型参数，故走包级函数） |
| `db.NewRepositoryWithContext[T](c, ctx)` / `db.NewSessionRepository[T](s)` / `db.NewTxRepository[T](t)` | 绑定 ctx / 会话 / 事务的构造入口 |
| `db.NewQuery()` | 链式查询构建器 |
| `db.NewMetricsAdapter()` | 连接池水位上报到 `metrics.DB` |
| `Client` | `Close` / `Ping(ctx)` / `Stats()` / `Config()` / `Engine()` / `WithContext(ctx)` / `BeginTx(ctx, opts...)` / `Raw(ctx, fn)` / `Repository()` |
| `Repository[T]` | `Create` / `CreateBatch` / `FindByID` / `FindOne` / `FindAll` / `Page` / `Count` / `Exists` / `Update` / `UpdateFields` / `Upsert` / `UpsertBatch` / `Delete` / `DeleteHard` / `AutoMigrate`（全部带 ctx） |
| `Tx` / `Session` | `Commit` / `Rollback` / `WithContext` / `Repository` |
| 断言 | `db.IsNotFound` / `IsDuplicate` / `IsTimeout` / `IsCanceled` / `IsDeadlock` / `IsTableNotExist` |
| Option | `WithSlowQueryThreshold` / `WithMetricsHook` / `WithConnMaxLifetime` / `WithDSNParam` / `WithCharset` / `WithAutoMigrate` |

完整方法表、Query 语义（**`Where` 会覆盖前一个条件**）、错误分类对照、可观测性、
connectOnly 复用规则与 FAQ 见 [mysql/README.md](mysql/README.md)。

## 错误处理

```go
if db.IsNotFound(err) { /* 资源不存在 */ }
if db.IsDuplicate(err) { /* 唯一键冲突（MySQL 1062） */ }
if db.IsTimeout(err)   { /* ctx 超时 */ }
if db.IsCanceled(err)  { /* ctx 取消 */ }
if db.IsDeadlock(err)  { /* 死锁（MySQL 1213），可重试 */ }

if e, ok := err.(*db.Error); ok {
    log.Printf("op=%s kind=%s dur=%s sql=%s err=%v",
        e.Op, e.Kind, e.Duration, e.SQL, e.Err) // SQL 已脱敏
}
```

## GORM 标签说明

### 常用标签

```go
ID        uint64           `gorm:"primaryKey;autoIncrement"` // 主键
Name      string           `gorm:"size:100;not null"`        // 长度 + 非空
Email     string           `gorm:"uniqueIndex"`              // 唯一索引
Status    string           `gorm:"index"`                    // 普通索引
CreatedAt time.Time        `gorm:"autoCreateTime"`           // 自动创建时间
UpdatedAt time.Time        `gorm:"autoUpdateTime"`           // 自动更新时间
DeletedAt gorm.DeletedAt   `gorm:"index"`                    // 软删除
```

### 关系标签

```go
// 一对一
Profile Profile `gorm:"foreignKey:UserID"`

// 一对多
Orders []Order `gorm:"foreignKey:UserID"`

// 多对多
Tags []Tag `gorm:"many2many:product_tags"`

// 自关联（树形）
Children []Category `gorm:"foreignKey:ParentID"`
```

关联预加载（`Preload`）不在 `Repository[T]` 的链式 API 内，走 `client.Raw`：

```go
var user User
err := client.Raw(ctx, func(gtx *gorm.DB) error {
    return gtx.Preload("Orders").Preload("Orders.Items").Preload("Profile").First(&user, 1).Error
})
```

## 最佳实践

1. **先判类型错误再重试**：死锁 / 锁等待可重试，唯一键冲突不可重试。
   ```go
   if db.IsDeadlock(err) { /* 重试 */ }
   if db.IsDuplicate(err) { /* 幂等处理，不要重试 */ }
   ```
2. **事务包住跨表写**，用 `NewTxRepository[T]` 拿到同一事务里的 Repository。
3. **局部更新用 `UpdateFields`**，避免全行 `Save` 覆盖并发写入的字段。
   ```go
   repo.UpdateFields(ctx, id, map[string]any{"status": "active"}) // ✅
   ```
4. **加索引**：唯一列 `uniqueIndex`，过滤列 `index`。
5. **只取需要的列**（`Select`）并限制结果集（`Limit` / `Page`）。
6. **合理配置连接池**：`MaxIdleConns` / `MaxOpenConns` 按实际并发调整。
7. **凭证不进版本库**：DSN 密码在错误对象与日志里会被脱敏，但仍不要打日志。

## 性能优化

```go
// 1. 索引
type User struct {
    Email  string `gorm:"uniqueIndex"`
    Status string `gorm:"index"`
    Age    int    `gorm:"index"`
}

// 2. 批量写入（比循环单条快很多）
_ = repo.CreateBatch(ctx, users)
_ = repo.UpsertBatch(ctx, users, []string{"email"})

// 3. 只查需要的列 + 限制条数
q := db.NewQuery().Select("id", "name", "email").OrderDesc("id").Limit(100)
```

## 常见问题

### Q: 如何看到生成的 SQL？

```go
// 单次语句开 Debug
err := client.Raw(ctx, func(gtx *gorm.DB) error {
    return gtx.Debug().Find(&users).Error
})

// 全局日志级别走子包 Option（未通过 db 门面暴露）：
// import mysqldb "touchgocore/db/mysql"
// c, _ := mysqldb.NewClient(cfg, mysqldb.WithLoggerLevel(logger.Info))
```

### Q: 如何获取连接池统计？

```go
st, err := client.Stats() // sql.DBStats
fmt.Printf("open=%d/%d idle=%d in_use=%d waiting=%d\n",
    st.OpenConnections, st.MaxOpenConnections, st.Idle, st.InUse, st.WaitCount)
```

### Q: 如何处理事务超时？

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

tx, err := client.BeginTx(ctx) // 超时后 ctx 取消，操作直接报错
```

### Q: 如何实现 / 绕过软删除？

```go
type User struct {
    ID        uint64           `gorm:"primaryKey"`
    DeletedAt gorm.DeletedAt   `gorm:"index"`
}

_ = repo.Delete(ctx, 1) // 软删（写 deleted_at）

var deleted []User
_ = client.Raw(ctx, func(gtx *gorm.DB) error {
    return gtx.Unscoped().Find(&deleted).Error // 查已删除记录
})

_ = repo.DeleteHard(ctx, 1) // 物理删除
```

### Q: 同配置多次 `NewMySql` 会重复拨号吗？

不会。按 `host-dbname-user-sha256(password)[:8]` 复用同一 socket 池，详见 mysql/README.md §15。

## 文件说明

| 文件 / 目录 | 职责 |
|---|---|
| `mysql_api.go` | MySQL 门面：类型别名 + 工厂 + Option + 错误断言 |
| `postgres_api.go` | PostgreSQL 门面（`Postgres` 前缀） |
| `redis.go` | Redis 客户端封装 |
| `mongo.go` / `mongo_crud.go` / `mongo_gridfs.go` | MongoDB 客户端、CRUD、GridFS |
| `mysql/` | MySQL 实现（client / repository / query / tx / errors / observability） |
| `mysql/models_example.go` | 模型定义示例（User / Order / Product / Category / Profile / Tag / Comment / RBAC 等） |
| `postgres/` | PostgreSQL 实现（与 mysql 同构，另带 JSONB 等类型） |
| `dbmap/` | 连接注册表（mysql / postgres / redis / mongo 共享） |
| `README.md` | 本文件 |
| `README_POSTGRES.md` | PostgreSQL 与缓存集成指南 |
| `mysql/README.md` | MySQL 完整使用文档 |

## 依赖

```go
require (
    gorm.io/gorm v1.31.2
    gorm.io/driver/mysql v1.6.0
    gorm.io/driver/postgres v1.6.3
    github.com/go-sql-driver/mysql v1.9.3
    github.com/jackc/pgx/v5 v5.10.0
    github.com/redis/go-redis/v9 v9.7.3
)
```

## 相关文档

- [MySQL 完整文档](mysql/README.md)
- [PostgreSQL 与缓存集成](README_POSTGRES.md)
- [模型示例](mysql/models_example.go)
- [GORM 官方文档](https://gorm.io/docs/)
- [GORM 中文文档](https://gorm.io/zh_CN/docs/)
- [MySQL 官方文档](https://dev.mysql.com/doc/)

## 统一缓存（dbcache）

缓存实现在子包 `db/dbcache`，根包 `db` 提供门面（`cache_api.go`）。对外只有两个数据接口：

```go
db.CacheQuery[User](ctx, "user", UserCond{ID: &id}, &out) // out 可为 *T 或 *[]T
db.CacheSave(ctx, "user", "1001", &u, false)               // false=延迟落库，true=立即落库
```

- Redis 数据模型：一行一 hash，`{prefix:table}:r:<key>`，field 为列名，value 为列值字符串。
- 单条查询在条件恰好覆盖全部键列时先读 Redis；未命中回源 MySQL/PostgreSQL/Mongo 并回填。
- 列表查询与含非键列的查询直接读数据库，命中行回填 Redis。
- 延迟落库：写 Redis 后把主键记入同表脏 ZSET，每表一个周期定时器批量 upsert；同键多次写自动合并。
- 停机：`cacheService` 仍注册在服务列表末尾，反序停止保证 final flush 早于 `closeDatabase`。
- 使用前需在启动期 `db.CacheBindMySQL` / `db.CacheBindPostgres` / `db.CacheBindMongo` 注册表与后端。

## License

Same as TouchGoCore project.
