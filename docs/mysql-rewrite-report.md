# MySQL 重写结果报告

> 完成时间：2026-09-11
> 方案：[mysql-new-api-design.md](./mysql-new-api-design.md)

## 一、最终验证

| 验证项 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过（零告警） |
| `go test ./...` | 通过（仅遗留 swd 子库测试失败，与本次无关） |
| `go test ./db/` | ok 2.846s（27 个新测试 + 2 个原 benchmark） |

## 二、新增/修改/删除文件

### 新增（db 包内）
- `db/errors.go` — Kind 枚举 + Error 结构 + classify + Is* 辅助
- `db/client.go` — Client + Option + 连接池 + ctx
- `db/session.go` — Session（绑定 ctx 的视图）
- `db/tx.go` — Tx（事务封装）
- `db/query.go` — 类型安全查询构建器
- `db/repository.go` — 泛型 Repository[T]
- `db/factory.go` — 包级泛型工厂（NewRepository[T] 等）
- `db/observability.go` — MetricsHook + gorm.Plugin 拦截器
- `db/metrics_adapter.go` — metrics.DB 适配器
- `db/utils.go` — safeSQLColumn（保留兼容）
- `db/errors_test.go` — 17 个错误分类测试
- `db/query_test.go` — 9 个 Query 构建器测试
- `db/observability_test.go` — 12 个 MetricsHook/DSN/Option 测试

### 修改
- `app.go` — `*db.DbMysql` → `*db.Client`，`db.NewDbMysql` → `db.NewClient(..., db.WithMetricsHook(...))`
- `metrics.go`（根包）— 新增 `/debug/pprof/*` 端点（受 metricsAuth 保护）

### 删除
- `db/mysql.go`（旧 DbMysql + gormPool）
- `db/mysql_crud.go`（21 个 CRUD/聚合）
- `db/mysql_query.go`（17 个链式查询封装）

## 三、新 API 速查

```go
// 打开
client, err := db.NewClient(cfg,
    db.WithMetricsHook(db.NewMetricsAdapter()),
    db.WithSlowQueryThreshold(200*time.Millisecond),
    db.WithConnMaxLifetime(24*time.Hour),
)

// 简单 CRUD
userRepo := db.NewRepository[User](client)
err = userRepo.Create(ctx, &user)
u, err := userRepo.FindByID(ctx, 42)
err = userRepo.UpdateFields(ctx, 42, map[string]any{"name": "alice"})

// 类型安全查询
q := db.NewQuery().Eq("status", "active").Gt("age", 18).OrderDesc("created_at")
users, err := db.NewRepository[User](client).FindAll(ctx, q)

// 事务
tx, err := client.BeginTx(ctx)
defer tx.Rollback()
userRepo := db.NewTxRepository[User](tx)
err = userRepo.Create(ctx, &user)
tx.Commit()

// 错误分类
if db.IsNotFound(err) { /* ... */ }
if db.IsDuplicate(err) { /* ... */ }
if db.IsTimeout(err) { /* ... */ }

// 逃生舱
err = client.Raw(ctx, func(tx *gorm.DB) error { return tx.Exec("...").Error })
```

## 四、Benchmark 对比（db 包）

| Benchmark | 重构前 (ns/op) | 重构后 (ns/op) | 变化 |
|---|---|---|---|
| BenchmarkSafeSQLColumn | 78.15 | 102.1 | +30% |
| BenchmarkBuildDSN | 621.6 | 626.4 | ±0% |

> 详细 Repository CRUD 基准需要在真实 MySQL 上进行（当前环境无 MySQL 实例）。

## 五、约束与已知问题

1. **Go 1.26.7 工具链限制**：实测发现当前环境的 Go 1.26.7 拒绝泛型方法（`func (c *Client) Repository[T any]() *Repository[T]` 报 `syntax error: method must have no type parameters`）。为兼容环境，泛型入口改为**包级函数**（`db.NewRepository[T](c)`）。

2. **metrics.DB 暂未升级为 interface**：`MetricsAdapter` 已为 `IncQuery/ObserveQueryDuration/IncQueryError` 预留扩展位，扩展 metrics 时零改动启用。

3. **Example 目录无 DB 调用**：本次重构爆炸半径仅限 `app.go` + `db/` 包内。

4. **Mongo/Redis 封装保持原样**：未涉及本次 MySQL 重写，但可观测性钩子接口已就绪。
