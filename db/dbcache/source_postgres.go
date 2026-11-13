package dbcache

import (
	"context"
	"fmt"
	"strings"

	pgdb "touchgocore/db/postgres"
	"touchgocore/vars"
)

// BindPostgres 把一张表绑定到 PostgreSQL 后端。
func BindPostgres[T any](table string, c *pgdb.Client, opt BindOption[T]) error {
	if c == nil {
		return fmt.Errorf("%w: postgres client is nil", ErrTableUnbound)
	}
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table, e.cfg.GlobalSlot)
	if err != nil {
		return err
	}
	b, err := newBinding[T](table, backendPostgres, opt, keys, e.cfg)
	if err != nil {
		return err
	}
	rep := pgdb.NewRepository[T](c)
	b.srcFindOne = func(ctx context.Context, vals []condVal) (any, error) {
		row, ferr := rep.FindOne(ctx, buildPostgresQuery(vals, b.orderBy))
		if ferr != nil {
			if pgdb.IsNotFound(ferr) {
				return nil, ErrNoRows
			}
			return nil, ferr
		}
		return row, nil
	}
	b.srcFindList = func(ctx context.Context, vals []condVal) ([]any, error) {
		rows, ferr := rep.FindAll(ctx, buildPostgresQuery(vals, nil))
		if ferr != nil {
			return nil, ferr
		}
		out := make([]any, len(rows))
		for i, row := range rows {
			out[i] = row
		}
		return out, nil
	}
	// srcPage 按首个行键列升序分页捞源表，供 BackfillTable 全量播种。
	b.srcPage = func(ctx context.Context, offset, limit int) ([]any, error) {
		q := pgdb.NewQuery().Limit(limit).Offset(offset)
		if len(b.meta.KeyCols) > 0 {
			q.OrderAsc(b.meta.KeyCols[0])
		}
		rows, ferr := rep.FindAll(ctx, q)
		if ferr != nil {
			return nil, ferr
		}
		out := make([]any, len(rows))
		for i, row := range rows {
			out[i] = row
		}
		return out, nil
	}
	// seedSeq：自增表的序列播种（取源表 MAX(自增列) 抬高 :seq）。
	// 与 MySQL 同源同因：新前缀冷启动序列从 0 起，取号会撞上回填/历史行键，
	// Insert 冲突只返回 affected=0，只追加台账会因此丢行。
	if b.autoInc != "" && columnNameRe.MatchString(b.autoInc) {
		col := b.autoInc
		srcTable := rep.TableName()
		b.seedSeq = func(ctx context.Context) (int64, error) {
			var maxID int64
			row := c.Engine().WithContext(ctx).Table(srcTable).
				Select(`COALESCE(MAX("` + col + `"), 0)`).Row()
			if serr := row.Scan(&maxID); serr != nil {
				return 0, serr
			}
			if maxID <= 0 {
				return 0, nil
			}
			if serr := e.store.SeqSeed(ctx, b.keys.seq, maxID); serr != nil {
				return 0, serr
			}
			return maxID, nil
		}
	}
	b.sinkBatch = func(ctx context.Context, rows []any) error {
		typed := make([]*T, len(rows))
		for i, row := range rows {
			typed[i] = row.(*T)
		}
		if opt.FlushFn != nil {
			return opt.FlushFn(ctx, typed)
		}
		return rep.UpsertBatch(ctx, typed, b.conflictCols)
	}
	b.sinkDelete = func(ctx context.Context, vals []condVal) error {
		return rep.DeleteBy(ctx, buildPostgresQuery(vals, nil))
	}
	if err := e.register(b); err != nil {
		return err
	}
	// 绑定时播种一次（库里可能已有大量历史行，而 Redis 序列是新的）。
	if b.seedSeq != nil {
		if _, serr := b.seedSeq(context.Background()); serr != nil {
			vars.Error("dbcache %s: 自增序列播种失败，取号可能与既有行键冲突: %v", table, serr)
		}
	}
	return nil
}

// buildPostgresQuery 把条件拼成单个 AND 表达式。
func buildPostgresQuery(vals []condVal, order []string) *pgdb.Query {
	q := pgdb.NewQuery()
	if len(vals) > 0 {
		parts := make([]string, 0, len(vals))
		args := make([]any, 0, len(vals))
		for _, v := range vals {
			col := `"` + v.Column + `"`
			if len(v.Values) == 1 {
				parts = append(parts, col+" = ?")
				args = append(args, v.Values[0])
				continue
			}
			ph := make([]string, len(v.Values))
			for i := range v.Values {
				ph[i] = "?"
			}
			parts = append(parts, col+" IN ("+strings.Join(ph, ",")+")")
			args = append(args, v.Values...)
		}
		q.Where(strings.Join(parts, " AND "), args...)
	}
	for _, o := range order {
		if formatted, ok := formatSQLOrder(o, `"`); ok {
			q.OrderBy(formatted)
		}
	}
	return q
}
