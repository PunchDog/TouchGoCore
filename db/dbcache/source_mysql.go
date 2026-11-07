package dbcache

import (
	"context"
	"fmt"
	"strings"

	mysqldb "touchgocore/db/mysql"
)

// BindMySQL 把一张表绑定到 MySQL 后端。
func BindMySQL[T any](table string, c *mysqldb.Client, opt BindOption[T]) error {
	if c == nil {
		return fmt.Errorf("%w: mysql client is nil", ErrTableUnbound)
	}
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table)
	if err != nil {
		return err
	}
	b, err := newBinding[T](table, backendMySQL, opt, keys, e.cfg)
	if err != nil {
		return err
	}
	rep := mysqldb.NewRepository[T](c)
	b.srcFindOne = func(ctx context.Context, vals []condVal) (any, error) {
		row, ferr := rep.FindOne(ctx, buildMySQLQuery(vals, b.orderBy))
		if ferr != nil {
			if mysqldb.IsNotFound(ferr) {
				return nil, ErrNoRows
			}
			return nil, ferr
		}
		return row, nil
	}
	b.srcFindList = func(ctx context.Context, vals []condVal) ([]any, error) {
		rows, ferr := rep.FindAll(ctx, buildMySQLQuery(vals, nil))
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
	return e.register(b)
}

// buildMySQLQuery 把条件拼成单个 AND 表达式。
func buildMySQLQuery(vals []condVal, order []string) *mysqldb.Query {
	q := mysqldb.NewQuery()
	if len(vals) > 0 {
		parts := make([]string, 0, len(vals))
		args := make([]any, 0, len(vals))
		for _, v := range vals {
			col := "`" + v.Column + "`"
			if len(v.Values) == 1 {
				parts = append(parts, col+" = ?")
				args = append(args, v.Values[0])
				continue
			}
			ph := strings.TrimSuffix(strings.Repeat("?,", len(v.Values)), ",")
			parts = append(parts, col+" IN ("+ph+")")
			args = append(args, v.Values...)
		}
		q.Where(strings.Join(parts, " AND "), args...)
	}
	for _, o := range order {
		if formatted, ok := formatSQLOrder(o, "`"); ok {
			q.OrderBy(formatted)
		}
	}
	return q
}

// formatSQLOrder 校验并引用排序列，如 "id DESC" → "`id` DESC"。
func formatSQLOrder(raw, quote string) (string, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", false
	}
	dir := ""
	if len(fields) >= 2 {
		last := strings.ToUpper(fields[len(fields)-1])
		if last == "ASC" || last == "DESC" {
			dir = last
			fields = fields[:len(fields)-1]
		}
	}
	for i, f := range fields {
		if !columnNameRe.MatchString(f) {
			return "", false
		}
		fields[i] = quote + f + quote
	}
	out := strings.Join(fields, " ")
	if dir != "" {
		out += " " + dir
	}
	return out, true
}
