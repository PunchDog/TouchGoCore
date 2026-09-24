# TouchGoCore DB - MySQL 数据库模块

基于 GORM 的 MySQL 数据库操作封装，提供简洁、高效的数据库访问接口。

## 特性

- ✅ 基于 GORM ORM 框架
- ✅ 连接池管理
- ✅ 事务支持
- ✅ 软删除支持
- ✅ 关联关系支持（一对一、一对多、多对多）
- ✅ 丰富的查询 API
- ✅ 原生 SQL 支持
- ✅ 批量操作优化
- ✅ 连接池统计

## 快速开始

### 1. 初始化

```go
import "touchgocore/db"

mysqlDB, err := db.NewDbMysql(&config.MySqlDBConfig{
    Host:          "localhost:3306",
    Username:      "root",
    Password:      "password",
    DBName:        "mydb",
    MaxIdleConns:  10,
    MaxOpenConns:  100,
})
if err != nil {
    log.Fatal(err)
}
defer mysqlDB.Close()
```

### 2. 定义模型

```go
type User struct {
    ID        uint           `gorm:"primaryKey"`
    Name      string         `gorm:"size:100;not null"`
    Email     string         `gorm:"size:100;uniqueIndex;not null"`
    Age       int            `gorm:"default:0"`
    Status    string         `gorm:"size:20;default:'active';index"`
    CreatedAt time.Time      `gorm:"autoCreateTime"`
    UpdatedAt time.Time      `gorm:"autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"index"`
}
```

### 3. 自动迁移

```go
err := mysqlDB.AutoMigrate(&User{}, &Order{}, &Product{})
```

### 4. 基本操作

```go
// 创建
user := User{Name: "John", Email: "john@example.com"}
mysqlDB.Create(&user)

// 查询
var user User
mysqlDB.First(&user, 1)

// 更新
mysqlDB.Update(&User{ID: 1, Name: "NewName"})

// 删除
mysqlDB.Delete(&User{}, 1)
```

## 文件说明

- **mysql.go** - 核心数据库操作封装
- **models_example.go** - 模型定义示例（包含15+常用模型）
- **map.go** - 连接池管理
- **GORM_GUIDE.md** - 详细使用指南
- **README.md** - 本文件

## 核心 API

### 基础 CRUD

```go
// 创建
Create(value interface{}) error
BatchCreate(values interface{}) error

// 查询
Find(dest interface{}, conds ...interface{}) error
First(dest interface{}, conds ...interface{}) error
Take(dest interface{}, conds ...interface{}) error
Last(dest interface{}, conds ...interface{}) error

// 更新
Updates(values interface{}) error
Update(column string, value interface{}) error
UpdateWithConditions(model, whereClause, args, updates) error

// 删除
Delete(value interface{}, conds ...interface{}) error
DeleteWithConditions(model, whereClause, args) error
```

### 聚合查询

```go
Count(model, whereClause, args...) (int64, error)
Exists(model, whereClause, args...) (bool, error)
Sum(model, column, whereClause, args...) (float64, error)
Avg(model, column, whereClause, args...) (float64, error)
Max(model, column, whereClause, args...) (interface{}, error)
Min(model, column, whereClause, args...) (interface{}, error)
```

### 高级功能

```go
// 分页
Paginate(model, page, pageSize, whereClause, args, order, dest) (int64, error)

// 事务
Transaction(fn func(tx *gorm.DB) error) error
Begin() *gorm.DB
Commit(tx *gorm.DB) error
Rollback(tx *gorm.DB) error

// 批量操作
BatchUpdate(model, whereClause, args, updates) (int64, error)
BatchDelete(model, whereClause, args) (int64, error)

// Upsert
Upsert(model, conflictColumns...) error

// 原生 SQL
RawQuery(sql, args, dest) error
Exec(sql, args...) error

// 其他
AutoMigrate(models...)
GetDB() *gorm.DB
SetLoggerLevel(level)
GetStats() (maxOpen, open, inUse, waitCount, waitDuration, idle int)
```

### 链式查询

```go
Where(query, args...) *gorm.DB
Order(value) *gorm.DB
Limit(limit int) *gorm.DB
Offset(offset int) *gorm.DB
Group(name string) *gorm.DB
Having(query, args...) *gorm.DB
Select(query, args...) *gorm.DB
Distinct(args...) *gorm.DB
Joins(query, args...) *gorm.DB
Preload(query, args...) *gorm.DB
Scopes(funcs...) *gorm.DB
Model(value) *gorm.DB
Table(name, args...) *gorm.DB
```

## 使用示例

### 查询示例

```go
// 基础查询
var users []User
mysqlDB.Find(&users)

// 条件查询
mysqlDB.Where("age > ?", 18).Where("status = ?", "active").Find(&users)

// 排序分页
total, err := mysqlDB.Paginate(&User{}, 1, 20,
    "age > ?", []interface{}{18},
    "created_at DESC", &users)

// 原生 SQL
var results []map[string]interface{}
mysqlDB.RawQuery("SELECT * FROM users WHERE age > ?", 18, &results)
```

### 关联查询

```go
// 预加载关联
var user User
mysqlDB.Preload("Orders").Preload("Profile").First(&user, 1)

// 嵌套预加载
mysqlDB.Preload("Orders.Items").First(&user)

// 带条件的预加载
mysqlDB.Preload("Orders", "status = ?", "paid").First(&user)
```

### 事务示例

```go
// 自动事务
err := mysqlDB.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&order).Error; err != nil {
        return err
    }
    return tx.Model(&product).Update("stock", gorm.Expr("stock - ?", order.Quantity)).Error
})

// 手动事务
tx := mysqlDB.Begin()
tx.Create(&order)
tx.Create(&payment)
if err := tx.Commit().Error; err != nil {
    tx.Rollback()
}
```

### 批量操作

```go
// 批量插入
users := []User{{Name: "John"}, {Name: "Jane"}}
mysqlDB.BatchCreate(users)

// 批量更新
count, _ := mysqlDB.BatchUpdate(&User{},
    "status = ?", []interface{}{"active"},
    map[string]interface{}{"updated_at": time.Now()})

// 批量删除
count, _ := mysqlDB.BatchDelete(&User{},
    "age < ?", []interface{}{18})
```

## 模型示例

详见 `models_example.go`，包含以下模型：

- **User** - 用户模型（基础）
- **Order** - 订单模型
- **Product** - 商品模型
- **Category** - 分类模型（树形结构）
- **Profile** - 用户资料（一对一）
- **OrderItem** - 订单项（一对多）
- **Tag** - 标签（多对多）
- **Log** - 日志模型
- **Setting** - 配置模型
- **Article** - 文章模型
- **Session** - 会话模型
- **Payment** - 支付模型
- **Notification** - 通知模型
- **Comment** - 评论模型（树形结构）
- **Attachment** - 附件模型
- **Permission/Role** - 权限角色模型（RBAC）

## GORM 标签说明

### 常用标签

```go
ID        uint  `gorm:"primaryKey"`        // 主键
Name      string `gorm:"size:100;not null"` // 长度+非空
Email     string `gorm:"uniqueIndex"`     // 唯一索引
Status    string `gorm:"index"`            // 普通索引
CreatedAt time.Time `gorm:"autoCreateTime"` // 自动创建时间
UpdatedAt time.Time `gorm:"autoUpdateTime"` // 自动更新时间
DeletedAt gorm.DeletedAt `gorm:"index"`   // 软删除
```

### 关系标签

```go
// 一对一
Profile Profile `gorm:"foreignKey:UserID"`

// 一对多
Orders []Order `gorm:"foreignKey:UserID"`

// 多对多
Tags []Tag `gorm:"many2many:product_tags;"`

// 自关联（树形）
Children []Category `gorm:"foreignKey:ParentID"`
```

## 最佳实践

1. **使用预加载避免 N+1 查询**
   ```go
   mysqlDB.Preload("Orders").First(&user)  // ✅
   // 而不是循环查询  // ❌
   ```

2. **使用事务保证数据一致性**
   ```go
   mysqlDB.Transaction(func(tx *gorm.DB) error {
       tx.Create(&order)
       tx.Model(&product).Update("stock", gorm.Expr("stock - ?", qty))
   })  // ✅
   ```

3. **添加索引优化查询**
   ```go
   Email string `gorm:"uniqueIndex"`  // ✅
   Status string `gorm:"index"`       // ✅
   ```

4. **使用模型而非 map[string]interface{}**
   ```go
   user := User{ID: 1, Name: "NewName"}  // ✅ 类型安全
   updates := map[string]interface{}{"name": "NewName"}  // ❌ 不推荐
   ```

5. **合理配置连接池**
   ```go
   MaxIdleConns: 10   // 根据实际负载调整
   MaxOpenConns: 100  // 根据实际负载调整
   ```

更多最佳实践请参考 [GORM_GUIDE.md](./GORM_GUIDE.md)

## 性能优化

### 1. 使用索引

```go
type User struct {
    Email  string `gorm:"uniqueIndex"`  // 唯一索引
    Status string `gorm:"index"`        // 普通索引
    Age    int    `gorm:"index"`        // 普通索引
}
```

### 2. 批量操作

```go
// 批量插入（比循环插入快很多）
mysqlDB.BatchCreate(users)

// 批量更新
mysqlDB.BatchUpdate(&User{}, "status = ?", []interface{}{"active"}, updates)
```

### 3. 预加载关联

```go
// 一次性加载，避免 N+1
mysqlDB.Preload("Orders").Preload("Profile").First(&user)
```

### 4. 只查询需要的字段

```go
mysqlDB.Select("id, name, email").Find(&users)
```

### 5. 使用 LIMIT 限制结果集

```go
mysqlDB.Limit(100).Find(&users)
```

## 常见问题

### Q: 如何查看生成的 SQL？

```go
// 开启日志
mysqlDB.SetLoggerLevel(logger.Info)

// 或直接使用 GORM
db := mysqlDB.GetDB()
db = db.Debug()
```

### Q: 如何获取连接池统计信息？

```go
maxOpen, open, inUse, waitCount, waitDuration, idle := mysqlDB.GetStats()
fmt.Printf("Open: %d/%d, In Use: %d, Idle: %d\n", open, maxOpen, inUse, idle)
```

### Q: 如何处理事务超时？

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

db := mysqlDB.GetDB().WithContext(ctx)
err := db.Transaction(func(tx *gorm.DB) error {
    // ...
})
```

### Q: 如何实现软删除？

```go
type User struct {
    ID        uint           `gorm:"primaryKey"`
    DeletedAt gorm.DeletedAt `gorm:"index"`
}

// 软删除
mysqlDB.Delete(&User{}, 1)

// 查询已删除的记录
mysqlDB.GetDB().Unscoped().Find(&users)

// 永久删除
mysqlDB.GetDB().Unscoped().Delete(&User{}, 1)
```

## 依赖

```go
require (
    gorm.io/gorm v1.31.1
    gorm.io/driver/mysql v1.6.0
)
```

## 相关文档

- [GORM 官方文档](https://gorm.io/docs/)
- [GORM 中文文档](https://gorm.io/zh_CN/docs/)
- [MySQL 官方文档](https://dev.mysql.com/doc/)
- [GORM_GUIDE.md](./GORM_GUIDE.md) - 详细使用指南
- [models_example.go](./models_example.go) - 模型示例

## 两级缓存（Redis + MySQL/Mongo）

在原有独立接口之上**纯追加**一层集成封装：Redis 作一级缓存，MySQL/Mongo 作回源与落库后端。子包 `db/cache` 不 import `db` 根包；Mongo 适配器（依赖 `DbOperate`）在 `db` 包的 `cache_mongo_adapter.go`，MySQL 适配器在 `db/cache/mysql_source.go`。门面类型别名与构造函数在 `db/cache_api.go`。

### 读线（Redis 优先）

client 拿到的值**一定是经过 Redis 的值**，不允许透传数据库值：

```
GetOrLoad(ctx, key)
  1. Redis GET → 命中且未逻辑过期 → 直接返回（空标记 → ErrCacheNotFound，防穿透）
  2. 命中但逻辑过期（默认 async）→ 返回 Redis 旧值 + 后台单飞预热（stale-while-revalidate）
  3. miss / decode 失败 / Redis 读错误：
       失败退避期内 → ErrSourceUnavailable（防打穿 DB）
       否则 singleflight 同步回源：Loader.Load
         ├ 成功      → SET 回 Redis → 再从 Redis GET 读回 → 返回读回值
         ├ 未找到    → SET 空标记(NegativeTTL) → 读回 → ErrCacheNotFound
         └ 失败      → 记录退避 → 返回错误
```

- `RequireRedis=true`（默认）：回填写不进/读不回 Redis 即本次报错；置 false 才允许直返源值。
- 写缓冲中的脏 key 会被读线回填跳过（写线持有 Redis 新鲜度优先权，避免旧库值覆盖新缓存值）。
- 批量：`MGetOrLoad` 优先 `BatchLoader` 一次批量回源 → 逐 key 回填 → MGet 读回。

### 写线（同步写 Redis，异步通知数据库）

```
Write(key, val)
  1. 同步 SET Redis（带逻辑过期 envelope + 物理 TTL 抖动）——写不进即本次 Write 报错、不入缓冲
  2. 入分片写缓冲（map 去重 + 原子 seq + dirty 计数），随后定时器批量落库
Remove/WriteDelete：同步 DEL → 缓冲 tombstone，等落库删除
```

- `flushLoop`：ctx / ticker(FlushInterval) / 容量信号三选一驱动 `flushOnce`；按 `BatchSize` 切批走 `BatchSaver.SaveBatch`（MySQL 为 `UpsertBatch` 单条 SQL），无批量接口时按 `SaveConcurrency` 并发单条。
- 落库失败回插重试，超过 `MaxRetry` 丢弃 + DEL 该 key 缓存 + `vars.Error`（防止 Redis 长期展示未落库值）；`MaxDirtyAge` 超龄只告警。
- 容量水位 `MaxDirtyKeys`：默认 `block` 内联 flush 背压，可 `drop` 告警丢弃。
- `Stop/Close`：cancel → 用独立 ctx 做 final flush 把残余脏数据全部落库。
- 降级（`Enabled=false`）：Write 退化为同步直写 Saver（write-through 不丢数据），读线直连回源。

### 崩溃不丢数据：脏账本 + 启动恢复（journal.go）

进程内写缓冲扛不住 kill -9 / OOM / Stop 超时。对策是把「待落库清单」也放进 Redis（写线本来就必须同步写 Redis 成功才返回）：

- `Write`/`Remove` 在同步写 Redis 的**同一次 pipeline** 里记/销账：ZSET `键 = {prefix}:{group}:{name}:dirty#zset`，member=`u|键`/`d|键`（一键至多一条，重复写自动去重），score=seq；
- 落库成功、或超过 MaxRetry 被丢弃时销账（ZREM）——已宣告放弃的数据不会被重启复活；
- 进程重启（`Layer.Start` → `Cache.Run` 开头）扫账重放：envelope 值还活着就重新落库；已物理过期的记 `RecoverMiss` 并 Error 告销账（这是唯一真正不可恢复的窗口）；
- 恢复重放要求 keyStr→K 可反解：string/整数族键框架自动处理，其它类型用 `WithKeyDecoder`；解不开记 `RecoverMiss` 销账止损；
- 配置 `cache.journal=false` 或一级缓存不支持时账本自动关闭（行为退回无账本版本）；
- 生产建议 Redis 开 AOF（appendfsync everysec），账本与 envelope 值本体都抗 Redis 重启。

丢失窗口由此从「整个写缓冲」压缩为「崩溃 + envelope 恰好在重启前 TTL 过期」双重小概率。资金等强一致路径用 **`WriteSync(ctx,key,val)`**：同步写 Redis + 同步落库、绕过缓冲与账本，任一失败即报错且回滚 Redis 展示值——真零丢失，代价是一次 DB RTT。

### 生命周期接线

配置 `cache` 段（`config.CacheConfig`，时长一律 `XxxMS int`）启用后，`initDatabase` 在 Redis 就绪后构造 `App.Cache *db.CacheLayer`；`cacheService` 注册在 `registerServices()` **列表末尾**——借 Shutdown 反序停止保证 final flush 早于 `closeDatabase`。未配置或未启用时该服务空转。

### 用法示例

```go
// 从配置构造（App 内），或独立 NewCache[K,V](name, app.Redis, opts...)
c, err := db.OpenCache[string, User](app.Cache, "user",
    db.WithLoader[string, User](db.NewMysqlCacheSource[User, string](userRepo, func(k string) any { return k })),
    db.WithSaver[string, User](db.NewMysqlCacheBatchSink[User, string](userRepo, func(k string) any { return k }, "id")),
)
u, err := c.GetOrLoad(ctx, "u:1001") // 拿到的是 Redis 读回的值
err = c.Write(ctx, "u:1001", &u)     // 同步写 Redis 成功即返回，稍后批量落库
```

### 关键限制

- **Redis 集群**：跨槽不能 MGET/pipeline，`KV.MGet` 自动退化为并发单 key GET（并发度 `MGetConcurrency`，默认 16）。
- **Mongo 后端**：`db` 包 CRUD 回调固定 5s 超时且不可 ctx 取消，闭包适配器无法提前中断——建议配大 `MaxRetry`、拉长 `FlushInterval`。
- **key 规则**：`{KeyPrefix}:{gameGroup}:{name}:{key}`，段内 `:`→`_`，超 128 字符截断后拼 SHA1 短哈希。
- 物理 TTL 带按 key 稳定的确定性抖动（防同刻集中过期雪崩），逻辑过期 = 物理 TTL × `LogicalPct%`。

## License

Same as TouchGoCore project.
