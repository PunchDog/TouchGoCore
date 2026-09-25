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

	// PostCommitAction 业务事务 COMMIT 成功后才允许执行的缓存动作（见下 WithPostCommit）。
	PostCommitAction = cachepkg.PostCommitAction
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

	ErrNoPostCommitSink = cachepkg.ErrNoPostCommitSink // ctx 未挂 post-commit 队列
	ErrPostCommitNested = cachepkg.ErrPostCommitNested // 重复挂载（嵌套事务失效归属不明）
	ErrPostCommitRan    = cachepkg.ErrPostCommitRan    // 队列已执行，再排动作永不被运行
	ErrFlushNotDurable  = cachepkg.ErrFlushNotDurable  // FlushNow 后仍有未落库脏数据
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

// NewMysqlCacheBatchSource MySQL 多主键一次回源（FindAll + IN）。
// keyFn 必须与该 Cache 的 WithKeyFunc 逐字一致，idOf 从实体取回业务键——
// 三者错位会让批量结果对不上键，退化成「命中即 miss」的静默低效。
func NewMysqlCacheBatchSource[T any, K comparable](
	r *Repository[T],
	keyToID func(K) any,
	idCol string,
	keyFn func(K) string,
	idOf func(*T) K,
) BatchLoader[K, T] {
	return cachepkg.MysqlBatchSource[T, K](r, keyToID, idCol, keyFn, idOf)
}

// NewMysqlCacheSink MySQL 单条落库（Save→Upsert，Delete→软删）
func NewMysqlCacheSink[T any, K comparable](r *Repository[T], keyToID func(K) any, idCols ...string) Saver[K, T] {
	return cachepkg.MysqlSink[T, K](r, keyToID, idCols...)
}

// NewMysqlCacheBatchSink MySQL 批量落库（UpsertBatch 一条 SQL 搞定 upsert 集）
func NewMysqlCacheBatchSink[T any, K comparable](r *Repository[T], keyToID func(K) any, idCols ...string) BatchSaver[K, T] {
	return cachepkg.MysqlBatchSink[T, K](r, keyToID, idCols...)
}

// ==================== 强一致写 / 事务边界（资金级路径用这组） ====================
//
// 写线 Write 是「同步写 Redis + 缓冲异步落库」，对资金表不可接受（丢失窗口 = flush 间隔，
// 且整行 Upsert 会盖掉并发的增量更新）。资金表的正确形态是：持久化真源仍在业务事务里，
// 缓存只做失效（DEL），且必须等 COMMIT 成功之后才动 Redis。
//
// 典型接线（数据访问层的事务包装）：
//
//	cctx, err := db.WithPostCommit(ctx)   // 挂失效动作队列
//	tx := ...Begin...
//	if err := fn(tx); err != nil { tx.Rollback(); return err }  // 回滚 → 队列一条不执行
//	if err := tx.Commit(); err != nil { return err }
//	if err := db.RunPostCommit(cctx); err != nil {
//	    vars.Error("缓存失效失败（库已提交，至多陈旧一个 TTL）: %v", err) // 只告警，不翻转业务结果
//	}
// 业务函数内部则写 db.InvalidateOnCommit(cctx, infra.UserCache(), uid)。

// WithPostCommit 给 ctx 挂 post-commit 动作队列（见 cachepkg.postcommit.go）。
func WithPostCommit(ctx context.Context) (context.Context, error) {
	return cachepkg.WithPostCommit(ctx)
}

// AppendPostCommit 追加动作；ctx 未挂队列返回 ErrNoPostCommitSink（不静默丢弃）。
func AppendPostCommit(ctx context.Context, acts ...PostCommitAction) error {
	return cachepkg.AppendPostCommit(ctx, acts...)
}

// RunPostCommit 执行并清空队列（COMMIT 成功后由事务包装器调用），返回聚合错误。
func RunPostCommit(ctx context.Context) error { return cachepkg.RunPostCommit(ctx) }

// Invalidate 生成「只失效 Redis」的动作；c 为 nil（本进程未启用缓存）时是空动作。
func Invalidate[K comparable, V any](ctx context.Context, c *Cache[K, V], key K) PostCommitAction {
	return cachepkg.Invalidate(ctx, c, key)
}

// InvalidateOnCommit 事务内标准失效：有队列则等 COMMIT，无队列则立即失效并告警。
func InvalidateOnCommit[K comparable, V any](ctx context.Context, c *Cache[K, V], key K) error {
	return cachepkg.InvalidateOnCommit(ctx, c, key)
}

// FlushNow 资金级「立即落库」：清掉指定键的异步残留，并对结果做诚实断言（详见
// cachepkg.Cache.FlushNow 的注释）。nil 的含义是「这些键库里已追上，或结构上不存在
// 异步路径」；未落库时返回包装了 ErrFlushNotDurable 的错误，绝不做静默 no-op。
func FlushNow[K comparable, V any](ctx context.Context, c *Cache[K, V], keys ...K) error {
	return c.FlushNow(ctx, keys...)
}

// Pending 报告指定键是否仍有未落库脏条目（观测/断言用）。
func Pending[K comparable, V any](c *Cache[K, V], keys ...K) bool {
	return c.Pending(keys...)
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
