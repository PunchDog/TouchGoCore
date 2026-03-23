package db

import (
	"fmt"
	"time"

	"touchgocore/config"
	"touchgocore/syncmap"
	"touchgocore/vars"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// DbMysql 封装 GORM MySQL 数据库操作
type DbMysql struct {
	gormDB *gorm.DB
	config *config.MySqlDBConfig
}

// 全局 GORM 连接池
// key: 数据库连接字符串, value: *gorm.DB
var gormPool *syncmap.Map = &syncmap.Map{}

// NewDbMysql 创建 MySQL 数据库连接
func NewDbMysql(cfg *config.MySqlDBConfig) (*DbMysql, error) {
	dsn := buildDSN(cfg)

	// 尝试从连接池获取
	if db, ok := gormPool.Load(dsn); ok {
		return &DbMysql{gormDB: db.(*gorm.DB), config: cfg}, nil
	}

	// 创建新连接
	gormDB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // 默认静默，可通过 SetLoggerLevel 调整
	})
	if err != nil {
		vars.Error("%v", err)
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	// 配置连接池
	sqlDB, err := gormDB.DB()
	if err != nil {
		vars.Error("%v", err)
		return nil, fmt.Errorf("failed to get sql.DB: %w", err)
	}

	if cfg.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	sqlDB.SetConnMaxLifetime(time.Hour * 24) // 连接最大生存时间24小时

	// 缓存到连接池
	gormPool.Store(dsn, gormDB)

	return &DbMysql{gormDB: gormDB, config: cfg}, nil
}

// buildDSN 构建数据源名称
func buildDSN(cfg *config.MySqlDBConfig) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&loc=Local&charset=utf8mb4&collation=utf8mb4_unicode_ci",
		cfg.Username, cfg.Password, cfg.Host, cfg.DBName)
}

// GetDB 获取 GORM 实例
// 使用方式: mysqlDB.GetDB().Model(&User{}).Where("id = ?", id).First(&user)
func (m *DbMysql) GetDB() *gorm.DB {
	return m.gormDB
}

// Config 获取配置
func (m *DbMysql) Config() *config.MySqlDBConfig {
	return m.config
}

// Ping 检查数据库连接
func (m *DbMysql) Ping() error {
	sqlDB, err := m.gormDB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

// Close 关闭数据库连接
func (m *DbMysql) Close() error {
	sqlDB, err := m.gormDB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// SetLoggerLevel 设置日志级别
// logger.Silent - 静默
// logger.Error - 错误
// logger.Warn - 警告
// logger.Info - 信息
func (m *DbMysql) SetLoggerLevel(level logger.LogLevel) {
	m.gormDB.Logger = logger.Default.LogMode(level)
}

// Transaction 执行事务
// 示例:
//   err := mysqlDB.Transaction(func(tx *gorm.DB) error {
//       if err := tx.Create(&order).Error; err != nil {
//           return err
//       }
//       return tx.Model(&product).Update("stock", gorm.Expr("stock - ?", order.Quantity)).Error
//   })
func (m *DbMysql) Transaction(fn func(tx *gorm.DB) error) error {
	return m.gormDB.Transaction(fn)
}

// AutoMigrate 自动迁移表结构
// 示例: mysqlDB.AutoMigrate(&User{}, &Order{})
func (m *DbMysql) AutoMigrate(models ...interface{}) error {
	return m.gormDB.AutoMigrate(models...)
}

// Create 创建记录
// 示例: mysqlDB.Create(&User{Name: "John", Age: 30})
func (m *DbMysql) Create(value interface{}) error {
	return m.gormDB.Create(value).Error
}

// BatchCreate 批量创建记录
// 示例: mysqlDB.BatchCreate([]*User{{Name: "John"}, {Name: "Jane"}})
func (m *DbMysql) BatchCreate(values interface{}) error {
	return m.gormDB.Create(values).Error
}

// Find 查询多条记录
// 示例: var users []User; mysqlDB.Find(&users, "age > ?", 18)
func (m *DbMysql) Find(dest interface{}, conds ...interface{}) error {
	return m.gormDB.Find(dest, conds...).Error
}

// First 查询第一条记录（按主键）
// 示例: var user User; mysqlDB.First(&user, 1)
func (m *DbMysql) First(dest interface{}, conds ...interface{}) error {
	return m.gormDB.First(dest, conds...).Error
}

// Take 查询一条记录（无排序）
// 示例: var user User; mysqlDB.Take(&user, "name = ?", "John")
func (m *DbMysql) Take(dest interface{}, conds ...interface{}) error {
	return m.gormDB.Take(dest, conds...).Error
}

// Last 查询最后一条记录（按主键倒序）
// 示例: var user User; mysqlDB.Last(&user)
func (m *DbMysql) Last(dest interface{}, conds ...interface{}) error {
	return m.gormDB.Last(dest, conds...).Error
}

// Updates 更新记录
// 示例: mysqlDB.Updates(&User{ID: 1, Name: "NewName"})
func (m *DbMysql) Updates(values interface{}) error {
	return m.gormDB.Updates(values).Error
}

// Update 更新单个字段
// 示例: mysqlDB.Update("name", "NewName")
func (m *DbMysql) Update(column string, value interface{}) error {
	return m.gormDB.Update(column, value).Error
}

// UpdateWithConditions 根据条件更新
// 示例: mysqlDB.UpdateWithConditions(&User{}, "age > ?", 18, map[string]interface{}{"status": "inactive"})
func (m *DbMysql) UpdateWithConditions(model interface{}, whereClause interface{}, args []interface{}, updates map[string]interface{}) error {
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	return query.Updates(updates).Error
}

// Delete 删除记录
// 示例: mysqlDB.Delete(&User{}, 1)
func (m *DbMysql) Delete(value interface{}, conds ...interface{}) error {
	return m.gormDB.Delete(value, conds...).Error
}

// DeleteWithConditions 根据条件删除
// 示例: mysqlDB.DeleteWithConditions(&User{}, "age > ?", []interface{}{18})
func (m *DbMysql) DeleteWithConditions(model interface{}, whereClause interface{}, args []interface{}) error {
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	return query.Delete(nil).Error
}

// Count 统计记录数
// 示例: count, _ := mysqlDB.Count(&User{}, "age > ?", 18)
func (m *DbMysql) Count(model interface{}, whereClause interface{}, args ...interface{}) (int64, error) {
	var count int64
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Count(&count).Error
	return count, err
}

// Exists 检查记录是否存在
// 示例: exists, _ := mysqlDB.Exists(&User{}, "email = ?", "test@example.com")
func (m *DbMysql) Exists(model interface{}, whereClause interface{}, args ...interface{}) (bool, error) {
	var count int64
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Count(&count).Error
	return count > 0, err
}

// Sum 计算字段总和
// 示例: total, _ := mysqlDB.Sum(&Order{}, "amount", "status = ?", "paid")
func (m *DbMysql) Sum(model interface{}, column string, whereClause interface{}, args ...interface{}) (float64, error) {
	var result float64
	query := m.gormDB.Model(model).Select(fmt.Sprintf("COALESCE(SUM(%s), 0)", column))
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Row().Scan(&result)
	return result, err
}

// Avg 计算字段平均值
// 示例: avg, _ := mysqlDB.Avg(&Order{}, "amount")
func (m *DbMysql) Avg(model interface{}, column string, whereClause interface{}, args ...interface{}) (float64, error) {
	var result float64
	query := m.gormDB.Model(model).Select(fmt.Sprintf("COALESCE(AVG(%s), 0)", column))
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Row().Scan(&result)
	return result, err
}

// Max 获取字段最大值
// 示例: max, _ := mysqlDB.Max(&Order{}, "amount")
func (m *DbMysql) Max(model interface{}, column string, whereClause interface{}, args ...interface{}) (interface{}, error) {
	var result interface{}
	query := m.gormDB.Model(model).Select(fmt.Sprintf("MAX(%s)", column))
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Row().Scan(&result)
	return result, err
}

// Min 获取字段最小值
// 示例: min, _ := mysqlDB.Min(&Order{}, "amount")
func (m *DbMysql) Min(model interface{}, column string, whereClause interface{}, args ...interface{}) (interface{}, error) {
	var result interface{}
	query := m.gormDB.Model(model).Select(fmt.Sprintf("MIN(%s)", column))
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	err := query.Row().Scan(&result)
	return result, err
}

// RawQuery 执行原生 SQL 查询
// 示例: var results []map[string]interface{}; mysqlDB.RawQuery("SELECT * FROM users WHERE age > ?", 18, &results)
func (m *DbMysql) RawQuery(sql string, args []interface{}, dest interface{}) error {
	return m.gormDB.Raw(sql, args...).Scan(dest).Error
}

// Exec 执行原生 SQL（不返回结果）
// 示例: mysqlDB.Exec("UPDATE users SET status = ? WHERE id IN ?", "active", []int{1, 2, 3})
func (m *DbMysql) Exec(sql string, args ...interface{}) error {
	return m.gormDB.Exec(sql, args...).Error
}

// Pluck 查询单个列的值
// 示例: var ages []int64; mysqlDB.Pluck(&User{}, "age", &ages)
func (m *DbMysql) Pluck(model interface{}, column string, dest interface{}) error {
	return m.gormDB.Model(model).Pluck(column, dest).Error
}

// Paginate 分页查询
// page: 页码（从1开始）
// pageSize: 每页数量
// 返回: 总记录数, 当前页数据, 错误
// 示例: total, users, err := mysqlDB.Paginate(&User{}, 1, 20, "age > ?", []interface{}{18}, "created_at DESC")
func (m *DbMysql) Paginate(model interface{}, page, pageSize int, whereClause interface{}, args []interface{}, order string, dest interface{}) (int64, error) {
	query := m.gormDB.Model(model)

	// 应用 WHERE 条件
	if whereClause != nil {
		if len(args) > 0 {
			query = query.Where(whereClause, args...)
		} else {
			query = query.Where(whereClause)
		}
	}

	// 应用排序
	if order != "" {
		query = query.Order(order)
	}

	// 获取总数
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return 0, err
	}

	// 应用分页
	offset := (page - 1) * pageSize
	if err := query.Offset(offset).Limit(pageSize).Find(dest).Error; err != nil {
		return 0, err
	}

	return total, nil
}

// BatchUpdate 批量更新
// 示例: mysqlDB.BatchUpdate(&User{}, "status = ?", []interface{}{"active"}, map[string]interface{}{"updated_at": time.Now()})
func (m *DbMysql) BatchUpdate(model interface{}, whereClause interface{}, args []interface{}, updates map[string]interface{}) (int64, error) {
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	result := query.Updates(updates)
	return result.RowsAffected, result.Error
}

// BatchDelete 批量删除
// 返回: 删除的记录数, 错误
// 示例: count, err := mysqlDB.BatchDelete(&User{}, "age < ?", []interface{}{18})
func (m *DbMysql) BatchDelete(model interface{}, whereClause interface{}, args []interface{}) (int64, error) {
	query := m.gormDB.Model(model)
	if len(args) > 0 {
		query = query.Where(whereClause, args...)
	} else {
		query = query.Where(whereClause)
	}
	result := query.Delete(nil)
	return result.RowsAffected, result.Error
}

// Upsert 插入或更新（MySQL 的 ON DUPLICATE KEY UPDATE）
// 示例: mysqlDB.Upsert(&User{Name: "John", Email: "john@example.com"}, "email")
func (m *DbMysql) Upsert(model interface{}, conflictColumns ...string) error {
	if len(conflictColumns) == 0 {
		return m.gormDB.Create(model).Error
	}

	query := m.gormDB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: conflictColumns[0]}},
		UpdateAll: true,
	})
	return query.Create(model).Error
}

// Where 创建带条件的查询链（方便链式调用）
// 示例:
//   var users []User
//   mysqlDB.Where("age > ?", 18).Where("status = ?", "active").Order("created_at DESC").Find(&users)
func (m *DbMysql) Where(query interface{}, args ...interface{}) *gorm.DB {
	return m.gormDB.Where(query, args...)
}

// Order 创建带排序的查询链
// 示例: mysqlDB.Order("created_at DESC").Find(&users)
func (m *DbMysql) Order(value interface{}) *gorm.DB {
	return m.gormDB.Order(value)
}

// Limit 创建带限制的查询链
// 示例: mysqlDB.Limit(10).Find(&users)
func (m *DbMysql) Limit(limit int) *gorm.DB {
	return m.gormDB.Limit(limit)
}

// Offset 创建带偏移的查询链
// 示例: mysqlDB.Offset(10).Limit(10).Find(&users)
func (m *DbMysql) Offset(offset int) *gorm.DB {
	return m.gormDB.Offset(offset)
}

// Group 创建带分组的查询链
// 示例: var results []map[string]interface{}; mysqlDB.Group("status").Select("status, COUNT(*) as count").Find(&results)
func (m *DbMysql) Group(name string) *gorm.DB {
	return m.gormDB.Group(name)
}

// Having 创建带 Having 条件的查询链
// 示例: mysqlDB.Group("status").Having("COUNT(*) > ?", 10).Find(&results)
func (m *DbMysql) Having(query interface{}, args ...interface{}) *gorm.DB {
	return m.gormDB.Having(query, args...)
}

// Select 创建带字段选择的查询链
// 示例: mysqlDB.Select("id, name").Find(&users)
func (m *DbMysql) Select(query interface{}, args ...interface{}) *gorm.DB {
	return m.gormDB.Select(query, args...)
}

// Distinct 去重
// 示例: mysqlDB.Distinct("status").Find(&statuses)
func (m *DbMysql) Distinct(args ...interface{}) *gorm.DB {
	return m.gormDB.Distinct(args...)
}

// Joins 关联查询
// 示例: mysqlDB.Joins("LEFT JOIN orders ON users.id = orders.user_id").Find(&results)
func (m *DbMysql) Joins(query string, args ...interface{}) *gorm.DB {
	return m.gormDB.Joins(query, args...)
}

// Preload 预加载关联（用于模型关联）
// 示例: var user User; mysqlDB.Preload("Orders").First(&user, 1)
func (m *DbMysql) Preload(query string, args ...interface{}) *gorm.DB {
	return m.gormDB.Preload(query, args...)
}

// Scopes 应用作用域
// 示例: mysqlDB.Scopes(Age(18), Active()).Find(&users)
func (m *DbMysql) Scopes(funcs ...func(*gorm.DB) *gorm.DB) *gorm.DB {
	return m.gormDB.Scopes(funcs...)
}

// Model 指定模型
// 示例: mysqlDB.Model(&User{}).Where("age > ?", 18).Count(&count)
func (m *DbMysql) Model(value interface{}) *gorm.DB {
	return m.gormDB.Model(value)
}

// Table 指定表名
// 示例: mysqlDB.Table("users").Where("age > ?", 18).Find(&results)
func (m *DbMysql) Table(name string, args ...interface{}) *gorm.DB {
	return m.gormDB.Table(name, args...)
}

// Begin 开始事务
// 示例: tx := mysqlDB.Begin()
func (m *DbMysql) Begin() *gorm.DB {
	return m.gormDB.Begin()
}

// Commit 提交事务
// 示例: tx.Commit()
func (m *DbMysql) Commit(tx *gorm.DB) error {
	return tx.Commit().Error
}

// Rollback 回滚事务
// 示例: tx.Rollback()
func (m *DbMysql) Rollback(tx *gorm.DB) error {
	return tx.Rollback().Error
}

// GetStats 获取连接池统计信息
// 返回: 最大打开连接数, 当前空闲连接数, 当前使用连接数, 总等待次数, 总等待时间, 空闲连接数
func (m *DbMysql) GetStats() (maxOpen, open, inUse, waitCount, waitDuration, idle int) {
	sqlDB, err := m.gormDB.DB()
	if err != nil {
		return
	}
	stats := sqlDB.Stats()
	return stats.MaxOpenConnections, stats.OpenConnections, stats.InUse, int(stats.WaitCount), int(stats.WaitDuration.Nanoseconds() / 1000000), stats.Idle
}
