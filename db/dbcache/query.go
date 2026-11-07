package dbcache

import (
	"context"
	"reflect"

	"touchgocore/vars"
)

// typeOf 返回类型参数 T 的非指针类型。
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// Query 按条件查询：out 传 *T 查单条，传 *[]T 查列表。
//
// 单条且条件恰好覆盖全部行键列时优先读 Redis；未命中回源数据库并回填。
// 列表与含非键列的查询直接读数据库，命中行回填 Redis 但不改变返回结果。
func Query[T any](ctx context.Context, table string, cond any, out any) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
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
