package dbcache

// ============================================================================
// 四个数据接口的 options 扩展
//
// 公开面仍然只有 CacheInsert / CacheQuery / CacheSave / CacheDelete；
// 事务、行锁、原子累加、CAS 都通过 options 表达，执行与缓存写穿在框架内部完成。
// 事务状态（txState）与写路径实现在 txstate.go；本文件只保留 options 形状与
// ctx 上的事务键。读形状扩展（范围/排序/聚合/JOIN）由 spec.go 的 Redis 引擎
// 求值（键集扫描 + Go 表达式），Redis 是唯一真源。
// ============================================================================

import (
	"context"
)

// Options 一次数据操作的扩展参数。
type Options struct {
	ForUpdate bool           // CacheQuery：单行取 Redis 行锁（必须处于 InTx 内）
	Increment string         // CacheSave：原子累加的列
	Delta     int64          // CacheSave：原子累加量（可为负）
	AtomicMax string         // CacheSave：原子取大的列（col = max(col, MaxValue)）
	MaxValue  int64          // CacheSave：原子取大候选值
	CASExpect map[string]any // CacheSave：条件更新的期望值
	CASSet    map[string]any // CacheSave：条件更新的写入列
	InTx      func(ctx context.Context) error

	// Cond 额外等值条件（cache tag 结构体）；Increment/CAS 用它定位行，
	// 不再要求调用方拼行键字符串。条件可以少于全部行键列（批量更新用）。
	Cond any
	// Conditions 额外的结构化条件（含 IN / 比较 / IS NULL），与 Cond 同时生效。
	Conditions []Range
	// Affected 非空时回填本次写影响的行数（依赖 RowsAffected 的 CAS 用）。
	Affected *int64
	// NoCache 历史上强制直查数据库；Redis 权威化后置位改走 spec 引擎（仍读 Redis
	// 真源，不回源滞后 sink）。默认点查即读权威行，强一致读无需本选项。
	NoCache bool
	// FromSource 让 spec/列表读以源库为真值：基表与 JOIN 表都从 MySQL 分页读取，
	// 结果按条件在 Go 侧求值，并尽力回填 Redis（save NX，不覆盖更新值）。
	// 与 WithSecrets 同时置位时不生效（秘密列只存在于 Redis :s:，源行拿不到）。
	FromSource bool
	// IncludeTombstoned 让 FromSource 不按墓碑跳过行：仅供「删除后由源库重建」
	// 这类显式刷新使用；普通列表读必须保持墓碑门控，防止已删行复活。
	IncludeTombstoned bool
	// SecretSet 只写不投影的秘密列（passwd_hash / from_private_key 等）：值进
	// Redis :s: 秘密 hash（不进模型行 hash），再经 sinkSecrets 按行键 UPDATE 落库
	// （异步 flush 与同步 mustSave 两条路径同口径）。
	SecretSet map[string]any
	// WithSecrets 读侧显式开启秘密列投影：把 :s: 秘密 hash 的白名单列并入结果
	// （仍读 Redis 真源，不回源 MySQL）。默认读永不暴露秘密列；仅鉴权场景
	// （登录口令摘要 / 支付私钥校验）使用。置位强制走 spec 引擎。
	WithSecrets bool
	// Sequence 原子取号：新行插入取 0，已有行把 col 加 delta 并回填 Out
	// （Redis 事务内即时求值，必须在事务内）。
	Sequence *SequenceSpec

	// 读形状扩展（CacheQuery）：范围、排序、分页、分组、聚合。
	Ranges []Range
	// AnyOfGroups 若干个 OR 分组：组内任一条命中即可，组与组之间仍是 AND。
	// 用于「done_count > 0 OR status IN (...)」这类无法用单一 Range 表达的判定。
	AnyOfGroups [][]Range
	Orders      []Order
	Groups      []string
	// GroupAliases 按 SELECT 别名分组（趋势日桶）。
	GroupAliases []string
	Limit        int
	Offset       int
	Agg          *Aggregate
	// Selects 自定义投影（含表达式与别名），与 Agg 二选一。
	Selects []SelectItem
	// BaseAlias 基表别名；非空时列引用也可带该前缀。
	BaseAlias string
	// Joins 结构化 JOIN 列表。
	Joins []Join
	// AllowAnyDest 跳过「out 类型必须等于绑定行类型」的校验（按列名扫描到 model）。
	AllowAnyDest bool
}

// SequenceSpec 原子取号规格：已有行把 Column 加 Delta，结果写 Out（Redis 事务内即时求值）。
type SequenceSpec struct {
	Column string
	Delta  int64
	Out    *int64
}

// Join 一个结构化 JOIN：Type=LEFT/INNER，LeftTable 为空表示基表。
type Join struct {
	Type       string
	Table      string
	Alias      string
	LeftTable  string
	LeftColumn string
	// RightColumn 是 joined 表上的列。
	RightColumn string
}

// SelectItem 一个投影项。
type SelectItem struct {
	Expr  Expr
	Alias string
}

// Range 一个比较条件。
type Range struct {
	Column string
	Op     string // =, <>, >, >=, <, <=, LIKE
	Value  any
	// Table 非空时按表别名限定列。
	Table string
}

// Order 一个排序列。
type Order struct {
	Column string
	Desc   bool
	// Alias 为真时 Column 是 SELECT 别名（不按绑定列校验）。
	Alias bool
	// Table 非空时按该表别名限定排序列。
	Table string
}

// Aggregate 一个聚合投影。
type Aggregate struct {
	Fn     string // COUNT / SUM / MIN / MAX / AVG
	Column string // 空 = COUNT(*)
	Alias  string
	// DistinctNotNull 为真且 Column 非空时生成 COUNT(DISTINCT NULLIF(col,''))。
	DistinctNotNull bool
	// Expr 非空时使用结构化表达式，忽略 Fn/Column。
	Expr Expr
}

// HasSpec 报告是否使用了读形状扩展。
func (o *Options) HasSpec() bool {
	return len(o.Ranges) > 0 || len(o.AnyOfGroups) > 0 || len(o.Orders) > 0 || len(o.Groups) > 0 ||
		o.NoCache || o.FromSource || o.IncludeTombstoned ||
		len(o.GroupAliases) > 0 || o.Limit > 0 || o.Offset > 0 ||
		o.Agg != nil || len(o.Selects) > 0 || len(o.Joins) > 0 || o.BaseAlias != "" ||
		o.AllowAnyDest || o.WithSecrets
}

// Option 修改 Options。
type Option func(*Options)

// ParseOptions 归一化 options。
func ParseOptions(opts []Option) *Options {
	o := &Options{}
	for _, fn := range opts {
		if fn != nil {
			fn(o)
		}
	}
	return o
}

type txKey struct{}

// txStateFrom 取 ctx 上的事务状态。
func txStateFrom(ctx context.Context) *txState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(txKey{}).(*txState)
	return st
}
