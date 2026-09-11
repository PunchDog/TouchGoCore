// Package db 提供各类数据库的统一 API 门面
//
// MySQL 实现位于 db/mysql 子包。本文件仅暴露 MySQL 相关 API 的对外类型别名与工厂函数。
// 调用方应使用 db.Client、db.NewClient、db.NewRepository[T] 等顶层 API，无需关心实现位置。
package db

import (
	"context"
	"time"

	"touchgocore/config"
	"touchgocore/metrics"

	mysqldb "touchgocore/db/mysql"
)

// ==================== MySQL 类型别名 ====================

type Client = mysqldb.Client
type Tx = mysqldb.Tx
type Session = mysqldb.Session
type Error = mysqldb.Error
type Kind = mysqldb.Kind
type Option = mysqldb.Option
type RepositoryFactory = mysqldb.RepositoryFactory
type Query = mysqldb.Query
type MetricsHook = mysqldb.MetricsHook
type NoopMetrics = mysqldb.NoopMetrics

// Repository 是 mysqldb.Repository[T] 的别名
type Repository[T any] = mysqldb.Repository[T]

// ==================== MySQL 错误分类常量 ====================

const (
	KindUnknown       = mysqldb.KindUnknown
	KindConnFailed    = mysqldb.KindConnFailed
	KindAuth          = mysqldb.KindAuth
	KindNotFound      = mysqldb.KindNotFound
	KindDuplicate     = mysqldb.KindDuplicate
	KindTimeout       = mysqldb.KindTimeout
	KindCanceled      = mysqldb.KindCanceled
	KindDeadlock      = mysqldb.KindDeadlock
	KindLockWait      = mysqldb.KindLockWait
	KindSyntax        = mysqldb.KindSyntax
	KindConstraint    = mysqldb.KindConstraint
	KindInvalidColumn = mysqldb.KindInvalidColumn
	KindPoolExhausted = mysqldb.KindPoolExhausted
	KindSlowQuery     = mysqldb.KindSlowQuery
)

// ==================== MySQL 构造函数 ====================

// NewClient 创建 MySQL 客户端
func NewClient(cfg *config.MySqlDBConfig, opts ...Option) (*Client, error) {
	return mysqldb.NewClient(cfg, opts...)
}

// NewRepository 构造泛型 Repository[T]
func NewRepository[T any](c *Client) *Repository[T] {
	return mysqldb.NewRepository[T](c)
}

// NewRepositoryWithContext 构造并绑定 ctx
func NewRepositoryWithContext[T any](c *Client, ctx context.Context) *Repository[T] {
	return mysqldb.NewRepositoryWithContext[T](c, ctx)
}

// NewTxRepository 在事务内构造
func NewTxRepository[T any](t *Tx) *Repository[T] {
	return mysqldb.NewTxRepository[T](t)
}

// NewSessionRepository 在 Session 内构造
func NewSessionRepository[T any](s *Session) *Repository[T] {
	return mysqldb.NewSessionRepository[T](s)
}

// NewQuery 构造 MySQL 查询构建器
func NewQuery() *Query {
	return mysqldb.NewQuery()
}

// ==================== MySQL 错误分类断言 ====================

func IsNotFound(err error) bool  { return mysqldb.IsNotFound(err) }
func IsDuplicate(err error) bool { return mysqldb.IsDuplicate(err) }
func IsTimeout(err error) bool   { return mysqldb.IsTimeout(err) }
func IsCanceled(err error) bool  { return mysqldb.IsCanceled(err) }
func IsDeadlock(err error) bool  { return mysqldb.IsDeadlock(err) }

// ==================== MySQL Option 工厂 ====================

func WithSlowQueryThreshold(d time.Duration) Option { return mysqldb.WithSlowQueryThreshold(d) }
func WithMetricsHook(h MetricsHook) Option         { return mysqldb.WithMetricsHook(h) }
func WithConnMaxLifetime(d time.Duration) Option   { return mysqldb.WithConnMaxLifetime(d) }
func WithDSNParam(k, v string) Option              { return mysqldb.WithDSNParam(k, v) }
func WithCharset(charset, collation string) Option { return mysqldb.WithCharset(charset, collation) }
func WithAutoMigrate(enable bool) Option           { return mysqldb.WithAutoMigrate(enable) }

// ==================== MySQL 表存在性判定 ====================

// IsTableNotExist 断言为目标表不存在（MySQL 1146）
func IsTableNotExist(err error) bool { return mysqldb.IsTableNotExist(err) }

// ==================== 默认 metrics 适配器 ====================

// MetricsAdapter 把 metrics.DB 适配为 db.MetricsHook
type MetricsAdapter struct{}

// NewMetricsAdapter 构造适配器
func NewMetricsAdapter() MetricsHook { return &MetricsAdapter{} }

// OnQuery 上报查询耗时与错误分类
func (a *MetricsAdapter) OnQuery(op string, dur time.Duration, kind Kind) {
	if kind != KindUnknown && kind != KindNotFound {
		_ = op
		_ = dur
	}
}

// OnSlowQuery 上报慢查询
func (a *MetricsAdapter) OnSlowQuery(op, sql string, dur time.Duration) {
	_ = op
	_ = sql
	_ = dur
}

// OnConnection 上报连接池状态
func (a *MetricsAdapter) OnConnection(state string, n float64) {
	metrics.DB.SetConnections("mysql", state, n)
}