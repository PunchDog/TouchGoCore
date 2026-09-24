// 本文件是两级缓存（db/cache 子包）的对外门面，风格对齐 mysql_api.go：
// 类型别名 + 泛型包装函数 + 配置转换。构造选项（WithTTL/WithLoader 等）是
// 泛型函数，无法别名转发，直接从 touchgocore/db/cache 包引入使用即可。
package db

import (
	"context"
	"strings"
	"time"

	"touchgocore/config"
	"touchgocore/util"

	cachepkg "touchgocore/db/cache"

	"github.com/redis/go-redis/v9"
)

// ==================== 缓存类型别名 ====================

type (
	KV             = cachepkg.KV
	CacheConfig    = cachepkg.Config
	StatsSnapshot  = cachepkg.StatsSnapshot
	StalePolicy    = cachepkg.StalePolicy
	OverflowPolicy = cachepkg.OverflowPolicy
	CacheLayer     = cachepkg.Layer
	LayerOption    = cachepkg.LayerOption
	Codec          = cachepkg.Codec
	OpKind         = cachepkg.OpKind

	Cache[K comparable, V any]       = cachepkg.Cache[K, V]
	CacheOption[K comparable, V any] = cachepkg.Option[K, V]
	Loader[K comparable, V any]      = cachepkg.Loader[K, V]
	BatchLoader[K comparable, V any] = cachepkg.BatchLoader[K, V]
	Saver[K comparable, V any]       = cachepkg.Saver[K, V]
	BatchSaver[K comparable, V any]  = cachepkg.BatchSaver[K, V]
	Item[K comparable]               = cachepkg.Item[K]
)

const (
	StaleAsync      = cachepkg.StaleAsync
	StaleWait       = cachepkg.StaleWait
	OverflowBlock   = cachepkg.OverflowBlock
	OverflowDrop    = cachepkg.OverflowDrop
	OpUpsert        = cachepkg.OpUpsert
	OpDelete        = cachepkg.OpDelete
	DefaultCacheTTL = 5 * time.Minute
)

var (
	ErrNoEntry           = cachepkg.ErrNoEntry           // Redis 无该键
	ErrCacheNotFound     = cachepkg.ErrNotFound          // 源确认不存在
	ErrCacheMiss         = cachepkg.ErrMiss              // 只查缓存未命中
	ErrSourceUnavailable = cachepkg.ErrSourceUnavailable // 回源失败退避中
)

// DefaultCacheConfig 返回框架默认缓存配置
func DefaultCacheConfig() CacheConfig { return cachepkg.DefaultConfig() }

// WithLayerConfig 设置 Layer 注册默认配置（转发 db/cache.WithLayerConfig）
func WithLayerConfig(c CacheConfig) LayerOption { return cachepkg.WithLayerConfig(c) }

// NewJSONCodec 标准库 JSON 编解码
func NewJSONCodec() Codec { return cachepkg.NewJSONCodec() }

// ==================== 构造 ====================

// NewRedisKV 用 App.Redis 的连接构造一级缓存句柄；连接缺失返回 nil。
func NewRedisKV(r *Redis) KV {
	if r == nil {
		return nil
	}
	cmd := r.Get()
	if cmd == nil {
		return nil
	}
	return cachepkg.NewRedisStore(cmd, 16)
}

// NewRedisKVStore 从原始 redis.Cmdable 构造 KV（集群批量读自动退化并发单键）
func NewRedisKVStore(cmd redis.Cmdable, conc int) KV {
	return cachepkg.NewRedisStore(cmd, conc)
}

// NewCacheLayer 构造缓存层（多类型 Cache 的注册表 + 生命周期容器）
func NewCacheLayer(kv KV, opts ...LayerOption) *CacheLayer {
	return cachepkg.NewLayer(kv, opts...)
}

// NewCache 独立构造一个类型的缓存（不经 Layer 时自行驱动 Run/Close）
func NewCache[K comparable, V any](name string, r *Redis, opts ...CacheOption[K, V]) (*Cache[K, V], error) {
	return cachepkg.New[K, V](name, NewRedisKV(r), opts...)
}

// OpenCache 在 Layer 上注册一个类型的 Cache（名称唯一，继承 Layer 默认配置）
func OpenCache[K comparable, V any](l *CacheLayer, name string, opts ...CacheOption[K, V]) (*Cache[K, V], error) {
	return cachepkg.Register[K, V](l, name, opts...)
}

// ==================== 后端闭包工厂 ====================

// NewLoaderFunc 闭包版回源（任意数据源通用）
func NewLoaderFunc[K comparable, V any](f func(ctx context.Context, key K) (*V, error)) Loader[K, V] {
	return cachepkg.LoaderFunc[K, V](f)
}

// NewSaverFunc 闭包版落库
func NewSaverFunc[K comparable, V any](
	save func(ctx context.Context, key K, val *V) error,
	del func(ctx context.Context, key K) error,
) Saver[K, V] {
	return cachepkg.SaverFunc[K, V]{SaveFn: save, DeleteFn: del}
}

// NewMysqlCacheSource MySQL 主键回源（gorm 未命中 → ErrCacheNotFound）
func NewMysqlCacheSource[T any, K comparable](r *Repository[T], keyToID func(K) any) Loader[K, T] {
	return cachepkg.MysqlSource[T, K](r, keyToID)
}

// NewMysqlCacheSink MySQL 单条落库（Save→Upsert，Delete→软删）
func NewMysqlCacheSink[T any, K comparable](r *Repository[T], keyToID func(K) any, idCols ...string) Saver[K, T] {
	return cachepkg.MysqlSink[T, K](r, keyToID, idCols...)
}

// NewMysqlCacheBatchSink MySQL 批量落库（UpsertBatch 一条 SQL 搞定 upsert 集）
func NewMysqlCacheBatchSink[T any, K comparable](r *Repository[T], keyToID func(K) any, idCols ...string) BatchSaver[K, T] {
	return cachepkg.MysqlBatchSink[T, K](r, keyToID, idCols...)
}

// ==================== 配置转换 ====================

// CacheConfigFrom 把 JSON 配置段（_ms 约定）转成 db/cache.Config；
// 零值字段回落框架默认。cc==nil 时返回默认配置（Enabled=false 语义由调用方判断）。
func CacheConfigFrom(cc *config.CacheConfig) CacheConfig {
	cfg := cachepkg.DefaultConfig()
	cfg.Group = util.GameGroup
	if cc == nil {
		return cfg
	}
	if cc.KeyPrefix != "" {
		cfg.KeyPrefix = cc.KeyPrefix
	}
	ms := func(v int, target *time.Duration) {
		if v > 0 {
			*target = time.Duration(v) * time.Millisecond
		}
	}
	ms(cc.TTLMS, &cfg.TTL)
	ms(cc.NegativeTTLMS, &cfg.NegativeTTL)
	ms(cc.ReadTimeoutMS, &cfg.ReadTimeout)
	ms(cc.WriteTimeoutMS, &cfg.WriteTimeout)
	ms(cc.FailBackoffMS, &cfg.FailBackoff)
	ms(cc.FlushIntervalMS, &cfg.FlushInterval)
	ms(cc.MaxDirtyAgeMS, &cfg.MaxDirtyAge)
	if cc.LogicalPct != 0 {
		cfg.LogicalPct = cc.LogicalPct
	}
	if cc.JitterPct != 0 {
		cfg.JitterPct = cc.JitterPct
	}
	if cc.NegativeTTLMS < 0 {
		cfg.NegativeTTL = 0 // 负值显式关闭空值缓存
	}
	if cc.BatchSize > 0 {
		cfg.BatchSize = cc.BatchSize
	}
	if cc.MaxDirtyKeys > 0 {
		cfg.MaxDirtyKeys = cc.MaxDirtyKeys
	}
	if cc.Shards > 0 {
		cfg.Shards = cc.Shards
	}
	if cc.MaxRetry > 0 {
		cfg.MaxRetry = cc.MaxRetry
	}
	if cc.SaveConcurrency > 0 {
		cfg.SaveConcurrency = cc.SaveConcurrency
	}
	if cc.RequireRedis != nil {
		cfg.RequireRedis = *cc.RequireRedis
	}
	if cc.Journal != nil {
		cfg.Journal = *cc.Journal
	}
	switch strings.ToLower(strings.TrimSpace(cc.StalePolicy)) {
	case "wait":
		cfg.Stale = cachepkg.StaleWait
	case "async", "":
		cfg.Stale = cachepkg.StaleAsync
	}
	switch strings.ToLower(strings.TrimSpace(cc.Overflow)) {
	case "drop":
		cfg.Overflow = cachepkg.OverflowDrop
	case "block", "":
		cfg.Overflow = cachepkg.OverflowBlock
	}
	cfg.Enabled = cc.Enabled
	return cfg
}
