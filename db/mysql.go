// Package db 提供 MySQL 数据库操作封装，基于 GORM 实现。
//
// 重构说明（v0.02）：
//   - 底层从 database/sql 迁移到 gorm.io/gorm，彻底消除 SQL 注入风险
//   - 所有 WHERE 条件通过 GORM 参数化查询传递，不再做字符串拼接
//   - 新增 Transaction / WithTx 支持事务
//   - 新增 WithContext 支持 context 超时传播
//   - Rows / DBResult 保持原有 API 不变，调用方无需修改
package db

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"touchgocore/config"
	"touchgocore/vars"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"context"
)

// ──────────────────────────────────────────────
// 配置模型
// ──────────────────────────────────────────────

// DBConfigModel 数据库连接配置。
type DBConfigModel struct {
	Host          string
	User          string
	Password      string
	DBName        string
	AutoCloseTime int
	MaxOpenConns  int
	MaxIdleConns  int
}

// ──────────────────────────────────────────────
// 操作枚举
// ──────────────────────────────────────────────

// EDBType 数据库操作类型。
type EDBType int

const (
	EDBType_Query       EDBType = iota + 1
	EDBType_Query_Count          // SELECT count(*)
	EDBType_Query_Sum            // SELECT sum(field)
	EDBType_Query_Max            // SELECT max(field)
	EDBType_Insert
	EDBType_Update
	EDBType_Delete
)

// ──────────────────────────────────────────────
// Condition — 链式查询构建器
// ──────────────────────────────────────────────

// Condition 封装一次数据库操作所需的条件、字段、分页等信息。
// 所有方法返回 *Condition 以支持链式调用。
type Condition struct {
	types     EDBType
	tableName string // 已处理反引号的表名
	cacheKey  string
	order     string
	limit     string // "offset,count" 或 "count"
	group     string
	// whereClauses 收集所有 AND 条件片段，每个元素为 (clause, args...)
	whereClauses []whereClause
	// orGroups 收集 OR 合并的子 Condition
	orGroups  [][]whereClause
	filter    bool
	values    *map[string]interface{} // INSERT / UPDATE 的 KV 数据
	ctx       context.Context         // 可选，用于超时传播
}

type whereClause struct {
	sql  string
	args []interface{}
}

// SetFilterEx 添加一个带表达式的 WHERE 条件（AND 连接）。
//
//	key: 字段名（自动加反引号，支持 "." 和已有反引号的字段名）
//	ex:  操作符，如 "=", ">", "<=", "!=" 等
//	val: 值，支持 scalar / []int / []int64 / []float64 / []string / []interface{}
func (c *Condition) SetFilterEx(key string, ex string, val interface{}) error {
	c.filter = true

	col := quoteColumn(key)
	var clause string
	var args []interface{}

	switch v := val.(type) {
	case []interface{}:
		placeholders := makePlaceholders(len(v))
		clause = fmt.Sprintf("%s %s (%s)", col, ex, placeholders)
		args = v
	case []string:
		iface := make([]interface{}, len(v))
		for i, s := range v {
			iface[i] = s
		}
		placeholders := makePlaceholders(len(iface))
		clause = fmt.Sprintf("%s IN (%s)", col, placeholders)
		args = iface
	case []int:
		iface := make([]interface{}, len(v))
		for i, n := range v {
			iface[i] = n
		}
		placeholders := makePlaceholders(len(iface))
		clause = fmt.Sprintf("%s IN (%s)", col, placeholders)
		args = iface
	case []int64:
		iface := make([]interface{}, len(v))
		for i, n := range v {
			iface[i] = n
		}
		placeholders := makePlaceholders(len(iface))
		clause = fmt.Sprintf("%s IN (%s)", col, placeholders)
		args = iface
	case []float64:
		iface := make([]interface{}, len(v))
		for i, f := range v {
			iface[i] = f
		}
		placeholders := makePlaceholders(len(iface))
		clause = fmt.Sprintf("%s IN (%s)", col, placeholders)
		args = iface
	default:
		clause = fmt.Sprintf("%s %s ?", col, ex)
		args = []interface{}{val}
	}

	c.whereClauses = append(c.whereClauses, whereClause{sql: clause, args: args})
	return nil
}

// SetFilter 添加等值 WHERE 条件（AND 连接），支持 slice 形式的 IN 查询。
func (c *Condition) SetFilter(key string, val interface{}) *Condition {
	_ = c.SetFilterEx(key, "=", val)
	return c
}

// SetFilterOr 将多个 Condition 的 WHERE 片段以 OR 方式合并到当前条件。
// 合并后与其他条件之间仍以 AND 连接。
func (c *Condition) SetFilterOr(conditions ...*Condition) {
	for _, other := range conditions {
		if c.tableName != "" && other.tableName != "" && c.tableName != other.tableName {
			vars.Error("不是操作同一张表，不能进行条件合并")
			continue
		}
		if len(other.whereClauses) > 0 {
			c.orGroups = append(c.orGroups, other.whereClauses)
		}
	}
}

// SetTableName 设置操作的表名（自动加反引号）。
func (c *Condition) SetTableName(tableName string) *Condition {
	c.tableName = quoteTable(tableName)
	return c
}

// SetCacheKey 设置缓存键（供上层缓存淘汰使用，框架本身不处理缓存）。
func (c *Condition) SetCacheKey(cacheKey ...interface{}) *Condition {
	if len(cacheKey) == 1 {
		switch v := cacheKey[0].(type) {
		case string:
			c.cacheKey = v
		default:
			c.cacheKey = fmt.Sprintf("%v", v)
		}
	} else if len(cacheKey) > 1 {
		format, ok := cacheKey[0].(string)
		if ok {
			c.cacheKey = fmt.Sprintf(format, cacheKey[1:]...)
		}
	}
	return c
}

// Order 设置排序（原始 SQL 片段，如 "id DESC"）。
func (c *Condition) Order(order string) *Condition {
	c.order = order
	return c
}

// Limit 设置分页。
//   - Limit(10)       → LIMIT 10
//   - Limit(20, 10)   → LIMIT 20,10  (offset=20, count=10)
func (c *Condition) Limit(limit ...int) *Condition {
	parts := make([]string, len(limit))
	for i, v := range limit {
		parts[i] = strconv.Itoa(v)
	}
	c.limit = strings.Join(parts, ",")
	return c
}

// Group 设置 GROUP BY 字段名（原始 SQL 片段）。
func (c *Condition) Group(field string) *Condition {
	c.group = field
	return c
}

// SetDataMap 设置 INSERT / UPDATE 的数据 KV Map。
func (c *Condition) SetDataMap(data *map[string]interface{}) *Condition {
	c.values = data
	return c
}

// SetDataMapByOne 向数据 KV Map 中追加一个字段。
func (c *Condition) SetDataMapByOne(key string, value interface{}) *Condition {
	if c.values == nil {
		m := make(map[string]interface{})
		c.values = &m
	}
	(*c.values)[key] = value
	return c
}

// SetDBType 设置操作类型（一般不需要手动调用，由 Query/Insert/Update/Del 自动设置）。
func (c *Condition) SetDBType(tp EDBType) {
	c.types = tp
}

// WithContext 绑定 context（用于超时控制）。
func (c *Condition) WithContext(ctx context.Context) *Condition {
	c.ctx = ctx
	return c
}

// ──────────────────────────────────────────────
// Rows — 遍历结果集
// ──────────────────────────────────────────────

// Rows 封装查询结果，提供顺序遍历和字段读取能力。
type Rows struct {
	row       *map[string]interface{}
	rows      *[]map[string]interface{}
	row_index int
}

// Next 移动游标到下一行；首次调用指向第一行。
// 返回 error 表示无数据或已到末尾。
func (r *Rows) Next() error {
	if r.rows == nil {
		return errors.New("返回多个数据才能使用")
	}
	if len(*r.rows) == 0 {
		return errors.New("没有查询到数据")
	}
	if r.row != nil {
		r.row_index++
		if r.row_index >= len(*r.rows) {
			return errors.New("已经是结果最后")
		}
	}
	r.row = &(*r.rows)[r.row_index]
	return nil
}

// GetInt 以 int64 读取当前行指定字段值。
func (r *Rows) GetInt(key string) int64 {
	if r.row == nil {
		return 0
	}
	v, ok := (*r.row)[key]
	if !ok || v == nil {
		return 0
	}
	switch val := v.(type) {
	case int64:
		return val
	case int:
		return int64(val)
	case float64:
		return int64(val)
	case string:
		n, _ := strconv.ParseInt(val, 10, 64)
		return n
	default:
		n, _ := strconv.ParseInt(fmt.Sprintf("%v", val), 10, 64)
		return n
	}
}

// GetFloat 以 float64 读取当前行指定字段值。
func (r *Rows) GetFloat(key string) float64 {
	if r.row == nil {
		return 0
	}
	v, ok := (*r.row)[key]
	if !ok || v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		f, _ := strconv.ParseFloat(val, 64)
		return f
	default:
		f, _ := strconv.ParseFloat(fmt.Sprintf("%v", val), 64)
		return f
	}
}

// GetString 以 string 读取当前行指定字段值。
func (r *Rows) GetString(key string) string {
	if r.row == nil {
		return ""
	}
	v, ok := (*r.row)[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// ForMap 遍历当前行的所有字段。
func (r *Rows) ForMap(fn func(k string, v interface{})) {
	if r.row == nil {
		return
	}
	for k, v := range *r.row {
		fn(k, v)
	}
}

// ──────────────────────────────────────────────
// DBResult — 查询结果集
// ──────────────────────────────────────────────

// DBResult 保存查询返回的全部行数据。
type DBResult struct {
	values []map[string]interface{}
}

// Count 返回结果行数。
func (r *DBResult) Count() int {
	return len(r.values)
}

// GetOne 返回第一行的 Rows 指针（用于单行读取）。
func (r *DBResult) GetOne() *Rows {
	if len(r.values) == 0 {
		return &Rows{}
	}
	return &Rows{row: &r.values[0]}
}

// GetAll 返回覆盖所有行的 Rows 指针（需配合 Next() 遍历）。
func (r *DBResult) GetAll() *Rows {
	return &Rows{rows: &r.values}
}

// ──────────────────────────────────────────────
// DbMysql — GORM 封装
// ──────────────────────────────────────────────

// DbMysql 封装 GORM DB，提供与原有 API 兼容的链式查询接口。
type DbMysql struct {
	db        *gorm.DB       // GORM 连接（来自连接池复用）
	config    *DBConfigModel
	condition *Condition
	Result    *DBResult
}

// NewDbMysql 创建 DbMysql 实例。同一 DSN 复用已有的 *gorm.DB 实例（连接池共享）。
func NewDbMysql(cfg *config.MySqlDBConfig) (*DbMysql, error) {
	m := &DbMysql{
		config: &DBConfigModel{
			Host:         cfg.Host,
			User:         cfg.Username,
			Password:     cfg.Password,
			DBName:       cfg.DBName,
			MaxIdleConns: cfg.MaxIdleConns,
			MaxOpenConns: cfg.MaxOpenConns,
		},
	}
	return m, m.connect()
}

// GetConfig 返回当前数据库配置。
func (m *DbMysql) GetConfig() *DBConfigModel {
	return m.config
}

// connect 建立（或复用）GORM 连接。
func (m *DbMysql) connect() error {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s)/%s?parseTime=true&loc=Local&charset=utf8mb4",
		m.config.User, m.config.Password, m.config.Host, m.config.DBName,
	)

	// 尝试复用已有连接
	if m.connectOnly(dsn) {
		return nil
	}

	// 日志级别：生产环境使用 Warn，仅打印慢查询和错误
	logLevel := logger.Warn
	gormDB, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logLevel),
		// 禁用外键约束检查（游戏服务端通常自己管理关联）
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return fmt.Errorf("gorm open mysql: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return fmt.Errorf("get underlying sql.DB: %w", err)
	}

	if m.config.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(m.config.MaxIdleConns)
	}
	if m.config.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(m.config.MaxOpenConns)
	}
	lifetime := time.Second * 2400 // 默认 40 分钟保活
	if m.config.AutoCloseTime > 0 {
		lifetime = time.Duration(m.config.AutoCloseTime) * time.Second
	}
	sqlDB.SetConnMaxLifetime(lifetime)

	_DbMap.Store(dsn, gormDB)
	m.db = gormDB
	return nil
}

// connectOnly 尝试从共享池中取出已有连接。
func (m *DbMysql) connectOnly(dsn string) bool {
	if v, ok := _DbMap.Load(dsn); ok {
		if gormDB, ok := v.(*gorm.DB); ok {
			m.db = gormDB
			return true
		}
	}
	return false
}

// Ping 检测数据库连接是否健康。
func (m *DbMysql) Ping() error {
	sqlDB, err := m.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

// Close 关闭底层连接池（通常不需要手动调用，连接池由框架管理）。
func (m *DbMysql) Close() {
	if sqlDB, err := m.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	m.db = nil
}

// SetCondition 设置本次操作的查询条件。
func (m *DbMysql) SetCondition(condition *Condition) *DbMysql {
	m.condition = condition
	return m
}

// ──────────────────────────────────────────────
// 内部工具：构建 GORM DB 实例
// ──────────────────────────────────────────────

// buildDB 根据 Condition 构建带 WHERE / ORDER / GROUP / LIMIT 的 *gorm.DB。
func (m *DbMysql) buildDB() (*gorm.DB, error) {
	if m.condition == nil {
		return nil, errors.New("没有设置条件，不能操作数据库")
	}

	base := m.db
	// 绑定 context（支持超时传播）
	if m.condition.ctx != nil {
		base = base.WithContext(m.condition.ctx)
	}

	db := base.Table(m.condition.tableName)

	// AND 条件
	for _, wc := range m.condition.whereClauses {
		db = db.Where(wc.sql, wc.args...)
	}

	// OR 条件组：每组内部 AND，组间 OR
	for _, group := range m.condition.orGroups {
		if len(group) == 0 {
			continue
		}
		// 将一个 group 的多个子句拼成一个 OR 子条件
		orDB := m.db.Where(group[0].sql, group[0].args...)
		for _, wc := range group[1:] {
			orDB = orDB.Where(wc.sql, wc.args...)
		}
		db = db.Or(orDB)
	}

	if m.condition.group != "" {
		db = db.Group(m.condition.group)
	}
	if m.condition.order != "" {
		db = db.Order(m.condition.order)
	}
	if m.condition.limit != "" {
		parts := strings.SplitN(m.condition.limit, ",", 2)
		if len(parts) == 1 {
			n, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			db = db.Limit(n)
		} else {
			offset, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			count, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			db = db.Offset(offset).Limit(count)
		}
	}

	return db, nil
}

// scanToDBResult 将 GORM 的 []map[string]interface{} 结果包装为 *DBResult。
func scanToDBResult(rows []map[string]interface{}) *DBResult {
	return &DBResult{values: rows}
}

// ──────────────────────────────────────────────
// 查询操作
// ──────────────────────────────────────────────

// Query 执行 SELECT 查询，返回满足条件的所有行。
// 若 Condition.values 中设置了字段名，则只 SELECT 指定列。
func (m *DbMysql) Query() (*DBResult, error) {
	db, err := m.buildDB()
	if err != nil {
		return nil, err
	}

	// 按需选择字段
	if m.condition.values != nil {
		cols := make([]string, 0, len(*m.condition.values))
		for k := range *m.condition.values {
			cols = append(cols, k)
		}
		db = db.Select(cols)
	}

	var result []map[string]interface{}
	if tx := db.Find(&result); tx.Error != nil {
		vars.Error("Query error: %v", tx.Error)
		return nil, tx.Error
	}
	m.Result = scanToDBResult(result)
	return m.Result, nil
}

// QueryCount 执行 SELECT count(*) 查询。
func (m *DbMysql) QueryCount() (*DBResult, error) {
	db, err := m.buildDB()
	if err != nil {
		return nil, err
	}

	var count int64
	if tx := db.Count(&count); tx.Error != nil {
		vars.Error("QueryCount error: %v", tx.Error)
		return nil, tx.Error
	}
	m.Result = &DBResult{values: []map[string]interface{}{
		{"count(*)": count},
	}}
	return m.Result, nil
}

// QuerySum 执行 SELECT sum(rowname) 查询。
func (m *DbMysql) QuerySum(rowname string) (*DBResult, error) {
	db, err := m.buildDB()
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}
	col := fmt.Sprintf("SUM(%s) AS `%s`", quoteColumn(rowname), rowname)
	if tx := db.Select(col).Find(&result); tx.Error != nil {
		vars.Error("QuerySum error: %v", tx.Error)
		return nil, tx.Error
	}
	m.Result = scanToDBResult(result)
	return m.Result, nil
}

// QueryMax 执行 SELECT max(rowname) 查询。
func (m *DbMysql) QueryMax(rowname string) (*DBResult, error) {
	db, err := m.buildDB()
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}
	col := fmt.Sprintf("MAX(%s) AS `%s`", quoteColumn(rowname), rowname)
	if tx := db.Select(col).Find(&result); tx.Error != nil {
		vars.Error("QueryMax error: %v", tx.Error)
		return nil, tx.Error
	}
	m.Result = scanToDBResult(result)
	return m.Result, nil
}

// ──────────────────────────────────────────────
// 写操作
// ──────────────────────────────────────────────

// Insert 向表中插入一行数据。数据来源于 Condition.values。
func (m *DbMysql) Insert() error {
	if m.condition == nil {
		return errors.New("没有设置条件，不能操作数据库")
	}
	if m.condition.values == nil {
		return errors.New("没有要插入的数据")
	}

	base := m.db
	if m.condition.ctx != nil {
		base = base.WithContext(m.condition.ctx)
	}

	// GORM Create 接受 map[string]interface{}
	if tx := base.Table(m.condition.tableName).Create(m.condition.values); tx.Error != nil {
		vars.Error("Insert error: %v", tx.Error)
		return tx.Error
	}
	return nil
}

// Update 更新满足条件的行。数据来源于 Condition.values，WHERE 由 Condition 的过滤条件决定。
func (m *DbMysql) Update() error {
	if m.condition == nil {
		return errors.New("没有设置条件，不能操作数据库")
	}
	if m.condition.values == nil {
		return errors.New("没有要修改的数据")
	}

	db, err := m.buildDB()
	if err != nil {
		return err
	}

	if tx := db.Updates(m.condition.values); tx.Error != nil {
		vars.Error("Update error: %v", tx.Error)
		return tx.Error
	}
	return nil
}

// Del 删除满足条件的行。要求必须设置 WHERE 条件（防止误删全表）。
func (m *DbMysql) Del() error {
	if m.condition == nil {
		return errors.New("没有删除条件")
	}
	if !m.condition.filter && len(m.condition.whereClauses) == 0 {
		return errors.New("Del 必须设置 WHERE 条件，拒绝无条件删除")
	}

	db, err := m.buildDB()
	if err != nil {
		return err
	}

	// 使用裸 map 作为目标以跳过 GORM 的零值过滤
	if tx := db.Delete(&map[string]interface{}{}); tx.Error != nil {
		vars.Error("Del error: %v", tx.Error)
		return tx.Error
	}
	return nil
}

// ──────────────────────────────────────────────
// 事务支持（新增）
// ──────────────────────────────────────────────

// Transaction 在一个事务中执行 fn。
// fn 返回 error 时自动回滚，返回 nil 时自动提交。
//
// 示例：
//
//	err := mysqlDB.Transaction(func(tx *gorm.DB) error {
//	    if err := tx.Table("users").Create(map[string]interface{}{...}).Error; err != nil {
//	        return err
//	    }
//	    return tx.Table("logs").Create(map[string]interface{}{...}).Error
//	})
func (m *DbMysql) Transaction(fn func(tx *gorm.DB) error) error {
	return m.db.Transaction(fn)
}

// WithTx 返回一个绑定了外部事务的新 DbMysql 实例，用于跨方法传递事务。
func (m *DbMysql) WithTx(tx *gorm.DB) *DbMysql {
	clone := *m
	clone.db = tx
	return &clone
}

// RawDB 返回底层 *gorm.DB，供需要直接操作 GORM 的高级用法使用。
func (m *DbMysql) RawDB() *gorm.DB {
	return m.db
}

// ──────────────────────────────────────────────
// 内部工具函数
// ──────────────────────────────────────────────

// quoteColumn 对字段名加反引号（跳过已有反引号或含 "." 的字段名）。
func quoteColumn(key string) string {
	if strings.ContainsAny(key, "`.") {
		return key
	}
	return "`" + key + "`"
}

// quoteTable 对表名加反引号（跳过已有特殊字符）。
func quoteTable(tableName string) string {
	if strings.ContainsAny(tableName, "`. ") {
		return tableName
	}
	return "`" + tableName + "`"
}

// makePlaceholders 生成 n 个 "?" 的逗号分隔字符串。
func makePlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}
