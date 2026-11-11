package dbcache

import (
	"context"
	"fmt"
	"strings"

	mysqldb "touchgocore/db/mysql"
)

// BindMySQL 把一张表绑定到缓存层。c 非空时挂载 MySQL 回源 + 异步 sink（滞后物化
// 视图）；c 为 nil 时是纯 Redis 权威绑定——无回源、无 sink，拔掉 MySQL 不影响读写。
// 无论是否挂载 sink，Redis 都是唯一读写权威；sink 只是提交后由脏账异步物化。
func BindMySQL[T any](table string, c *mysqldb.Client, opt BindOption[T]) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	keys, err := makeTableKeys(e.cfg.KeyPrefix, table, e.cfg.GlobalSlot)
	if err != nil {
		return err
	}
	b, err := newBinding[T](table, backendMySQL, opt, keys, e.cfg)
	if err != nil {
		return err
	}
	if c == nil {
		// 纯 Redis 权威：无源可回，强制关闭回源（资金表 fail-closed 语义）。
		b.allowSourceBackfill = false
		return e.register(b)
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
	// srcPage 按首个行键列升序分页捞源表，供 BackfillTable 全量播种。
	b.srcPage = func(ctx context.Context, offset, limit int) ([]any, error) {
		q := mysqldb.NewQuery().Limit(limit).Offset(offset)
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
		return rep.DeleteBy(ctx, buildMySQLQuery(vals, nil))
	}
	// sinkSecrets 把秘密列（不在模型结构体里，只进 Redis :s: hash）按行键落库。
	// 行已由 sinkBatch upsert，这里只 UPDATE 秘密列；列名来自白名单、值参数化。
	b.sinkSecrets = func(ctx context.Context, vals []condVal, secrets map[string]any) error {
		if len(secrets) == 0 {
			return nil
		}
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
		if len(parts) == 0 {
			return fmt.Errorf("%w: secret sink without row key", ErrKeyMissing)
		}
		return c.Engine().WithContext(ctx).Table(rep.TableName()).
			Where(strings.Join(parts, " AND "), args...).Updates(secrets).Error
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
