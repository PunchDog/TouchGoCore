// Package db 的统一缓存门面。
//
// 实现在 db/dbcache 子包；本文件只做类型别名与薄封装，调用方 import
// touchgocore/db 后即可使用 CacheInsert / CacheQuery / CacheSave / CacheDelete
// 四个数据接口（CacheBind* 仅用于表绑定）。
// 子包不 import 根 db，因此这里的反向引用不会形成循环依赖。
//
// 权威模型：Redis 是唯一读写权威，MySQL 是可拆卸的异步 sink（滞后物化视图）。
// 拔掉 MySQL（BindRedis / BindMySQL(nil)）不影响 Redis 读写、事务与列表。
//
// 资金 / 账本列写路径硬约束（接入前置条件，不是现存漏洞）：
//
//   - 资金列、余额、账本与状态跃迁的唯一合法写路径是 Increment / CompareAndSwap：
//     二者在 Redis 侧 Lua 对 live 行原子校验条件后应用（HINCRBY 保 int64 精确），
//     校验失败零副作用（affected=0）；成功后登记脏账，由 sink 异步物化。读己写
//     由 Redis 权威保证，不存在「库已写、缓存未刷新」的双真源窗口。
//   - CacheSave 的 mustSave=false 只写 Redis + 登记脏账，由定时 flush 批量物化到
//     sink，滞后约一个 flush_interval_ms（默认 3s）；Redis 权威写即时生效，崩溃
//     丢数据风险由 Redis AOF 兜底（RequireAOF 缺失响亮告警），sink 只是滞后视图。
//     该模式只用于可容忍 sink 滞后的任务进度 / 统计类数据（当前唯一消费方是
//     task_engine 的进度写）。
//   - mustSave=true 在返回前同步物化到 sink（flushKey）；sink 缺席时按
//     Config.SinkRequired 告警放行（false）或报 ErrSinkRequired（true）——两种
//     情形下 Redis 权威写都已成功，错误/告警只针对缺席的物化视图。
//   - 强一致读默认即读 Redis 权威行（点查命中即真值）；NoCache 历史上「直查数据库」
//     的语义在 Redis 权威下不再成立（库是滞后 sink，读它反而陈旧）。
//   - 定性：pay / tron / ton / bsc / sol / ledger 等资金链路当前完全不读 dbcache
//     （全仓 db.Cache* 调用只出现在 task_engine/）。以上是「资金链路接入 dbcache
//     之前必须满足的接入前置约束」，不是当前已经存在的资金漏洞。
//
// 已知取舍（R-4）：dbcache 的默认引擎是进程级全局单例（db/dbcache/engine.go 的
// defaultEngine），一进程只能装一套缓存配置，多租户隔离与并行测试受限，相关测试须串行。
package db

import (
	"context"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"touchgocore/config"
	"touchgocore/db/dbcache"
)

// startDBSpan 从调用方 ctx 开启数据接口 span；ctx 为 nil 时回退 background。
//
// 不引入全局 tracer 单例（otel.Tracer 是包级函数，未配置 provider 时返回 noop），
// 也不配置采样率 / 后端：埋点只提供上下文透传，不改变任何业务语义。
// 返回的 ctx 带 span，继续传给 dbcache 以便下游埋点挂在同一条链路上。
func startDBSpan(ctx context.Context, spanName, table string) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	return otel.Tracer("touchgocore.db").Start(ctx, spanName,
		trace.WithAttributes(
			attribute.String("db.system", "touchgocore.dbcache"),
			attribute.String("db.table", table),
		))
}

// recordDBSpanError 把错误记到 span；不改变任何返回值语义。
func recordDBSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

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
	// ErrCacheDuplicateKey 表示唯一列组冲突（Insert 幂等冲突 / Save 守卫撞他行）。
	ErrCacheDuplicateKey = dbcache.ErrDuplicateKey
	// ErrCacheTxConflict 表示事务行锁等待超时（资金链读己写竞争）。
	ErrCacheTxConflict = dbcache.ErrTxConflict
	// ErrCacheSinkRequired 表示 SinkRequired=true 时 mustSave 写入没有可用 sink。
	ErrCacheSinkRequired = dbcache.ErrSinkRequired
	// ErrCacheScanBudgetExceeded 表示无索引扫描超过 MaxScanRows 预算（fail-closed）。
	ErrCacheScanBudgetExceeded = dbcache.ErrScanBudgetExceeded
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

// FlushAllCache 把所有存在待物化脏账的表立即写入数据库（无脏账的表跳过）。
func FlushAllCache(ctx context.Context) error { return dbcache.FlushAll(ctx) }

// FlushKeyCache 精确物化一个脏键（只落该行，不触碰同表其它脏账）。
func FlushKeyCache(ctx context.Context, table, key string) error {
	return dbcache.FlushKey(ctx, table, key)
}

// CacheKeyOf 按表绑定从行推导 Redis 行键。
func CacheKeyOf[T any](table string, row *T) (string, error) { return dbcache.KeyOf(table, row) }

// CacheKeyFromCond 按表绑定从完整行键条件推导 Redis 行键。
func CacheKeyFromCond(table string, cond any) (string, error) {
	return dbcache.KeyFromCond(table, cond)
}

// ==================== 表绑定 ====================

// CacheBindMySQL 把表绑定到 MySQL 后端。
func CacheBindMySQL[T any](table string, c *Client, opt CacheBindOption[T]) error {
	return dbcache.BindMySQL[T](table, c, opt)
}

// CacheBindRedis 把表绑定为纯 Redis 权威（无 sink）：拔掉 MySQL 不影响读写。
// 资金表用此入口（AllowSourceBackfill 强制 false，fail-closed）。
func CacheBindRedis[T any](table string, opt CacheBindOption[T]) error {
	return dbcache.BindRedis[T](table, opt)
}

// CacheBindPostgres 把表绑定到 PostgreSQL 后端。
func CacheBindPostgres[T any](table string, c *PostgresClient, opt CacheBindOption[T]) error {
	return dbcache.BindPostgres[T](table, c, opt)
}

// CacheBackfill 把源表分页全量播种进 Redis（迁移/冷启动工具）：已有行不覆盖、
// 墓碑存活不复活，重复调用幂等。返回值是本次实际新播种的行数。
func CacheBackfill[T any](ctx context.Context, table string, pageSize int) (int64, error) {
	return dbcache.BackfillTable[T](ctx, table, pageSize)
}

// CacheBindMongo 把表绑定到 MongoDB 集合。
func CacheBindMongo[T any](table string, dbo *DbOperate, opt CacheBindOption[T]) error {
	return dbcache.BindMongo[T](table, dbo, opt)
}

// ==================== 数据接口 ====================

// CacheOption 一次数据操作的扩展参数（事务/行锁/原子累加/CAS）。
type CacheOption = dbcache.Option

// CacheOptions 是解析后的 option 集合（门面导出，免得消费方借子包）。
type CacheOptions = dbcache.Options

// ParseCacheOptions 把若干 CacheOption 解析为 CacheOptions（纯读取，无副作用）。
func ParseCacheOptions(opts ...CacheOption) *CacheOptions {
	return dbcache.ParseOptions(opts)
}

// ForUpdate 单行查询取 Redis 行锁（必须在 InTx 内；TxRowLocks 是资金链读己写安全前提）。
func ForUpdate() CacheOption { return func(o *dbcache.Options) { o.ForUpdate = true } }

// Increment 让 CacheSave 执行 Redis 侧 Lua 原子累加（HINCRBY 保 int64 精确）。
func Increment(column string, delta int64) CacheOption {
	return func(o *dbcache.Options) {
		o.Increment = column
		o.Delta = delta
	}
}

// AtomicMax 让 CacheSave 执行 Redis 侧 Lua 原子取大（column = max(column, value)）。
// 行缺席或条件不命中 affected=0；命中即写（值不变也算命中），单 Lua op 无竞态。
func AtomicMax(column string, value int64) CacheOption {
	return func(o *dbcache.Options) {
		o.AtomicMax = column
		o.MaxValue = value
	}
}

// CompareAndSwap 让 CacheSave 执行 Redis 侧 Lua 条件更新（expect 全部命中才写 set）。
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

// SequenceIncrement 原子取号：新行插入取 0，已有行把 col 加 delta 并回填 out
// （Redis 事务内即时求值，非 MySQL 会话）。必须在事务内使用。
func SequenceIncrement(column string, delta int64, out *int64) CacheOption {
	return func(o *dbcache.Options) {
		o.Sequence = &dbcache.SequenceSpec{Column: column, Delta: delta, Out: out}
	}
}

// SecretSet 只写不默认投影的秘密列（passwd_hash / from_private_key 等）：
// 值进 Redis :s: 秘密 hash（永不进行 hash），经 sinkSecrets 落库；读回须显式
// WithSecrets()。
func SecretSet(cols map[string]any) CacheOption {
	return func(o *dbcache.Options) { o.SecretSet = cols }
}

// WithSecrets 读侧显式开启秘密列投影（passwd_hash / from_private_key 等白名单列）。
// 默认查询永不返回秘密列；仅鉴权场景（登录口令摘要 / 支付私钥校验）使用。
// 读的是 Redis 权威 :s: hash，不回源数据库。
func WithSecrets() CacheOption {
	return func(o *dbcache.Options) { o.WithSecrets = true }
}

// FromSource 让列表/spec 读以源库为真值：基表与 JOIN 表从 MySQL 读取并按条件
// 在 Go 侧求值，结果尽力回填 Redis（save NX）。单行点查仍读 Redis 权威行。
// 与 WithSecrets 同时置位时不生效（秘密列只存在于 Redis :s:）。
func FromSource() CacheOption {
	return func(o *dbcache.Options) { o.FromSource = true }
}

// IncludeTombstoned 让 FromSource 读不按墓碑跳过：仅供「删除后由源库重建」的
// 显式刷新使用。普通列表读必须保留墓碑门控，防止已删行复活。
func IncludeTombstoned() CacheOption {
	return func(o *dbcache.Options) { o.IncludeTombstoned = true }
}

// NoCache 历史上表示「绕过缓存行 hash、直查数据库」的强一致读。
//
// Redis 权威化后该语义不再成立：数据库只是滞后 sink，读它反而陈旧；默认点查
// 命中 Redis 权威行即真值（强一致）。本选项保留签名兼容，置位后读路径改走
// Redis spec 引擎（键集扫描 + 条件过滤），仍读同一 Redis 真源，不再回源数据库。
// 资金 / 状态类强一致读无需再依赖本选项——默认读即权威。
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
	ctx, span := startDBSpan(ctx, "touchgocore.db.CacheQuery", table)
	defer span.End()
	err := dbcache.Query[T](ctx, table, cond, out, opts...)
	recordDBSpanError(span, err)
	return err
}

// CacheSave 全值写入一行；mustSave 决定是否在返回前同步物化到 sink。
//
// 无论 mustSave 真假，Redis 权威写都即时生效（行 hash + 脏账）。mustSave=false
// 由定时 flush 异步物化到 sink，滞后约一个 flush_interval_ms（默认 3s），只适用
// 于可容忍 sink 滞后的任务进度 / 统计类数据（当前唯一消费方是 task_engine 的
// 进度写）。mustSave=true 在返回前经 flushKey 同步物化；sink 缺席时按
// Config.SinkRequired 告警放行（false）或报 ErrSinkRequired（true），Redis 写均已成功。
//
// 资金 / 账本列与状态跃迁的唯一合法写路径是 Increment / CompareAndSwap（Redis
// Lua 原子应用，见 dbcache/mutate.go），强一致读默认即读 Redis 权威行。
// 当前资金链路完全不接 dbcache，本条是接入前置约束，不是现存资金漏洞。
func CacheSave[T any](ctx context.Context, table string, key string, row *T, mustSave bool, opts ...CacheOption) error {
	ctx, span := startDBSpan(ctx, "touchgocore.db.CacheSave", table)
	defer span.End()
	if o := dbcache.ParseOptions(opts); o.InTx != nil {
		err := dbcache.RunTx(ctx, o.InTx)
		recordDBSpanError(span, err)
		return err
	}
	err := dbcache.Save[T](ctx, table, key, row, mustSave, opts...)
	recordDBSpanError(span, err)
	return err
}

// CacheInsert 插入一行，唯一键冲突返回 affected=0（不覆盖已有行）。
func CacheInsert[T any](ctx context.Context, table string, row *T, opts ...CacheOption) (int64, error) {
	ctx, span := startDBSpan(ctx, "touchgocore.db.CacheInsert", table)
	defer span.End()
	n, err := dbcache.Insert[T](ctx, table, row, opts...)
	recordDBSpanError(span, err)
	return n, err
}

// CacheDelete 物理删除一行：Redis 侧单条 Lua 原子 op 删行 hash / 秘密 / 索引 /
// 键集并立删除墓碑、销脏账/重试账（权威写即时生效），sink 删除经 dirtyDel 异步
// 物化。墓碑阻止在途脏账回写与迁移期回源复活。cond 必须覆盖表的全部 KeyColumns。
// Redis 失败直接上抛且零副作用（fail-closed），没有「数据库兜底」旁路。
func CacheDelete(ctx context.Context, table string, cond any, opts ...CacheOption) error {
	ctx, span := startDBSpan(ctx, "touchgocore.db.CacheDelete", table)
	defer span.End()
	err := dbcache.Delete(ctx, table, cond, opts...)
	recordDBSpanError(span, err)
	return err
}
