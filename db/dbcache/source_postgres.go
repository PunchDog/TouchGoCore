package dbcache

import (
	"context"
	"fmt"
	"strings"

	pgdb "touchgocore/db/postgres"
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
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table)
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
	return e.register(b)
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
