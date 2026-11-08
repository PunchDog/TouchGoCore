package dbcache

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	mysqldb "touchgocore/db/mysql"
	"touchgocore/vars"
)

// typeOf 返回类型参数 T 的非指针类型。
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// Query 按条件查询：out 传 *T 查单条，传 *[]T 查列表。
//
// 单条且条件恰好覆盖全部行键列时优先读 Redis；未命中回源数据库并回填。
// 列表与含非键列的查询直接读数据库，命中行回填 Redis 但不改变返回结果。
func Query[T any](ctx context.Context, table string, cond any, out any, opt ...Option) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	o := ParseOptions(opt)
	if o.HasSpec() {
		return querySpec[T](e, ctx, table, cond, out, o)
	}
	if st := txStateFrom(ctx); st != nil {
		b, err := e.lookupTyped(table, typeOf[T]())
		if err != nil {
			return err
		}
		vals, err := parseCond(b.meta, b.kind, cond)
		if err != nil {
			return err
		}
		if o.ForUpdate {
			single, ok := out.(*T)
			if !ok {
				return ErrBadOut
			}
			gx := applyCond(st.raw.DB.WithContext(ctx).Table(b.table), vals).
				Clauses(clause.Locking{Strength: "UPDATE"})
			err := gx.Take(single).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNoRows
			}
			return err
		}
		repo := mysqldb.NewTxRepository[T](st.raw)
		q := buildMySQLQuery(vals, b.orderBy)
		switch oo := out.(type) {
		case *T:
			row, ferr := repo.FindOne(ctx, q)
			if ferr != nil {
				if mysqldb.IsNotFound(ferr) {
					return ErrNoRows
				}
				return ferr
			}
			*oo = *row
		case *[]T:
			rows, ferr := repo.FindAll(ctx, q)
			if ferr != nil {
				return ferr
			}
			res := make([]T, 0, len(rows))
			for _, r := range rows {
				res = append(res, *r)
			}
			*oo = res
		default:
			return ErrBadOut
		}
		return nil
	}
	return e.query[T](ctx, table, cond, out)
}

func (e *Engine) query[T any](ctx context.Context, table string, cond any, out any) error {
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	if e.cfg.QueryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.QueryTimeout)
		defer cancel()
	}
	switch o := out.(type) {
	case *T:
		return e.queryOne[T](ctx, b, vals, o)
	case *[]T:
		return e.queryList[T](ctx, b, vals, o)
	default:
		return ErrBadOut
	}
}

// queryOne 执行单条查询。
func (e *Engine) queryOne[T any](ctx context.Context, b *binding, vals []condVal, out *T) error {
	// 条件完整覆盖行键：走点查，Redis 命中直接返回。
	if condIsExactKey(b.meta, vals) {
		key, err := keyFromCond(b.meta, vals)
		if err == nil {
			rowKey := b.keys.rowPrefix + key
			h, herr := e.store.HGetAll(ctx, rowKey)
			if herr != nil {
				vars.Warning("dbcache %s: redis get %s: %v", b.table, rowKey, herr)
			} else if len(h) > 0 {
				if row, derr := b.decode(h); derr == nil {
					*out = *row.(*T)
					return nil
				} else {
					vars.Warning("dbcache %s: decode cache row %s: %v", b.table, rowKey, derr)
				}
			}

			row, err := b.srcFindOne(ctx, vals)
			if err != nil {
				return err
			}
			fields, ferr := b.encodeFields(row)
			if ferr == nil {
				if h, herr := e.store.BackfillAndGet(ctx, rowKey, fields, b.ttl); herr == nil && len(h) > 0 {
					if cached, derr := b.decode(h); derr == nil {
						*out = *cached.(*T)
						return nil
					}
				}
			}
			*out = *row.(*T)
			return nil
		}
	}

	// 非键条件：直查数据库，命中后尽力回填。
	row, err := b.srcFindOne(ctx, vals)
	if err != nil {
		return err
	}
	if key, kerr := b.keyFromRow(row); kerr == nil {
		if fields, ferr := b.encodeFields(row); ferr == nil {
			if _, herr := e.store.BackfillAndGet(ctx, b.keys.rowPrefix+key, fields, b.ttl); herr != nil {
				vars.Warning("dbcache %s: backfill %s: %v", b.table, key, herr)
			}
		}
	}
	*out = *row.(*T)
	return nil
}

// queryList 执行列表查询：始终读数据库，命中行逐条回填。
func (e *Engine) queryList[T any](ctx context.Context, b *binding, vals []condVal, out *[]T) error {
	rows, err := b.srcFindList(ctx, vals)
	if err != nil {
		return err
	}
	result := make([]T, 0, len(rows))
	for _, rowAny := range rows {
		if key, kerr := b.keyFromRow(rowAny); kerr == nil {
			if fields, ferr := b.encodeFields(rowAny); ferr == nil {
				if _, herr := e.store.BackfillAndGet(ctx, b.keys.rowPrefix+key, fields, b.ttl); herr != nil {
					vars.Warning("dbcache %s: backfill %s: %v", b.table, key, herr)
				}
			}
		}
		result = append(result, *rowAny.(*T))
	}
	*out = result
	return nil
}

// querySpec 执行读形状扩展（范围/排序/分页/分组/聚合），始终走数据库后端。
func querySpec[T any](e *Engine, ctx context.Context, table string, cond any, out any, o *Options) error {
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	if b.kind != backendMySQL {
		return ErrTableUnbound
	}
	if o.Agg == nil && len(o.Selects) == 0 && !o.AllowAnyDest && b.typ != typeOf[T]() {
		return ErrTypeMismatch
	}
	resolver := map[string]*binding{}
	baseAlias := o.BaseAlias
	if baseAlias != "" {
		if !columnNameRe.MatchString(baseAlias) {
			return ErrBadColumn
		}
		resolver[baseAlias] = b
	} else {
		resolver[b.table] = b
	}
	type joinRef struct {
		join  Join
		alias string
		bind  *binding
	}
	joins := make([]joinRef, 0, len(o.Joins))
	for _, j := range o.Joins {
		typ := strings.ToUpper(j.Type)
		if typ == "" {
			typ = "LEFT"
		}
		if typ != "LEFT" && typ != "INNER" {
			return ErrBadColumn
		}
		jb, err := e.lookupTable(j.Table)
		if err != nil {
			return err
		}
		alias := j.Alias
		if alias == "" {
			alias = j.Table
		}
		if !columnNameRe.MatchString(alias) {
			return ErrBadColumn
		}
		leftAlias := j.LeftTable
		if leftAlias == "" {
			leftAlias = baseAlias
		}
		if leftAlias == "" {
			leftAlias = b.table
		}
		lb, ok := resolver[leftAlias]
		if !ok || lb == nil {
			return ErrBadColumn
		}
		if validateColumn(lb, j.LeftColumn) != nil {
			return ErrBadColumn
		}
		if validateColumn(jb, j.RightColumn) != nil {
			return ErrBadColumn
		}
		resolver[alias] = jb
		joins = append(joins, joinRef{join: Join{Type: typ}, alias: alias, bind: jb})
		joins[len(joins)-1].join.LeftColumn = j.LeftColumn
		joins[len(joins)-1].join.RightColumn = j.RightColumn
		joins[len(joins)-1].join.Table = j.Table
		joins[len(joins)-1].join.LeftTable = leftAlias
	}
	var vals []condVal
	if cond != nil {
		vals, err = parseCond(b.meta, b.kind, cond)
		if err != nil {
			return err
		}
	}
	var gx *gorm.DB
	if st := txStateFrom(ctx); st != nil {
		gx = st.raw.DB.WithContext(ctx)
	} else {
		e.mu.RLock()
		c := e.mysqlClient
		e.mu.RUnlock()
		if c == nil {
			return ErrTableUnbound
		}
		gx = c.Engine().WithContext(ctx)
	}
	baseTable := "`" + b.table + "`"
	if baseAlias != "" {
		baseTable += " " + baseAlias
	}
	gx = gx.Table(baseTable)
	if o.ForUpdate {
		gx = gx.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if len(vals) > 0 && baseAlias != "" {
		for _, v := range vals {
			if err := validateColumn(b, v.Column); err != nil {
				return err
			}
			col := "`" + baseAlias + "`.`" + v.Column + "`"
			if len(v.Values) == 1 {
				gx = gx.Where(col+" = ?", v.Values[0])
			} else {
				gx = gx.Where(col+" IN ?", v.Values)
			}
		}
	} else {
		gx = applyCond(gx, vals)
	}
	for _, j := range joins {
		gx = gx.Joins(j.join.Type + " JOIN `" + j.join.Table + "` " + j.alias +
			" ON `" + j.alias + "`.`" + j.join.RightColumn + "` = `" +
			j.join.LeftTable + "`.`" + j.join.LeftColumn + "`")
	}
	for _, r := range o.Ranges {
		part, args, err := renderRange(b, resolver, baseAlias, r)
		if err != nil {
			return err
		}
		gx = gx.Where(part, args...)
	}
	for _, r := range o.Conditions {
		part, args, err := renderRange(b, resolver, baseAlias, r)
		if err != nil {
			return err
		}
		gx = gx.Where(part, args...)
	}
	for _, grp := range o.AnyOfGroups {
		if len(grp) == 0 {
			return ErrBadColumn
		}
		parts := make([]string, 0, len(grp))
		var args []any
		for _, r := range grp {
			clause, a, err := renderRange(b, resolver, baseAlias, r)
			if err != nil {
				return err
			}
			parts = append(parts, clause)
			args = append(args, a...)
		}
		gx = gx.Where("("+strings.Join(parts, " OR ")+")", args...)
	}
	if len(o.Groups) > 0 || len(o.GroupAliases) > 0 {
		cols := make([]string, 0, len(o.Groups)+len(o.GroupAliases))
		for _, c := range o.Groups {
			if err := validateColumn(b, c); err != nil {
				return err
			}
			cols = append(cols, "`"+c+"`")
		}
		for _, a := range o.GroupAliases {
			if !columnNameRe.MatchString(a) {
				return ErrBadColumn
			}
			cols = append(cols, "`"+a+"`")
		}
		gx = gx.Group(strings.Join(cols, ", "))
	}
	if len(o.Selects) > 0 {
		parts := make([]string, 0, len(o.Selects))
		var selArgs []any
		for _, it := range o.Selects {
			s, a, err := RenderExpr(b, resolver, it.Expr)
			if err != nil {
				return err
			}
			if it.Alias != "" {
				if !columnNameRe.MatchString(it.Alias) {
					return ErrBadColumn
				}
				s += " AS `" + it.Alias + "`"
			}
			parts = append(parts, s)
			selArgs = append(selArgs, a...)
		}
		gx = gx.Select(strings.Join(parts, ", "), selArgs...)
	} else if o.Agg != nil {
		if o.Agg.Expr != nil {
			s, a, err := RenderExpr(b, resolver, o.Agg.Expr)
			if err != nil {
				return err
			}
			if o.Agg.Alias != "" {
				if !columnNameRe.MatchString(o.Agg.Alias) {
					return ErrBadColumn
				}
				s += " AS `" + o.Agg.Alias + "`"
			}
			gx = gx.Select(s, a...)
		} else {
			fn := strings.ToUpper(o.Agg.Fn)
			switch fn {
			case "COUNT", "SUM", "MIN", "MAX", "AVG":
			default:
				return ErrBadColumn
			}
			expr := fn + "(*)"
			if o.Agg.Column != "" {
				if err := validateColumn(b, o.Agg.Column); err != nil {
					return err
				}
				if o.Agg.DistinctNotNull {
					expr = "COUNT(DISTINCT NULLIF(`" + o.Agg.Column + "`, ''))"
				} else {
					expr = fn + "(`" + o.Agg.Column + "`)"
				}
			}
			if o.Agg.Alias != "" {
				if !columnNameRe.MatchString(o.Agg.Alias) {
					return ErrBadColumn
				}
				expr += " AS `" + o.Agg.Alias + "`"
			}
			gx = gx.Select(expr)
		}
	}
	for _, ord := range o.Orders {
		col := "`" + ord.Column + "`"
		if !ord.Alias {
			if ord.Table != "" {
				ob, ok := resolver[ord.Table]
				if !ok || ob == nil {
					return ErrBadColumn
				}
				if err := validateColumn(ob, ord.Column); err != nil {
					return err
				}
				col = "`" + ord.Table + "`.`" + ord.Column + "`"
			} else if err := validateColumn(b, ord.Column); err != nil {
				return err
			}
		} else if !columnNameRe.MatchString(ord.Column) {
			return ErrBadColumn
		}
		dir := " ASC"
		if ord.Desc {
			dir = " DESC"
		}
		gx = gx.Order(col + dir)
	}
	if o.Limit > 0 {
		gx = gx.Limit(o.Limit)
	}
	if o.Offset > 0 {
		gx = gx.Offset(o.Offset)
	}
	return gx.Scan(out).Error
}

// renderRange 把一个结构化条件渲染成「列 操作符 ?」片段与参数。
//
// 列名按绑定校验（带表别名时按该别名对应绑定校验），操作符是白名单 —— 项目侧
// 永远传不进 SQL 文本。IN 的取值列表整体作为一个占位参数交给 gorm 展开。
func renderRange(b *binding, resolver map[string]*binding, baseAlias string, r Range) (string, []any, error) {
	col := "`" + r.Column + "`"
	if r.Table != "" {
		ob, ok := resolver[r.Table]
		if !ok || ob == nil {
			return "", nil, ErrBadColumn
		}
		if err := validateColumn(ob, r.Column); err != nil {
			return "", nil, err
		}
		col = "`" + r.Table + "`.`" + r.Column + "`"
	} else if err := validateColumn(b, r.Column); err != nil {
		return "", nil, err
	} else if baseAlias != "" {
		col = "`" + baseAlias + "`.`" + r.Column + "`"
	}
	switch r.Op {
	case "=", "<>", ">", ">=", "<", "<=", "LIKE":
		return col + " " + r.Op + " ?", []any{r.Value}, nil
	case "IN":
		vals, ok := r.Value.([]any)
		if !ok || len(vals) == 0 {
			return "", nil, ErrBadColumn
		}
		return col + " IN ?", []any{vals}, nil
	case "IS NULL", "IS NOT NULL":
		return col + " " + r.Op, nil, nil
	default:
		return "", nil, ErrBadColumn
	}
}
