# GORM MySQL 使用指南

## 目录
- [快速开始](#快速开始)
- [基础操作](#基础操作)
- [模型定义](#模型定义)
- [查询](#查询)
- [关联关系](#关联关系)
- [事务](#事务)
- [高级功能](#高级功能)
- [最佳实践](#最佳实践)

## 快速开始

### 1. 初始化数据库连接

```go
import "touchgocore/db"

// 创建数据库连接
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

// 检查连接
if err := mysqlDB.Ping(); err != nil {
    log.Fatal(err)
}
```

### 2. 自动迁移表结构

```go
// 自动迁移所有模型
err := mysqlDB.AutoMigrate(&User{}, &Order{}, &Product{})
if err != nil {
    log.Fatal(err)
}
```

### 3. 基本操作

```go
// 创建
user := User{Name: "John", Email: "john@example.com", Age: 30}
err := mysqlDB.Create(&user)

// 查询
var user User
err := mysqlDB.First(&user, 1) // 查询 ID=1 的用户

// 更新
err := mysqlDB.Update(&User{ID: 1, Name: "NewName"})

// 删除
err := mysqlDB.Delete(&User{}, 1)
```

## 基础操作

### 创建记录

```go
// 单个记录
user := User{Name: "John", Email: "john@example.com"}
mysqlDB.Create(&user)
fmt.Println(user.ID) // 自增 ID

// 批量创建
users := []User{
    {Name: "John", Email: "john@example.com"},
    {Name: "Jane", Email: "jane@example.com"},
}
mysqlDB.BatchCreate(users)
```

### 查询记录

```go
// 查询单条记录
var user User
mysqlDB.First(&user, 1)              // 按 ID
mysqlDB.Take(&user)                 // 查询第一条
mysqlDB.Last(&user)                 // 查询最后一条

// 带条件查询
mysqlDB.Take(&User{}, "name = ?", "John")
mysqlDB.First(&User{}, "email = ?", "test@example.com")

// 查询多条记录
var users []User
mysqlDB.Find(&users)                 // 查询所有
mysqlDB.Find(&users, "age > ?", 18)  // 带条件

// 统计
count, err := mysqlDB.Count(&User{}, "age > ?", 18)
fmt.Println(count)

// 检查存在
exists, _ := mysqlDB.Exists(&User{}, "email = ?", "test@example.com")
fmt.Println(exists)
```

### 更新记录

```go
// 更新整个记录
user := User{ID: 1, Name: "NewName", Age: 31}
mysqlDB.Update(&user)

// 更新单个字段
mysqlDB.Update("name", "NewName")

// 根据条件更新
mysqlDB.UpdateWithConditions(&User{}, "age > ?", []interface{}{18},
    map[string]interface{}{"status": "inactive"})

// 批量更新
count, _ := mysqlDB.BatchUpdate(&User{},
    "status = ?", []interface{}{"active"},
    map[string]interface{}{"updated_at": time.Now()})
```

### 删除记录

```go
// 删除单条记录
mysqlDB.Delete(&User{}, 1)

// 根据条件删除
mysqlDB.DeleteWithConditions(&User{}, "age < ?", []interface{}{18})

// 批量删除
count, _ := mysqlDB.BatchDelete(&User{}, "status = ?", []interface{}{"inactive"})
```

## 模型定义

### 基础模型

```go
type User struct {
    ID        uint           `gorm:"primaryKey"`
    Name      string         `gorm:"size:100;not null"`
    Email     string         `gorm:"size:100;uniqueIndex;not null"`
    Age       int            `gorm:"default:0"`
    Status    string         `gorm:"size:20;default:'active';index"`
    CreatedAt time.Time      `gorm:"autoCreateTime"`
    UpdatedAt time.Time      `gorm:"autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"index"` // 软删除
}
```

### 常用标签

```go
// 主键
ID uint `gorm:"primaryKey"`

// 自增
ID uint `gorm:"primaryKey;autoIncrement"`

// 非空
Name string `gorm:"not null"`

// 唯一索引
Email string `gorm:"uniqueIndex"`

// 普通索引
Status string `gorm:"index"`

// 复合索引
FirstName string `gorm:"index:idx_name"`
LastName  string `gorm:"index:idx_name"`

// 指定类型
Amount float64 `gorm:"type:decimal(10,2)"`

// 默认值
Status string `gorm:"default:'active'"`

// 自动时间
CreatedAt time.Time `gorm:"autoCreateTime"`
UpdatedAt time.Time `gorm:"autoUpdateTime"`

// 自定义表名
func (User) TableName() string {
    return "sys_users"
}
```

## 查询

### 基础查询

```go
// 查询所有
var users []User
mysqlDB.Find(&users)

// 条件查询
mysqlDB.Where("age > ?", 18).Find(&users)
mysqlDB.Where("name = ? AND age > ?", "John", 18).Find(&users)

// 多条件
mysqlDB.Where("age > ?", 18).Where("status = ?", "active").Find(&users)

// OR 条件
mysqlDB.Where("name = ?", "John").Or("name = ?", "Jane").Find(&users)
```

### 排序和分页

```go
// 排序
mysqlDB.Order("age DESC").Find(&users)
mysqlDB.Order("created_at DESC").Order("id ASC").Find(&users)

// 分页
total, err := mysqlDB.Paginate(&User{}, 1, 20,
    "age > ?", []interface{}{18},
    "created_at DESC", &users)
fmt.Printf("Total: %d, Current: %d\n", total, len(users))

// 手动分页
mysqlDB.Offset(0).Limit(10).Find(&users)
mysqlDB.Offset(10).Limit(10).Find(&users)
```

### 聚合查询

```go
// Sum
total, _ := mysqlDB.Sum(&Order{}, "amount", "status = ?", "paid")

// Avg
avg, _ := mysqlDB.Avg(&Order{}, "amount")

// Max
max, _ := mysqlDB.Max(&Order{}, "amount")

// Min
min, _ := mysqlDB.Min(&Order{}, "amount")

// Group
var results []map[string]interface{}
mysqlDB.Group("status").Select("status, COUNT(*) as count").Find(&results)

// Having
mysqlDB.Group("status").Having("COUNT(*) > ?", 10).Find(&results)
```

### 复杂查询

```go
// 原生 SQL
var results []map[string]interface{}
mysqlDB.RawQuery(`
    SELECT u.*, COUNT(o.id) as order_count
    FROM users u
    LEFT JOIN orders o ON u.id = o.user_id
    GROUP BY u.id
`, nil, &results)

// 链式查询
mysqlDB.
    Model(&User{}).
    Select("id, name, email").
    Where("age > ?", 18).
    Where("status = ?", "active").
    Order("created_at DESC").
    Limit(10).
    Find(&users)

// In 查询
mysqlDB.Where("id IN ?", []int{1, 2, 3}).Find(&users)

// Like 查询
mysqlDB.Where("name LIKE ?", "%John%").Find(&users)

// Between 查询
mysqlDB.Where("age BETWEEN ? AND ?", 18, 30).Find(&users)

// Null 查询
mysqlDB.Where("deleted_at IS NULL").Find(&users)
```

## 关联关系

### 一对一 (Has One)

```go
type User struct {
    ID     uint    `gorm:"primaryKey"`
    Name   string
    Profile Profile `gorm:"foreignKey:UserID"`
}

type Profile struct {
    ID     uint   `gorm:"primaryKey"`
    UserID uint   `gorm:"uniqueIndex"`
    Bio    string
}

// 查询
var user User
mysqlDB.Preload("Profile").First(&user, 1)

// 创建
user := User{Name: "John"}
user.Profile = Profile{Bio: "Hello World"}
mysqlDB.Create(&user)
```

### 一对多 (Has Many)

```go
type User struct {
    ID     uint    `gorm:"primaryKey"`
    Name   string
    Orders []Order `gorm:"foreignKey:UserID"`
}

type Order struct {
    ID     uint  `gorm:"primaryKey"`
    UserID uint  `gorm:"not null;index"`
    Amount float64
}

// 查询
var user User
mysqlDB.Preload("Orders").First(&user, 1)

// 创建订单
order := Order{UserID: 1, Amount: 100}
mysqlDB.Create(&order)
```

### 多对多 (Many to Many)

```go
type Product struct {
    ID    uint    `gorm:"primaryKey"`
    Name  string
    Tags  []Tag   `gorm:"many2many:product_tags;"`
}

type Tag struct {
    ID       uint       `gorm:"primaryKey"`
    Name     string
    Products []Product  `gorm:"many2many:product_tags;"`
}

// 查询
var product Product
mysqlDB.Preload("Tags").First(&product, 1)

// 创建关联
tag := Tag{Name: "New"}
mysqlDB.Create(&tag)
mysqlDB.Model(&product).Association("Tags").Append(&tag)
```

### 预加载 (Preload)

```go
// 预加载单个关联
mysqlDB.Preload("Orders").First(&user)

// 预加载多个关联
mysqlDB.Preload("Orders").Preload("Profile").First(&user)

// 带条件的预加载
mysqlDB.Preload("Orders", "status = ?", "paid").First(&user)

// 嵌套预加载
mysqlDB.Preload("Orders.Items").First(&user)
```

### 关联操作

```go
// 添加关联
mysqlDB.Model(&user).Association("Orders").Append(&order)

// 移除关联
mysqlDB.Model(&user).Association("Orders").Delete(&order)

// 清空关联
mysqlDB.Model(&user).Association("Orders").Clear()

// 统计关联数量
count := mysqlDB.Model(&user).Association("Orders").Count()
```

## 事务

### 基础事务

```go
// 自动提交/回滚
err := mysqlDB.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&order).Error; err != nil {
        return err // 返回错误会自动回滚
    }

    if err := tx.Create(&payment).Error; err != nil {
        return err
    }

    return nil // 返回 nil 会自动提交
})
```

### 手动事务

```go
tx := mysqlDB.Begin()

// 执行操作
if err := tx.Create(&order).Error; err != nil {
    tx.Rollback()
    return err
}

if err := tx.Create(&payment).Error; err != nil {
    tx.Rollback()
    return err
}

// 提交事务
if err := tx.Commit().Error; err != nil {
    return err
}
```

### 嵌套事务

```go
err := mysqlDB.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&order).Error; err != nil {
        return err
    }

    // 嵌套事务
    return tx.Transaction(func(tx2 *gorm.DB) error {
        return tx2.Create(&payment).Error
    })
})
```

## 高级功能

### 钩子 (Hooks)

```go
type User struct {
    ID        uint
    Name      string
    CreatedAt time.Time
    UpdatedAt time.Time
}

// 创建前
func (u *User) BeforeCreate(tx *gorm.DB) error {
    if u.Name == "" {
        return errors.New("name is required")
    }
    return nil
}

// 创建后
func (u *User) AfterCreate(tx *gorm.DB) error {
    // 发送欢迎邮件
    return nil
}

// 更新前
func (u *User) BeforeUpdate(tx *gorm.DB) error {
    // 验证数据
    return nil
}

// 删除前
func (u *User) BeforeDelete(tx *gorm.DB) error {
    // 清理关联数据
    return nil
}
```

### 作用域 (Scopes)

```go
// 定义作用域
func Age(age int) func(db *gorm.DB) *gorm.DB {
    return func(db *gorm.DB) *gorm.DB {
        return db.Where("age > ?", age)
    }
}

func Status(status string) func(db *gorm.DB) *gorm.DB {
    return func(db *gorm.DB) *gorm.DB {
        return db.Where("status = ?", status)
    }
}

// 使用作用域
var users []User
mysqlDB.Scopes(Age(18), Status("active")).Find(&users)
```

### Upsert

```go
// 插入或更新（基于唯一键）
user := User{Email: "john@example.com", Name: "John"}
err := mysqlDB.Upsert(&user, "email")

// 如果 email 存在则更新，否则插入
```

### 原生 SQL

```go
// 查询
var results []map[string]interface{}
mysqlDB.RawQuery("SELECT * FROM users WHERE age > ?", 18, &results)

// 执行
mysqlDB.Exec("UPDATE users SET status = ? WHERE id IN ?", "active", []int{1, 2, 3})

// 执行存储过程
var result int
mysqlDB.gormDB.Raw("CALL calculate_sum(?, ?)", 1, 2).Scan(&result)
```

### 软删除

```go
// 软删除（实际上是更新 deleted_at）
mysqlDB.Delete(&User{}, 1)

// 查找已删除的记录
var users []User
mysqlDB.gormDB.Unscoped().Find(&users)

// 永久删除
mysqlDB.gormDB.Unscoped().Delete(&User{}, 1)
```

### 批量操作

```go
// 批量插入
users := []User{{Name: "John"}, {Name: "Jane"}}
mysqlDB.BatchCreate(users)

// 批量更新
count, _ := mysqlDB.BatchUpdate(&User{},
    "age > ?", []interface{}{18},
    map[string]interface{}{"status": "active"})

// 批量删除
count, _ := mysqlDB.BatchDelete(&User{},
    "status = ?", []interface{}{"inactive"})
```

## 最佳实践

### 1. 模型设计

```go
// ✅ 推荐：使用结构体标签明确定义
type User struct {
    ID        uint           `gorm:"primaryKey;autoIncrement"`
    Email     string         `gorm:"size:100;uniqueIndex;not null"`
    Name      string         `gorm:"size:100;not null"`
    Status    string         `gorm:"size:20;default:'active';index"`
    CreatedAt time.Time      `gorm:"autoCreateTime"`
    UpdatedAt time.Time      `gorm:"autoUpdateTime"`
    DeletedAt gorm.DeletedAt `gorm:"index"`
}

// ❌ 不推荐：依赖默认值
type User struct {
    ID    uint
    Email string
    Name  string
}
```

### 2. 查询优化

```go
// ✅ 推荐：使用索引字段查询
mysqlDB.Where("email = ?", "test@example.com").First(&user)

// ❌ 不推荐：使用 LIKE 前缀模糊查询
mysqlDB.Where("email LIKE ?", "%test@example.com%").Find(&users)

// ✅ 推荐：使用 Preload 预加载关联
mysqlDB.Preload("Orders").First(&user)

// ❌ 不推荐：N+1 查询
mysqlDB.First(&user)
for _, order := range user.Orders {
    // 每个订单都会触发一次查询
}
```

### 3. 事务使用

```go
// ✅ 推荐：使用自动事务
err := mysqlDB.Transaction(func(tx *gorm.DB) error {
    return tx.Create(&order).Error
})

// ❌ 不推荐：忘记处理错误
tx := mysqlDB.Begin()
tx.Create(&order)
// 忘记 commit 或 rollback
```

### 4. 错误处理

```go
// ✅ 推荐：检查错误
err := mysqlDB.Create(&user)
if err != nil {
    log.Printf("Create failed: %v", err)
    return err
}

// ❌ 不推荐：忽略错误
mysqlDB.Create(&user)
```

### 5. 连接池配置

```go
// ✅ 推荐：根据实际负载配置
config := &config.MySqlDBConfig{
    MaxIdleConns:  10,  // 空闲连接数
    MaxOpenConns:  100, // 最大连接数
}

// ❌ 不推荐：配置不当
config := &config.MySqlDBConfig{
    MaxIdleConns:  1000, // 太大，浪费资源
    MaxOpenConns:  5,    // 太小，性能瓶颈
}
```

### 6. 使用 Preload 避免 N+1

```go
// ✅ 推荐：预加载关联
var users []User
mysqlDB.Preload("Orders").Preload("Profile").Find(&users)

// ❌ 不推荐：循环查询
var users []User
mysqlDB.Find(&users)
for _, user := range users {
    var orders []Order
    mysqlDB.Where("user_id = ?", user.ID).Find(&orders) // N+1
}
```

### 7. 使用事务保证数据一致性

```go
// ✅ 推荐：使用事务
err := mysqlDB.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(&order).Error; err != nil {
        return err
    }
    if err := tx.Model(&product).Update("stock", gorm.Expr("stock - ?", order.Quantity)).Error; err != nil {
        return err
    }
    return nil
})

// ❌ 不推荐：多个操作没有事务
mysqlDB.Create(&order)
mysqlDB.Model(&product).Update("stock", gorm.Expr("stock - ?", order.Quantity))
```

## 常见问题

### 1. 时间字段显示错误

```go
// 设置 DSN 时添加 loc=Local
dsn := "user:password@tcp(host)/dbname?parseTime=true&loc=Local"
```

### 2. 连接池耗尽

```go
// 增加 MaxOpenConns
config.MaxOpenConns = 200

// 或者检查是否有连接泄漏
sqlDB, _ := mysqlDB.GetDB().DB()
stats := sqlDB.Stats()
fmt.Printf("In Use: %d, Idle: %d\n", stats.InUse, stats.Idle)
```

### 3. 查询慢

```go
// 开启日志查看 SQL
mysqlDB.SetLoggerLevel(logger.Info)

// 添加索引
Email string `gorm:"uniqueIndex"`
Status string `gorm:"index"`

// 使用 EXPLAIN 分析
mysqlDB.gormDB.Raw("EXPLAIN SELECT * FROM users WHERE age > ?", 18).Scan(&result)
```

### 4. 事务死锁

```go
// 按固定顺序访问表
// ✅ 正确
tx.Create(&order)
tx.Create(&payment)

// ❌ 可能死锁
tx2.Create(&payment)
tx2.Create(&order)
```

## 更多资源

- [GORM 官方文档](https://gorm.io/docs/)
- [GORM 中文文档](https://gorm.io/zh_CN/docs/)
- [MySQL 官方文档](https://dev.mysql.com/doc/)
