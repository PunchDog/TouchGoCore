# db/mysql 使用说明

> 路径：`touchgocore/db/mysql`
> 入口：`touchgocore/db`（公共 API 门面）
> 版本：v1.0

`db/mysql` 是 TouchGoCore 的 MySQL 客户端，基于 **GORM v2** 构建，对外提供连接管理、泛型 CRUD、事务、查询构建器、结构化错误与可观测性插拔等能力。**实现位于子包 `db/mysql`，调用方统一通过 `db` 包的 API 门面使用。**

---

## 1. 包结构

| 文件 | 职责 |
|---|---|
| `errors.go` | `Error` 结构化错误 + `Kind` 错误分类 + 分类函数（`classify`、`Is*`） |
| `client.go` | `Client` 客户端 + 构造/连接池/Ping/关闭 + `Option` 函数式配置 |
| `session.go` | `Session` ctx 绑定视图 |
| `tx.go` | `Tx` 事务封装（`Commit`/`Rollback`） |
| `query.go` | `Query` 类型安全链式查询构建器 |
| `repository.go` | `Repository[T]` 泛型 CRUD 引擎（12 个 CRUD 方法） |
| `factory.go` | 4 个泛型工厂（裸/ctx/事务/Session） |
| `observability.go` | `MetricsHook` 接口 + gorm 插件 + `NoopMetrics` |
| `helpers.go` | `toSnakeCase` 工具 |
| `utils.go` | `safeSQLColumn` 列名校验 |

---

## 2. 核心类型一览

```go
type Client struct { /* ... */ }        // 连接入口
type Session struct { /* ... */ }       // ctx 绑定视图
type Tx     struct { /* ... */ }        // 事务
type Query  struct { /* ... */ }        // 查询构建器
type Repository[T any] struct { /* ... */ }  // 泛型 CRUD
type Error  struct { /* ... */ }        // 结构化错误
type Kind   uint8                       // 错误分类
type Option func(*options)              // 函数式配置
type MetricsHook interface { /* ... */ } // 可观测性 hook
```

---

## 3. 快速开始

### 3.1 创建客户端

```go
import "touchgocore/db"

cfg := &config.MySqlDBConfig{
    Host:         "127.0.0.1:3306",
    Username:     "app",
    Password:     "secret",
    DBName:       "touchgocore",
    MaxIdleConns: 10,
    MaxOpenConns: 100,
}

client, err := db.NewClient(cfg,
    db.WithSlowQueryThreshold(300*time.Millisecond),
    db.WithMetricsHook(db.NewMetricsAdapter()),
    db.WithCharset("utf8mb4", "utf8mb4_unicode_ci"),
)
if err != nil {
    log.Fatalf("connect mysql: %v", err)
}
defer client.Close()
```

### 3.2 Ping 健康检查

```go
ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
defer cancel()
if err := client.Ping(ctx); err != nil {
    log.Printf("mysql unhealthy: %v", err)
}
```

### 3.3 关闭客户端

```go
// 幂等，重复调用安全
if err := client.Close(); err != nil {
    log.Printf("close mysql: %v", err)
}
```

---

## 4. 泛型 Repository

### 4.1 工厂函数

| 函数 | 用途 |
|---|---|
| `db.NewRepository[T](*Client)` | 裸 Repository，无 ctx 绑定 |
| `db.NewRepositoryWithContext[T](*Client, ctx)` | 指定 ctx |
| `db.NewSessionRepository[T](*Session)` | 在 Session 内 |
| `db.NewTxRepository[T](*Tx)` | 在事务内 |

> ⚠️ **方法上不能声明类型参数**（Go 1.26.7 工具链限制），所以泛型工厂独立为包级函数。`Client.Repository()` 返回的 `RepositoryFactory` 提供了内部接入点。

### 4.2 CRUD 方法

```go
type User struct {
    ID        uint64    `gorm:"primaryKey;autoIncrement"`
    Name      string    `gorm:"size:64;not null;index"`
    Age       int       `gorm:"default:0"`
    Status    string    `gorm:"size:16;default:'active'"`
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"` // 启用软删
}

func (User) TableName() string { return "t_user" }
```

| 方法 | 说明 | 返回 |
|---|---|---|
| `Create(ctx, *T)` | 插入单条 | `error` |
| `CreateBatch(ctx, []*T)` | 批量插入 | `error` |
| `FindByID(ctx, id)` | 按主键查 | `(*T, error)` |
| `FindOne(ctx, *Query)` | 条件查单条 | `(*T, error)` |
| `FindAll(ctx, *Query)` | 条件查多条 | `([]*T, error)` |
| `Page(ctx, *Query, page, size)` | 分页 | `([]*T, int64 total, error)` |
| `Count(ctx, *Query)` | 统计 | `(int64, error)` |
| `Exists(ctx, *Query)` | 是否存在 | `(bool, error)` |
| `Update(ctx, *T)` | 按主键全量更新 | `error` |
| `UpdateFields(ctx, id, map[string]any)` | 局部字段更新 | `error` |
| `Upsert(ctx, *T, []string)` | 冲突时更新 | `error` |
| `Delete(ctx, id)` | 软删 | `error` |
| `DeleteHard(ctx, id)` | 硬删（`Unscoped`） | `error` |
| `AutoMigrate(ctx)` | 自动建表 | `error` |

### 4.3 基础示例

```go
repo := db.NewRepository[User](client)

// 插入
u := &User{Name: "alice", Age: 28}
if err := repo.Create(ctx, u); err != nil { /* ... */ }

// 按主键查
got, err := repo.FindByID(ctx, u.ID)

// 局部字段更新
if err := repo.UpdateFields(ctx, u.ID, map[string]any{"age": 29}); err != nil { /* ... */ }

// 软删
if err := repo.Delete(ctx, u.ID); err != nil { /* ... */ }
```

---

## 5. Query 查询构建器

`Query` 是类型安全的链式构建器，所有方法返回 `*Query` 自身以支持链式调用。

### 5.1 WHERE 条件

| 方法 | SQL 等价 |
|---|---|
| `Where("id > ?", 10)` | `id > 10` |
| `Eq("status", "active")` | `status = 'active'` |
| `Ne("status", "deleted")` | `status <> 'deleted'` |
| `Lt / Lte / Gt / Gte` | `<` / `<=` / `>` / `>=` |
| `Like("name", "%ali%")` | `name LIKE '%ali%'` |
| `In("id", 1, 2, 3)` | `id IN (1,2,3)` |
| `NotIn("id", 1, 2)` | `id NOT IN (1,2)` |
| `Between("age", 18, 30)` | `age BETWEEN 18 AND 30` |
| `IsNull("deleted_at")` | `deleted_at IS NULL` |
| `IsNotNull("deleted_at")` | `deleted_at IS NOT NULL` |

> 注：`Where` 会**覆盖**之前的 where 条件；其他方法本质都是 `Where` 的语法糖。

### 5.2 排序、分页、投影

```go
q := db.NewQuery().
    Where("age > ?", 18).
    OrderDesc("created_at").
    Limit(20).
    Offset(40).
    Select("id, name, age").
    GroupBy("status").
    Having("COUNT(*) > ?", 5)
```

### 5.3 使用示例

```go
q := db.NewQuery().
    Gte("age", 18).
    IsNotNull("email").
    OrderDesc("id").
    Limit(10)

users, err := repo.FindAll(ctx, q)

// 分页
items, total, err := repo.Page(ctx, q, 1, 20)

// 统计活跃用户
n, err := repo.Count(ctx, db.NewQuery().Eq("status", "active"))

// 是否存在
exists, err := repo.Exists(ctx, db.NewQuery().Eq("email", "a@b.com"))
```

---

## 6. Session

`Session` 是绑定 `ctx` 的客户端视图，避免每次手动 `WithContext`。

```go
sess := client.WithContext(ctx)

// 在 Session 上获取 Repository
repo := db.NewSessionRepository[User](sess)

u, err := repo.FindByID(ctx, 1)

// 派生新 ctx
sess2 := sess.WithContext(otherCtx)
```

---

## 7. 事务

### 7.1 显式事务

```go
tx, err := client.BeginTx(ctx)
if err != nil {
    return err
}
// 显式提交或回滚（两者必选其一）
defer func() {
    if p := recover(); p != nil {
        _ = tx.Rollback()
        panic(p)
    } else if err != nil {
        _ = tx.Rollback()
    }
}()

userRepo := db.NewTxRepository[User](tx)
logRepo  := db.NewTxRepository[AuditLog](tx)

if err = userRepo.UpdateFields(ctx, uid, map[string]any{"balance": 100}); err != nil {
    return err
}
if err = logRepo.Create(ctx, &AuditLog{UID: uid, Action: "recharge"}); err != nil {
    return err
}

err = tx.Commit()
return err
```

### 7.2 事务内替换 ctx

```go
tx2 := tx.WithContext(otherCtx)
```

---

## 8. 错误处理

### 8.1 错误分类（Kind）

| Kind | 触发场景 |
|---|---|
| `KindUnknown` | 未知错误（兜底） |
| `KindConnFailed` | TCP 连接失败、超时断开（MySQL 2002/2003/2006/2013、1049） |
| `KindAuth` | 权限拒绝（MySQL 1045） |
| `KindNotFound` | 记录不存在（`gorm.ErrRecordNotFound`、`sql.ErrNoRows`） |
| `KindDuplicate` | 唯一键冲突（MySQL 1062、`gorm.ErrDuplicatedKey`） |
| `KindTimeout` | ctx 超时 |
| `KindCanceled` | ctx 取消 |
| `KindDeadlock` | 死锁（MySQL 1213） |
| `KindLockWait` | 锁等待超时（MySQL 1205） |
| `KindSyntax` | SQL 语法/表不存在（MySQL 1064/1146） |
| `KindConstraint` | 外键/约束冲突（MySQL 1216/1451/1452 等） |
| `KindInvalidColumn` | 无效字段（`gorm.ErrInvalidField`） |
| `KindPoolExhausted` | 连接池耗尽 |
| `KindSlowQuery` | 慢查询（仅用于 hook 标记） |

### 8.2 断言函数

```go
if db.IsNotFound(err) { /* 404 / 资源不存在 */ }
if db.IsDuplicate(err) { /* 409 / 唯一键冲突 */ }
if db.IsTimeout(err)   { /* 504 / 超时 */ }
if db.IsCanceled(err)  { /* 499 / 客户端断开 */ }
if db.IsDeadlock(err)  { /* 重试 */ }
```

### 8.3 完整 Error 结构

```go
type Error struct {
    Kind     Kind
    Op       string
    SQL      string
    Args     []any
    Duration time.Duration
    Err      error
}

if e, ok := err.(*db.Error); ok {
    log.Printf("op=%s kind=%s dur=%s sql=%s args=%v err=%v",
        e.Op, e.Kind, e.Duration, e.SQL, e.Args, e.Err)
}
```

`Error` 实现了 `error` / `Unwrap` / `Is` 接口，支持 `errors.Is` / `errors.As` / `fmt.Errorf("%w", err)`。

---

## 9. 可观测性

### 9.1 MetricsHook 接口

```go
type MetricsHook interface {
    OnQuery(op string, dur time.Duration, kind Kind)
    OnSlowQuery(op, sql string, dur time.Duration)
    OnConnection(state string, n float64)
}
```

实现任意一个接口即可上报到你的 metrics 系统（Prometheus / OpenTelemetry / 自研）。

### 9.2 内置实现

| 类型 | 行为 |
|---|---|
| `NoopMetrics{}` | 空实现（默认） |
| `db.MetricsAdapter{}` | 上报到 `metrics.DB`（连接池 gauge） |

### 9.3 自定义 hook 示例

```go
type promHook struct {
    queryTotal   *prometheus.CounterVec
    queryDurHist *prometheus.HistogramVec
    slowQueries  *prometheus.CounterVec
}

func (h *promHook) OnQuery(op string, dur time.Duration, kind db.Kind) {
    h.queryTotal.WithLabelValues(op, kind.String()).Inc()
    h.queryDurHist.WithLabelValues(op).Observe(dur.Seconds())
}

func (h *promHook) OnSlowQuery(op, sql string, dur time.Duration) {
    h.slowQueries.WithLabelValues(op).Inc()
    log.Printf("[SLOW] %s dur=%s sql=%s", op, dur, sql)
}

func (h *promHook) OnConnection(state string, n float64) {
    // 已在 client.go 中通过 NewMetricsAdapter 上报
}

client, _ := db.NewClient(cfg, db.WithMetricsHook(&promHook{ /* ... */ }))
```

### 9.4 慢查询

通过 `WithSlowQueryThreshold(d)` 设置阈值（默认 200ms），超过则触发 `OnSlowQuery`。

---

## 10. 函数式配置（Option）

| Option | 默认值 | 说明 |
|---|---|---|
| `WithSlowQueryThreshold(time.Duration)` | 200ms | 慢查询阈值 |
| `WithMetricsHook(MetricsHook)` | `NoopMetrics` | 注入可观测性 |
| `WithConnMaxLifetime(time.Duration)` | 24h | 连接最大存活时间 |
| `WithDSNParam(k, v)` | – | 添加自定义 DSN 参数（如 `time_zone`） |
| `WithCharset(charset, collation)` | `utf8mb4` / `utf8mb4_unicode_ci` | 字符集 |

DSN 由 `parseTime=true&loc=Local` + 上述选项自动拼装。

---

## 11. 完整示例：用户注册流程

```go
package service

import (
    "context"
    "errors"
    "time"

    "touchgocore/config"
    "touchgocore/db"
)

type User struct {
    ID        uint64    `gorm:"primaryKey;autoIncrement"`
    Email     string    `gorm:"size:128;uniqueIndex;not null"`
    Name      string    `gorm:"size:64;not null"`
    CreatedAt time.Time
}

func (User) TableName() string { return "t_user" }

type UserService struct {
    client *db.Client
}

func NewUserService(c *db.Client) *UserService {
    return &UserService{client: c}
}

func (s *UserService) Register(ctx context.Context, email, name string) (*User, error) {
    repo := db.NewRepository[User](s.client)

    // 幂等检查
    if exists, _ := repo.Exists(ctx, db.NewQuery().Eq("email", email)); exists {
        return nil, errors.New("email already registered")
    }

    // 写入
    u := &User{Email: email, Name: name}
    if err := repo.Create(ctx, u); err != nil {
        if db.IsDuplicate(err) {
            return nil, errors.New("email already registered")
        }
        return nil, err
    }
    return u, nil
}

func (s *UserService) Profile(ctx context.Context, uid uint64) (*User, error) {
    u, err := db.NewRepository[User](s.client).FindByID(ctx, uid)
    if err != nil {
        if db.IsNotFound(err) {
            return nil, nil
        }
        return nil, err
    }
    return u, nil
}

func (s *UserService) Search(ctx context.Context, keyword string) ([]*User, error) {
    return db.NewRepository[User](s.client).FindAll(ctx,
        db.NewQuery().
            Like("name", "%"+keyword+"%").
            OrderDesc("id").
            Limit(20),
    )
}
```

---

## 12. 完整示例：转账事务

```go
func Transfer(ctx context.Context, c *db.Client, fromUID, toUID uint64, amount int64) error {
    tx, err := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
    if err != nil {
        return err
    }
    committed := false
    defer func() {
        if !committed {
            _ = tx.Rollback()
        }
    }()

    repo := db.NewTxRepository[Account](tx)

    from, err := repo.FindByID(ctx, fromUID)
    if err != nil { return err }
    if from.Balance < amount {
        return errors.New("insufficient balance")
    }
    if err := repo.UpdateFields(ctx, fromUID, map[string]any{
        "balance": from.Balance - amount,
    }); err != nil {
        return err
    }

    to, err := repo.FindByID(ctx, toUID)
    if err != nil { return err }
    if err := repo.UpdateFields(ctx, toUID, map[string]any{
        "balance": to.Balance + amount,
    }); err != nil {
        return err
    }

    // 死锁重试
    if err := tx.Commit(); err != nil {
        if db.IsDeadlock(err) {
            return Transfer(ctx, c, fromUID, toUID, amount) // 重试
        }
        return err
    }
    committed = true
    return nil
}
```

---

## 13. 导入方式

**推荐**：使用 `touchgocore/db` 顶层门面。

```go
import "touchgocore/db"

client, _ := db.NewClient(cfg)
repo := db.NewRepository[User](client)
```

**底层**（高级场景，如自定义 gorm Plugin）：直接 import 子包。

```go
import "touchgocore/db/mysql"

gormDB, _ := mysql.NewClient(cfg)
gormDB.Engine().Use(myCustomGormPlugin) // 仍可访问原生 gorm.DB
```

---

## 14. 常见问题

**Q: 为什么 Repository[T] 不能在 `Client` 方法上声明？**
A: Go 1.26.7 工具链不支持在方法上声明类型参数。`Client.Repository()` 返回 `*RepositoryFactory`，泛型构造走包级函数（`NewRepository[T]` / `NewTxRepository[T]` 等）。

**Q: 如何获取原生 `*gorm.DB`？**
A: `client.Engine()` 返回 `*gorm.DB`，可用于自定义 gorm 调用或注册 plugin。

**Q: 软删与硬删的区别？**
A: `Delete` 写入 `deleted_at`；`DeleteHard` 调用 `Unscoped().Delete` 真正从表中移除。

**Q: 慢查询日志会重复吗？**
A: 不会。`observability.go` 在 `gorm:after_*` 回调中检测 `db.Statement.Error == nil` 才上报 `OnSlowQuery`，且每个 callback 实例独立。

**Q: `IsNotFound` 与 `errors.Is(err, gorm.ErrRecordNotFound)` 的区别？**
A: 前者封装了 `sql.ErrNoRows`、GORM 错误、MySQL 原生错误码的统一识别，**推荐**使用 `db.IsNotFound`。

**Q: 是否支持读写分离？**
A: 暂未内建。可在 `Client.Raw` 回调中实现主从切换，或自行封装 `*gorm.DB`。

---

## 15. 连接复用（connectOnly）

`db/mysql.NewClient` 遵循与 `db/redis.NewRedis`、`db/mongo.NewMongoDB` 一致的 **connectOnly 模式**：同配置多次调用会复用同一 `*gorm.DB` 连接池，避免重复拨号。

### 15.1 工作机制

- **注册表键**：`poolKey()` = `host + "-" + dbname + "-" + username + "-" + sha256(password)[:8]`
  - 不同账号 / 不同库 / 密码变更会触发新建连接
- **复用流程**：`NewClient(cfg)` → `poolKey()` → `dbmap.Global.Load(key)`
  - 命中 → 复用 `*gorm.DB`，跳过 `gorm.Open`，直接返回新 `*Client` 包装
  - 未命中 → 完整拨号 + 注册可观测性插件 → 写入注册表
- **失效探测**：命中后对底层 `*sql.DB` 做 500ms `PingContext`；失败视为失效，清出注册表并重新拨号
- **关闭语义**：`Close()` 首次调用关闭底层 socket 并 `dbmapGlobal.Delete(key)`；后续调用幂等 no-op（共享 socket 的多个 Client，**先关者获胜**，其余持有者此后调用 `Ping`/`Query` 会报错）

### 15.2 使用示例

```go
// 第一次：完整初始化
client1, _ := db.NewClient(cfg)
defer client1.Close()

// 第二次：复用 client1 的 socket 与 gorm.DB
client2, _ := db.NewClient(cfg) // 同步返回，无网络开销
defer client2.Close()           // 第二次 Close 是 no-op
```

### 15.3 与 dbmap 子包的关系

`dbmap.Global` 是 `db/mysql`、`db/redis`、`db/mongo` 共享的全局连接注册表；`dbmap.appRegistry` 由 `dbmap.UseAppRegistry(m)` 绑定（由 `app.go` 在 `NewApp` 中调用），启用后 `dbmap.Get` 优先查 app 表再 fallback 全局。注册表逻辑全部位于 `touchgocore/db/dbmap` 子包，`db` 顶层包不再持有任何注册表状态。

---

## 16. 自动建表（表不存在时）

`db.WithAutoMigrate(true)` 启用「表不存在时自动建表」能力。**默认关闭**，需在 `NewClient` 时显式打开。

### 16.1 用法

```go
client, _ := db.NewClient(cfg,
    db.WithAutoMigrate(true), // 开启自动建表
)

repo := db.NewRepository[Order](client)

// 表不存在时自动 CREATE TABLE 并重试
if err := repo.Create(ctx, &Order{...}); err != nil {
    log.Fatal(err)
}
```

### 16.2 行为

| 状态 | 行为 |
|---|---|
| `WithAutoMigrate(false)`（默认） | 行为不变，错误按 `*Error` 原样返回 |
| `WithAutoMigrate(true)` + 表已存在 | 查询直接执行，无额外开销（`migrated=true` 后跳过） |
| `WithAutoMigrate(true)` + 表不存在 | 查询返回 MySQL 1146 → 自动 `AutoMigrate(T)` → 标记 `migrated=true` → **重试一次** |
| `WithAutoMigrate(true)` + 迁移失败 | 返回 `*Error{Op:"Op.AutoMigrate", Kind:KindSyntax}`，不再重试 |
| 事务内 | 自动建表强制关闭（DDL 会触发隐式提交） |

### 16.3 检测函数

```go
if db.IsTableNotExist(err) {
    // 调用方也可以手动触发一次 AutoMigrate
    if err := db.NewRepository[T](client).AutoMigrate(ctx); err != nil {
        return err
    }
}
```

`IsTableNotExist` 仅识别 MySQL 错误码 **1146**（Table 'x.y' doesn't exist）；与 `KindSyntax` 分类正交。

### 16.4 缓存粒度

- `migrated atomic.Bool` 在每个 `Repository[T]` 实例内独立
- 同一 `*Client` 派生的多个 Repository 实例共享 `Client.AutoMigrateEnabled()` 但各自维护迁移状态
- 跨实例不共享缓存（与 Repository 生命周期一致）

### 16.5 注意事项

- ⚠️ **生产环境慎用**：开启后任何对不存在表的写操作都会隐式创建表结构，可能掩盖「表被误删」等事故
- ✅ **推荐用法**：开发/测试环境开启加速迭代；生产建议保持关闭，仍使用显式 `repo.AutoMigrate(ctx)`
- ✅ **并发安全**：`atomic.Bool` 无锁；多个 goroutine 同时触发迁移时 `AutoMigrate` 本身幂等
- ✅ **错误透传**：非表不存在错误（如字段不存在、约束冲突）不会被错误归类为「表不存在」，直接返回
- ⚠️ **事务隔离**：事务内不自动建表（`NewTxRepository` 强制关闭），避免 DDL 导致隐式提交
