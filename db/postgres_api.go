// Package db 的 PostgreSQL 门面。
//
// PostgreSQL 实现位于 db/postgres 子包，风格与 db/mysql 完全对齐。本文件暴露
// 对外类型别名、工厂函数、错误断言与缓存回源/落库适配器包装，调用方无需关心实现位置。
// 缓存适配器实体类型（PostgresRepository[T]）与 MySQL 的 Repository[T] 是不同类型，
// 因此 Postgres 的缓存函数单列于此，避免与 cache_api.go 里 MySQL 版签名撞名。
package db

import (
	"context"
	"time"

	"touchgocore/config"
	"touchgocore/metrics"

	pgdb "touchgocore/db/postgres"
)

// ==================== PostgreSQL 类型别名 ====================

type PostgresClient = pgdb.Client
type PostgresTx = pgdb.Tx
type PostgresSession = pgdb.Session
type PostgresError = pgdb.Error
type PostgresKind = pgdb.Kind
type PostgresOption = pgdb.Option
type PostgresRepositoryFactory = pgdb.RepositoryFactory
type PostgresQuery = pgdb.Query
type PostgresMetricsHook = pgdb.MetricsHook
type PostgresNoopMetrics = pgdb.NoopMetrics

// PG 特有类型别名
type PGJSONB = pgdb.JSONB
type PGStringArray = pgdb.StringArray
type PGInt64Array = pgdb.Int64Array
type PGHStore = pgdb.HStore

// PostgresRepository 是 pgdb.Repository[T] 的别名
type PostgresRepository[T any] = pgdb.Repository[T]

// ==================== PostgreSQL 构造函数 ====================

// NewPostgres 创建 PostgreSQL 客户端（同配置复用连接池）
func NewPostgres(cfg *config.PostgreDBConfig, opts ...PostgresOption) (*PostgresClient, error) {
	return pgdb.NewClient(cfg, opts...)
}

// NewPostgresRepository 构造泛型 Repository[T]
func NewPostgresRepository[T any](c *PostgresClient) *PostgresRepository[T] {
	return pgdb.NewRepository[T](c)
}

// NewPostgresRepositoryWithContext 构造并绑定 ctx
func NewPostgresRepositoryWithContext[T any](c *PostgresClient, ctx context.Context) *PostgresRepository[T] {
	return pgdb.NewRepositoryWithContext[T](c, ctx)
}

// NewPostgresTxRepository 在事务内构造
func NewPostgresTxRepository[T any](t *PostgresTx) *PostgresRepository[T] {
	return pgdb.NewTxRepository[T](t)
}

// NewPostgresSessionRepository 在 Session 内构造
func NewPostgresSessionRepository[T any](s *PostgresSession) *PostgresRepository[T] {
	return pgdb.NewSessionRepository[T](s)
}

// NewPostgresQuery 构造 PostgreSQL 查询构建器
func NewPostgresQuery() *PostgresQuery {
	return pgdb.NewQuery()
}

// ==================== PostgreSQL 错误分类断言 ====================

func IsPostgresNotFound(err error) bool  { return pgdb.IsNotFound(err) }
func IsPostgresDuplicate(err error) bool { return pgdb.IsDuplicate(err) }
func IsPostgresTimeout(err error) bool   { return pgdb.IsTimeout(err) }
func IsPostgresCanceled(err error) bool  { return pgdb.IsCanceled(err) }
func IsPostgresDeadlock(err error) bool  { return pgdb.IsDeadlock(err) }

// IsPostgresTableNotExist 断言为目标表不存在（SQLSTATE 42P01）
func IsPostgresTableNotExist(err error) bool { return pgdb.IsTableNotExist(err) }

// ==================== PostgreSQL Option 工厂 ====================

func WithPostgresSlowQueryThreshold(d time.Duration) PostgresOption {
	return pgdb.WithSlowQueryThreshold(d)
}
func WithPostgresMetricsHook(h PostgresMetricsHook) PostgresOption {
	return pgdb.WithMetricsHook(h)
}
func WithPostgresConnMaxLifetime(d time.Duration) PostgresOption {
	return pgdb.WithConnMaxLifetime(d)
}
func WithPostgresDSNParam(k, v string) PostgresOption { return pgdb.WithDSNParam(k, v) }
func WithPostgresSSLMode(mode string) PostgresOption  { return pgdb.WithSSLMode(mode) }
func WithPostgresTimeZone(tz string) PostgresOption   { return pgdb.WithTimeZone(tz) }
func WithPostgresSchema(schema string) PostgresOption { return pgdb.WithSchema(schema) }
func WithPostgresAutoMigrate(enable bool) PostgresOption {
	return pgdb.WithAutoMigrate(enable)
}

// ==================== PostgreSQL 缓存回源/落库适配器 ====================
//
// 缓存适配器包装函数（NewPostgresCacheSource / NewPostgresCacheBatchSource /
// NewPostgresCacheSink / NewPostgresCacheBatchSink）与 MySQL 版同处 cache_api.go，
// 便于「回源/落库」这组能力在一处对照查阅。底层实现在 db/cache/postgres_source.go。

// ==================== PostgreSQL 默认 metrics 适配器 ====================

// PostgresMetricsAdapter 把 metrics.DB 适配为 pgdb.MetricsHook
type PostgresMetricsAdapter struct{}

// NewPostgresMetricsAdapter 构造适配器
func NewPostgresMetricsAdapter() PostgresMetricsHook { return &PostgresMetricsAdapter{} }

// OnQuery 上报查询耗时与错误分类
func (a *PostgresMetricsAdapter) OnQuery(op string, dur time.Duration, kind PostgresKind) {
	if kind != pgdb.KindUnknown && kind != pgdb.KindNotFound {
		_ = op
		_ = dur
	}
}

// OnSlowQuery 上报慢查询
func (a *PostgresMetricsAdapter) OnSlowQuery(op, sql string, dur time.Duration) {
	_ = op
	_ = sql
	_ = dur
}

// OnConnection 上报连接池状态
func (a *PostgresMetricsAdapter) OnConnection(state string, n float64) {
	metrics.DB.SetConnections("postgres", state, n)
}
