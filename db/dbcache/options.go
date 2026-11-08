package dbcache

// ============================================================================
// 四个数据接口的 options 扩展
//
// 公开面仍然只有 CacheInsert / CacheQuery / CacheSave / CacheDelete；
// 事务、行锁、原子累加、CAS 都通过 options 表达，执行与缓存写穿在框架内部完成。
//
// 适用范围：**仅 MySQL 绑定**。PostgreSQL / MongoDB 相关部分不参与本次迁移，
// 继续使用各自后端的直连仓储（resolveMySQL 会拒绝非 MySQL 表）。
// ============================================================================

import (
	"context"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	mysqldb "touchgocore/db/mysql"
) // Options 一次数据操作的扩展参数。
type Options struct {
	ForUpdate bool           // CacheQuery：单行 SELECT ... FOR UPDATE（必须处于 InTx 内）
	Increment string         // CacheSave：原子累加的列
	Delta     int64          // CacheSave：原子累加量（可为负）
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
	// NoCache 为真时读路径强制直查数据库（秘密列投影等不允许进缓存的读）。
	NoCache bool
	// SecretSet 只写不缓存的秘密列（passwd_hash / from_private_key 等）：
	// 主写入完成后在同一事务里补一条带 WHERE 行键的 UPDATE，绝不进 Redis。
	SecretSet map[string]any
	// Sequence 原子取号：INSERT 新行取 0，冲突时 col = LAST_INSERT_ID(col + delta)，
	// 结果回填 Out（MySQL 会话语义，必须在事务内）。
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

// SequenceSpec 原子取号规格：冲突时把 Column 加到 LAST_INSERT_ID 会话值，结果写 Out。
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
		o.NoCache ||
		len(o.GroupAliases) > 0 || o.Limit > 0 || o.Offset > 0 ||
		o.Agg != nil || len(o.Selects) > 0 || len(o.Joins) > 0 || o.BaseAlias != "" ||
		o.AllowAnyDest
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

// txState 是一段框架内部事务：所有操作走同一连接，提交后统一刷新缓存。
type txState struct {
	e         *Engine
	raw       *mysqldb.Tx
	refreshes []func()
}

// RunTx 在框架内部开启事务并执行 fn；fn 内必须通过 ctx 调四个数据接口。
func RunTx(ctx context.Context, fn func(context.Context) error) error {
	e := Default()
	if e == nil {
		return ErrNotInitialized
	}
	e.mu.RLock()
	c := e.mysqlClient
	e.mu.RUnlock()
	if c == nil {
		return ErrTableUnbound
	}
	raw, err := c.BeginTx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = raw.Rollback()
			panic(p)
		}
	}()
	st := &txState{e: e, raw: raw}
	tctx := context.WithValue(ctx, txKey{}, st)
	if err := fn(tctx); err != nil {
		_ = raw.Rollback()
		return err
	}
	if err := raw.Commit(); err != nil {
		return err
	}
	for _, fn := range st.refreshes {
		fn()
	}
	return nil
}

// txStateFrom 取 ctx 上的事务状态。
func txStateFrom(ctx context.Context) *txState {
	if ctx == nil {
		return nil
	}
	st, _ := ctx.Value(txKey{}).(*txState)
	return st
}

// saveInTx 事务内执行一次写：普通整行 / 原子累加 / CAS，并登记提交后刷新。
//
// 原子写（Increment / CAS）的行定位有三条来源，按优先级：
//  1. Options.Cond（cache tag 结构体，可少于全部行键列）；2. 行键字符串 key；
//  3. 两者都没有 ⇒ 拒绝（防止全表更新）。
// Options.Conditions 追加 IN / 比较等结构化条件（如批量按 id 集合更新、带下限扣减）。
func (s *txState) saveInTx(ctx context.Context, b *binding, key string, row any, o *Options) error {
	gx := s.raw.DB.WithContext(ctx).Table(b.table)
	vals, err := s.condVals(b, key, o)
	if err != nil {
		return err
	}
	gx = applyCond(gx, vals)
	gx, err = applyRanges(gx, b, o.Conditions)
	if err != nil {
		return err
	}
	gx, err = applyAnyOf(gx, b, o.AnyOfGroups)
	if err != nil {
		return err
	}
	var affected int64
	switch {
	case o.Sequence != nil:
		if row == nil {
			return ErrNilRow
		}
		col := o.Sequence.Column
		if err := validateColumn(b, col); err != nil {
			return err
		}
		cols := make([]clause.Column, len(b.meta.KeyCols))
		for i, kc := range b.meta.KeyCols {
			cols[i] = clause.Column{Name: kc}
		}
		res := s.raw.DB.WithContext(ctx).Table(b.table).
			Clauses(clause.OnConflict{
				Columns: cols,
				DoUpdates: clause.Assignments(map[string]any{
					col: gorm.Expr("LAST_INSERT_ID(`"+col+"` + ?)", o.Sequence.Delta),
				}),
			}).Create(row)
		if res.Error != nil {
			return res.Error
		}
		// MySQL ON DUPLICATE KEY：插入返回 1，更新返回 2。新行取 0，
		// 只有走过 LAST_INSERT_ID(...) 的更新分支才去读会话值。
		var id int64
		if res.RowsAffected > 1 {
			if err := s.raw.DB.WithContext(ctx).Raw("SELECT LAST_INSERT_ID()").Scan(&id).Error; err != nil {
				return err
			}
		}
		if o.Sequence.Out != nil {
			*o.Sequence.Out = id
		}
		affected = 1
	case o.Increment != "":
		if err := validateColumn(b, o.Increment); err != nil {
			return err
		}
		res := gx.UpdateColumn(o.Increment, gorm.Expr("`"+o.Increment+"` + ?", o.Delta))
		if res.Error != nil {
			return res.Error
		}
		affected = res.RowsAffected
	case o.CASSet != nil:
		if len(o.CASSet) == 0 {
			return ErrBadColumn
		}
		for col, v := range o.CASExpect {
			if err := validateColumn(b, col); err != nil {
				return err
			}
			gx = applyExpect(gx, col, v)
		}
		for col := range o.CASSet {
			if err := validateColumn(b, col); err != nil {
				return err
			}
		}
		res := gx.UpdateColumns(o.CASSet)
		if res.Error != nil {
			return res.Error
		}
		affected = res.RowsAffected
	default:
		if row == nil {
			return ErrNilRow
		}
		if res := gx.Clauses(clause.OnConflict{UpdateAll: true}).Create(row); res.Error != nil {
			return res.Error
		}
		affected = 1
		if len(o.SecretSet) > 0 {
			if err := applySecretSet(s.raw.DB.WithContext(ctx).Table(b.table), b, row, o); err != nil {
				return err
			}
		}
	}
	if o.Affected != nil {
		*o.Affected = affected
	}
	if affected == 0 {
		return nil
	}
	for _, refreshVals := range refreshValsFor(b, vals, o.Conditions) {
		rv := refreshVals
		s.refreshes = append(s.refreshes, func() {
			s.e.refreshFromDB(context.Background(), b, rv)
		})
	}
	return nil
}

// applySecretSet 把只写不缓存的秘密列补写进同一事务：按行的行键列定位。
//
// 这些列不在绑定行结构体里（缓存与整行写都不会碰它们），所以单独一条 UPDATE；
// 列名来自调用方显式白名单，不参与缓存编解码。
func applySecretSet(gx *gorm.DB, b *binding, row any, o *Options) error {
	vals := keyValsFromRow(b, row)
	if len(vals) == 0 {
		return ErrKeyMissing
	}
	return applyCond(gx, vals).UpdateColumns(o.SecretSet).Error
}

// condVals 归一化原子写的行定位条件：Cond 结构体优先，其次行键字符串。
func (s *txState) condVals(b *binding, key string, o *Options) ([]condVal, error) {
	if o.Cond != nil {
		return parseCond(b.meta, b.kind, o.Cond)
	}
	if key == "" {
		return nil, nil
	}
	return keyValsFromString(b, key)
}

// refreshValsFor 计算提交后要重读的定位条件。
//
// 定位条件完整覆盖行键时重读一行即可；否则看 Conditions 里有没有单个行键列上的
// 等值/IN（批量更新），有就逐个重读。都不满足时返回空（调用方自行靠 TTL 收敛）。
func refreshValsFor(b *binding, vals []condVal, ranges []Range) [][]condVal {
	if condIsExactKey(b.meta, vals) {
		return [][]condVal{vals}
	}
	keySet := make(map[string]bool, len(b.meta.KeyCols))
	for _, c := range b.meta.KeyCols {
		keySet[c] = true
	}
	for _, r := range ranges {
		if !keySet[r.Column] {
			continue
		}
		switch r.Op {
		case "=":
			return [][]condVal{{{Column: r.Column, Values: []any{r.Value}}}}
		case "IN":
			list, ok := r.Value.([]any)
			if !ok {
				return nil
			}
			out := make([][]condVal, 0, len(list))
			for _, v := range list {
				out = append(out, []condVal{{Column: r.Column, Values: []any{v}}})
			}
			return out
		}
	}
	return nil
}

// applyAnyOf 把 OR 分组挂到写语句上：组内任一命中，组与组之间 AND。
func applyAnyOf(gx *gorm.DB, b *binding, groups [][]Range) (*gorm.DB, error) {
	for _, grp := range groups {
		if len(grp) == 0 {
			return nil, ErrBadColumn
		}
		parts := make([]string, 0, len(grp))
		args := make([]any, 0, len(grp))
		for _, r := range grp {
			if err := validateColumn(b, r.Column); err != nil {
				return nil, err
			}
			col := "`" + r.Column + "`"
			switch r.Op {
			case "=", "<>", ">", ">=", "<", "<=", "LIKE":
				parts = append(parts, col+" "+r.Op+" ?")
				args = append(args, r.Value)
			case "IN":
				list, ok := r.Value.([]any)
				if !ok || len(list) == 0 {
					return nil, ErrBadColumn
				}
				parts = append(parts, col+" IN ?")
				args = append(args, list)
			case "IS NULL", "IS NOT NULL":
				parts = append(parts, col+" "+r.Op)
			default:
				return nil, ErrBadColumn
			}
		}
		gx = gx.Where("("+strings.Join(parts, " OR ")+")", args...)
	}
	return gx, nil
}

// applyRanges 把结构化条件挂到写语句上（列名按绑定校验，操作符白名单）。
func applyRanges(gx *gorm.DB, b *binding, ranges []Range) (*gorm.DB, error) {
	for _, r := range ranges {
		if err := validateColumn(b, r.Column); err != nil {
			return nil, err
		}
		col := "`" + r.Column + "`"
		switch r.Op {
		case "=", "<>", ">", ">=", "<", "<=", "LIKE":
			gx = gx.Where(col+" "+r.Op+" ?", r.Value)
		case "IN":
			list, ok := r.Value.([]any)
			if !ok || len(list) == 0 {
				return nil, ErrBadColumn
			}
			gx = gx.Where(col+" IN ?", list)
		case "IS NULL", "IS NOT NULL":
			gx = gx.Where(col + " " + r.Op)
		default:
			return nil, ErrBadColumn
		}
	}
	return gx, nil
}

// insertInTx 事务内插入（冲突跳过），登记提交后刷新。
func (s *txState) insertInTx(ctx context.Context, b *binding, row any, o *Options) (int64, error) {
	res := s.raw.DB.WithContext(ctx).Table(b.table).
		Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 && o != nil && len(o.SecretSet) > 0 {
		if err := applySecretSet(s.raw.DB.WithContext(ctx).Table(b.table), b, row, o); err != nil {
			return 0, err
		}
	}
	if res.RowsAffected > 0 {
		if vals := keyValsFromRow(b, row); len(vals) > 0 {
			s.refreshes = append(s.refreshes, func() {
				s.e.refreshFromDB(context.Background(), b, vals)
			})
		}
	}
	return res.RowsAffected, nil
}

// deleteWhereInTx 事务内按结构化条件批量删除（目前只支持行键列上的 IN/等值），
// 提交后逐行清缓存。
func (s *txState) deleteWhereInTx(ctx context.Context, b *binding, ranges []Range) (int64, error) {
	gx, err := applyRanges(s.raw.DB.WithContext(ctx).Table(b.table), b, ranges)
	if err != nil {
		return 0, err
	}
	res := gx.Delete(nil)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		return 0, nil
	}
	for _, vals := range refreshValsFor(b, nil, ranges) {
		rv := vals
		s.refreshes = append(s.refreshes, func() { s.e.dropCached(b, rv) })
	}
	return res.RowsAffected, nil
}

// deleteInTx 事务内删除一行，登记提交后清缓存。
func (s *txState) deleteInTx(ctx context.Context, b *binding, vals []condVal) error {
	res := applyCond(s.raw.DB.WithContext(ctx).Table(b.table), vals).Delete(nil)
	if res.Error != nil {
		return res.Error
	}
	s.refreshes = append(s.refreshes, func() {
		s.e.dropCached(b, vals)
	})
	return nil
}

// keyValsFromString 把行键字符串还原成条件（单列行键）。
func keyValsFromString(b *binding, key string) ([]condVal, error) {
	if len(b.meta.KeyCols) != 1 {
		return nil, ErrPartialKey
	}
	return []condVal{{Column: b.meta.KeyCols[0], Values: []any{key}}}, nil
}
