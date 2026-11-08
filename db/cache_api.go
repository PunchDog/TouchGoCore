// Package db 的统一缓存门面。
//
// 实现在 db/dbcache 子包；本文件只做类型别名与薄封装，调用方 import
// touchgocore/db 后即可使用 CacheInsert / CacheQuery / CacheSave / CacheDelete
// 四个数据接口（CacheBind* 仅用于表绑定）。
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
	// ErrCachePartialKey 表示 CacheDelete 的条件未覆盖全部行键列。
	ErrCachePartialKey = dbcache.ErrPartialKey
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

// CacheOption 一次数据操作的扩展参数（事务/行锁/原子累加/CAS）。
type CacheOption = dbcache.Option

// ForUpdate 单行查询加 SELECT ... FOR UPDATE（必须在 InTx 内）。
func ForUpdate() CacheOption { return func(o *dbcache.Options) { o.ForUpdate = true } }

// Increment 让 CacheSave 执行数据库侧原子累加。
func Increment(column string, delta int64) CacheOption {
	return func(o *dbcache.Options) {
		o.Increment = column
		o.Delta = delta
	}
}

// CompareAndSwap 让 CacheSave 执行条件更新（expect 全部命中才写 set）。
func CompareAndSwap(expect, set map[string]any) CacheOption {
	return func(o *dbcache.Options) {
		o.CASExpect = expect
		o.CASSet = set
	}
}

// InTx 开启框架内部事务；fn 内通过 ctx 调用四个数据接口。
func InTx(fn func(ctx context.Context) error) CacheOption {
	return func(o *dbcache.Options) { o.InTx = fn }
}

// AnyDest 跳过绑定行类型校验，按列名把结果扫描到调用方类型（单表 model 投影）。
func AnyDest() CacheOption {
	return func(o *dbcache.Options) { o.AllowAnyDest = true }
}

// Equal 读条件：column = value。
func Equal(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "=", Value: value})
	}
}

// EqualOn 读条件：table.column = value（JOIN 查询）。
func EqualOn(table, column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Table: table, Column: column, Op: "=", Value: value})
	}
}

// Like 读条件：column LIKE value（value 由调用方拼好并走占位符）。
func Like(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "LIKE", Value: value})
	}
}

// MatchStruct 原子写(CacheSave+Increment/CAS)的行定位条件：cache tag 结构体指针。
// 条件可以少于全部行键列（配合 Condition 做批量更新）。
func MatchStruct(cond any) CacheOption {
	return func(o *dbcache.Options) { o.Cond = cond }
}

// Condition 追加一个结构化条件：column op value（op 白名单与读条件一致）。
// 读路径按 AND 生效，写路径用作定位条件。
func Condition(column, op string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Conditions = append(o.Conditions, dbcache.Range{Column: column, Op: op, Value: value})
	}
}

// ConditionOn 表限定的结构化条件（JOIN 读里列属于 joined 表时用）。
func ConditionOn(table, column, op string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Conditions = append(o.Conditions, dbcache.Range{Table: table, Column: column, Op: op, Value: value})
	}
}

// Affected 把本次写影响的行数回填到 n（依赖 RowsAffected 的 CAS/扣减用）。
func Affected(n *int64) CacheOption {
	return func(o *dbcache.Options) { o.Affected = n }
}

// SequenceIncrement 原子取号：INSERT 新行取 0，冲突时 col=LAST_INSERT_ID(col+delta)，
// 结果写 out。必须在事务内使用（MySQL 会话语义）。
func SequenceIncrement(column string, delta int64, out *int64) CacheOption {
	return func(o *dbcache.Options) {
		o.Sequence = &dbcache.SequenceSpec{Column: column, Delta: delta, Out: out}
	}
}

// SecretSet 只写不缓存的秘密列（passwd_hash / from_private_key 等）。
func SecretSet(cols map[string]any) CacheOption {
	return func(o *dbcache.Options) { o.SecretSet = cols }
}

// NoCache 读路径强制直查数据库（秘密列投影等不允许进缓存的读）。
func NoCache() CacheOption {
	return func(o *dbcache.Options) { o.NoCache = true }
}

// In 读条件：column IN (values...)（values 为空时拒绝）。
func In(column string, values ...any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "IN", Value: values})
	}
}

// Cond 一个结构化条件，仅用于 AnyOf 的 OR 分组。
type Cond = dbcache.Range

// CondEqual 生成 cond：column = value。
func CondEqual(column string, value any) Cond {
	return dbcache.Range{Column: column, Op: "=", Value: value}
}

// CondGreaterThan 生成 cond：column > value。
func CondGreaterThan(column string, value any) Cond {
	return dbcache.Range{Column: column, Op: ">", Value: value}
}

// CondOn 生成带表限定的 cond：table.column op value（JOIN 的 OR 分组用）。
func CondOn(table, column, op string, value any) Cond {
	return dbcache.Range{Table: table, Column: column, Op: op, Value: value}
}

// CondNotEqual 生成 cond：column <> value。
func CondNotEqual(column string, value any) Cond {
	return dbcache.Range{Column: column, Op: "<>", Value: value}
}

// CondIn 生成 cond：column IN (values...)。
func CondIn(column string, values ...any) Cond {
	return dbcache.Range{Column: column, Op: "IN", Value: values}
}

// AnyOf 追加一个 OR 分组：组内任一 cond 命中即可；它与其它条件仍是 AND。
func AnyOf(conds ...Cond) CacheOption {
	return func(o *dbcache.Options) {
		grp := make([]dbcache.Range, 0, len(conds))
		grp = append(grp, conds...)
		o.AnyOfGroups = append(o.AnyOfGroups, grp)
	}
}

// Expr 聚合表达式（结构化，不传 SQL 文本）。
type Expr = dbcache.Expr

// Col 引用一个绑定表列。
func Col(name string) Expr { return dbcache.Col(name) }

// Lit 一个字面量（渲染成占位符）。
func Lit(v any) Expr { return dbcache.Lit(v) }

// Sub 减法表达式 a - b。
func Sub(a, b Expr) Expr { return dbcache.Sub(a, b) }

// Greatest GREATEST(args...)。
func Greatest(args ...Expr) Expr { return dbcache.Greatest(args...) }

// Coalesce COALESCE(args...)。
func Coalesce(args ...Expr) Expr { return dbcache.Coalesce(args...) }

// SumExpr SUM(arg)。
func SumExpr(arg Expr) Expr { return dbcache.SumExpr(arg) }

// CountExpr COUNT/COUNT(DISTINCT arg)。
func CountExpr(arg Expr, distinct bool) Expr { return dbcache.CountExpr(arg, distinct) }

// Star 聚合里的 *（仅 COUNT 使用）。
func Star() Expr { return dbcache.Star() }

// TCol 引用带表别名限定的列（JOIN 查询）。
func TCol(table, name string) Expr { return dbcache.TCol(table, name) }

// Alias 设置基表别名。
func Alias(name string) CacheOption {
	return func(o *dbcache.Options) { o.BaseAlias = name }
}

// JoinLeft 追加 LEFT JOIN。
func JoinLeft(table, alias, leftTable, leftColumn, rightColumn string) CacheOption {
	return func(o *dbcache.Options) {
		o.Joins = append(o.Joins, dbcache.Join{
			Type: "LEFT", Table: table, Alias: alias,
			LeftTable: leftTable, LeftColumn: leftColumn, RightColumn: rightColumn,
		})
	}
}

// JoinInner 追加 INNER JOIN。
func JoinInner(table, alias, leftTable, leftColumn, rightColumn string) CacheOption {
	return func(o *dbcache.Options) {
		o.Joins = append(o.Joins, dbcache.Join{
			Type: "INNER", Table: table, Alias: alias,
			LeftTable: leftTable, LeftColumn: leftColumn, RightColumn: rightColumn,
		})
	}
}

// OrderByOn 按指定表别名限定排序。
func OrderByOn(table, column string, desc bool) CacheOption {
	return func(o *dbcache.Options) {
		o.Orders = append(o.Orders, dbcache.Order{Table: table, Column: column, Desc: desc})
	}
}

// DateBucket 按「加 offsetSec 秒后的日期」分桶。
func DateBucket(column string, offsetSec int) Expr {
	return dbcache.DateBucket(column, offsetSec)
}

// AggregateExpr 聚合投影：expr AS total。
func AggregateExpr(e Expr) CacheOption {
	return func(o *dbcache.Options) { o.Agg = &dbcache.Aggregate{Expr: e, Alias: "total"} }
}

// Select 自定义投影项（趋势的多列聚合）。
func Select(e Expr, alias string) CacheOption {
	return func(o *dbcache.Options) {
		o.Selects = append(o.Selects, dbcache.SelectItem{Expr: e, Alias: alias})
	}
}

// GroupByAlias 按 SELECT 别名分组。
func GroupByAlias(aliases ...string) CacheOption {
	return func(o *dbcache.Options) { o.GroupAliases = append(o.GroupAliases, aliases...) }
}

// OrderByAlias 按 SELECT 别名排序。
func OrderByAlias(alias string, desc bool) CacheOption {
	return func(o *dbcache.Options) {
		o.Orders = append(o.Orders, dbcache.Order{Column: alias, Desc: desc, Alias: true})
	}
}

// NotEqual 读条件：column <> value。
func NotEqual(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "<>", Value: value})
	}
}

// IsNull 读条件：column IS NULL。
func IsNull(column string) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "IS NULL"})
	}
}

// NotNull 读条件：column IS NOT NULL。
func NotNull(column string) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "IS NOT NULL"})
	}
}

// GreaterThan 读条件：column > value。
func GreaterThan(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: ">", Value: value})
	}
}

// GreaterOrEqual 读条件：column >= value。
func GreaterOrEqual(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: ">=", Value: value})
	}
}

// LessThan 读条件：column < value。
func LessThan(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "<", Value: value})
	}
}

// LessOrEqual 读条件：column <= value。
func LessOrEqual(column string, value any) CacheOption {
	return func(o *dbcache.Options) {
		o.Ranges = append(o.Ranges, dbcache.Range{Column: column, Op: "<=", Value: value})
	}
}

// OrderBy 读排序。
func OrderBy(column string, desc bool) CacheOption {
	return func(o *dbcache.Options) {
		o.Orders = append(o.Orders, dbcache.Order{Column: column, Desc: desc})
	}
}

// GroupBy 读分组。
func GroupBy(columns ...string) CacheOption {
	return func(o *dbcache.Options) { o.Groups = append(o.Groups, columns...) }
}

// Limit 读分页：最多取 n 行。
func Limit(n int) CacheOption { return func(o *dbcache.Options) { o.Limit = n } }

// Offset 读分页：跳过前 n 行。
func Offset(n int) CacheOption { return func(o *dbcache.Options) { o.Offset = n } }

// CountDistinctNotNull 聚合：COUNT(DISTINCT NULLIF(column,”)) AS total。
func CountDistinctNotNull(column string) CacheOption {
	return func(o *dbcache.Options) {
		o.Agg = &dbcache.Aggregate{Fn: "COUNT", Column: column, Alias: "total", DistinctNotNull: true}
	}
}

// Sum 聚合：SUM(column) AS total。
func Sum(column string) CacheOption {
	return func(o *dbcache.Options) {
		o.Agg = &dbcache.Aggregate{Fn: "SUM", Column: column, Alias: "total"}
	}
}

// Count 聚合：COUNT(column) AS total；column 为空表示 COUNT(*)。
func Count(column string) CacheOption {
	return func(o *dbcache.Options) {
		o.Agg = &dbcache.Aggregate{Fn: "COUNT", Column: column, Alias: "total"}
	}
}

// CacheQuery 按条件查询：out 传 *T 查单条，传 *[]T 查列表。
func CacheQuery[T any](ctx context.Context, table string, cond any, out any, opts ...CacheOption) error {
	return dbcache.Query[T](ctx, table, cond, out, opts...)
}

// CacheSave 全值写入一行；mustSave 决定是否在返回前落库。
func CacheSave[T any](ctx context.Context, table string, key string, row *T, mustSave bool, opts ...CacheOption) error {
	if o := dbcache.ParseOptions(opts); o.InTx != nil {
		return dbcache.RunTx(ctx, o.InTx)
	}
	return dbcache.Save[T](ctx, table, key, row, mustSave, opts...)
}

// CacheInsert 插入一行，唯一键冲突返回 affected=0（不覆盖已有行）。
func CacheInsert[T any](ctx context.Context, table string, row *T, opts ...CacheOption) (int64, error) {
	return dbcache.Insert[T](ctx, table, row, opts...)
}

// CacheDelete 物理删除一行：先删数据库，再清 Redis 行 hash、脏账与重试账，
// 并留下删除墓碑阻止在途脏账回写。cond 必须覆盖表的全部 KeyColumns。
func CacheDelete(ctx context.Context, table string, cond any, opts ...CacheOption) error {
	return dbcache.Delete(ctx, table, cond, opts...)
}
