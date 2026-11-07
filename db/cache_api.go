// Package db 的统一缓存门面。
//
// 实现在 db/dbcache 子包；本文件只做类型别名与薄封装，调用方 import
// touchgocore/db 后即可使用 CacheQuery / CacheSave / CacheBind*。
// 子包不 import 根 db，因此这里的反向引用不会形成循环依赖。
package db

import (
	"context"

	"github.com/redis/go-redis/v9"

	"touchgocore/config"
	"touchgocore/db/dbcache"
)

// CacheEngine 是统一缓存层实例。
type CacheEngine = dbcache.Engine

// CacheConfig 是缓存层运行参数。
type CacheConfig = dbcache.Config

// CacheBindOption 是表绑定的可选参数。
type CacheBindOption[T any] = dbcache.BindOption[T]

// ==================== 缓存错误 ====================

var (
	// ErrCacheNotInitialized 表示 InitCache 尚未调用。
	ErrCacheNotInitialized = dbcache.ErrNotInitialized
	// ErrCacheTableUnbound 表示表未通过 CacheBind* 注册。
	ErrCacheTableUnbound = dbcache.ErrTableUnbound
	// ErrCacheTableBound 表示同名表重复绑定。
	ErrCacheTableBound = dbcache.ErrTableBound
	// ErrCacheTypeMismatch 表示调用方模型类型与绑定类型不一致。
	ErrCacheTypeMismatch = dbcache.ErrTypeMismatch
	// ErrCacheEmptyCondition 表示条件为空（拒绝全表查询）。
	ErrCacheEmptyCondition = dbcache.ErrEmptyCondition
	// ErrCacheNoRows 表示数据库中没有符合条件的行。
	ErrCacheNoRows = dbcache.ErrNoRows
	// ErrCacheBadOut 表示 out 参数不是 *T 或 *[]T。
	ErrCacheBadOut = dbcache.ErrBadOut
	// ErrCacheBadCond 表示条件不是结构体指针。
	ErrCacheBadCond = dbcache.ErrBadCond
	// ErrCacheKeyMissing 表示无法从行或参数得到主键。
	ErrCacheKeyMissing = dbcache.ErrKeyMissing
	// ErrCacheDecode 表示 hash 行无法解码回模型。
	ErrCacheDecode = dbcache.ErrDecode
	// ErrCacheFlushIncomplete 表示停机时仍有未落库的脏键。
	ErrCacheFlushIncomplete = dbcache.ErrFlushIncomplete
	// ErrCacheBadColumn 表示列名不合法或不存在。
	ErrCacheBadColumn = dbcache.ErrBadColumn
	// ErrCacheNilRow 表示 CacheSave 收到 nil 行。
	ErrCacheNilRow = dbcache.ErrNilRow
)

// ==================== 生命周期 ====================

// CacheConfigFrom 把配置段转换为缓存运行参数；nil 或缺失字段回落默认值。
func CacheConfigFrom(c *config.CacheConfig) CacheConfig {
	return dbcache.ConfigFrom(c)
}

// InitCache 创建并安装默认缓存引擎。rdb 为 nil 时返回错误。
func InitCache(cfg CacheConfig, rdb redis.Cmdable) error {
	return dbcache.Init(cfg, rdb)
}

// DefaultCache 返回默认缓存引擎；未初始化时为 nil。
func DefaultCache() *CacheEngine { return dbcache.Default() }

// StartCache 启动默认引擎的延迟落库定时器。
func StartCache(ctx context.Context) error { return dbcache.Start(ctx) }

// StopCache 停止默认引擎并执行 final flush。
func StopCache(ctx context.Context) error { return dbcache.Stop(ctx) }

// CloseCache 卸载默认引擎并尽力完成 final flush。
func CloseCache() { dbcache.Close() }

// FlushCache 立即把指定表的全部脏键写入数据库。
func FlushCache(ctx context.Context, table string) error { return dbcache.Flush(ctx, table) }

// ==================== 表绑定 ====================

// CacheBindMySQL 把表绑定到 MySQL 后端。
func CacheBindMySQL[T any](table string, c *Client, opt CacheBindOption[T]) error {
	return dbcache.BindMySQL[T](table, c, opt)
}

// CacheBindPostgres 把表绑定到 PostgreSQL 后端。
func CacheBindPostgres[T any](table string, c *PostgresClient, opt CacheBindOption[T]) error {
	return dbcache.BindPostgres[T](table, c, opt)
}

// CacheBindMongo 把表绑定到 MongoDB 集合。
func CacheBindMongo[T any](table string, dbo *DbOperate, opt CacheBindOption[T]) error {
	return dbcache.BindMongo[T](table, dbo, opt)
}

// ==================== 数据接口 ====================

// CacheQuery 按条件查询：out 传 *T 查单条，传 *[]T 查列表。
func CacheQuery[T any](ctx context.Context, table string, cond any, out any) error {
	return dbcache.Query[T](ctx, table, cond, out)
}

// CacheSave 全值写入一行；mustSave 决定是否在返回前落库。
func CacheSave[T any](ctx context.Context, table string, key string, row *T, mustSave bool) error {
	return dbcache.Save[T](ctx, table, key, row, mustSave)
}
