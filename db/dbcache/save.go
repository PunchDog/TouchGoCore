package dbcache

import (
	"context"
	"fmt"
	"time"
)

// Save 全值写入一行：先写 Redis 全字段，再按 mustSave 决定立即落库或延迟落库。
//
// key 为空时按表的 KeyColumns 从 row 推导；mustSave=true 时返回前会尝试写入数据库，
// 写入失败会保留脏账并返回错误。
func Save[T any](ctx context.Context, table string, key string, row *T, mustSave bool, opt ...Option) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	o := ParseOptions(opt)
	if st := txStateFrom(ctx); st != nil {
		atomic := o.Increment != "" || o.CASSet != nil
		var b *binding
		var err error
		if atomic {
			// 原子写只按表找绑定：调用方不需要构造缓存行类型，只给条件与列。
			b, err = e.lookupTable(table)
		} else {
			b, err = e.lookupTyped(table, typeOf[T]())
		}
		if err != nil {
			return err
		}
		if key == "" && !atomic {
			if row == nil {
				return ErrNilRow
			}
			key, err = b.keyFromRow(any(row))
			if err != nil {
				return err
			}
		}
		var payload any
		if row != nil {
			payload = any(row)
		}
		return st.saveInTx(ctx, b, key, payload, o)
	}
	if o.Sequence != nil {
		return fmt.Errorf("dbcache: 原子取号必须在事务内执行")
	}
	if o.Increment != "" || o.CASSet != nil {
		// 事务外的原子写：直接落库并同步刷新缓存行（资金/状态列必须走这条）。
		b, c, err := e.resolveMySQL(table)
		if err != nil {
			return err
		}
		gx := c.Engine().WithContext(ctx)
		var n int64
		if o.Increment != "" {
			n, err = e.incrementOn(ctx, gx, b, o.Cond, o.Increment, o.Delta, nil)
		} else {
			n, err = e.casOn(ctx, gx, b, o.Cond, o.CASExpect, o.CASSet, nil)
		}
		if o.Affected != nil {
			*o.Affected = n
		}
		return err
	}
	return e.save[T](ctx, table, key, row, mustSave)
}

func (e *Engine) save[T any](ctx context.Context, table string, key string, row *T, mustSave bool) error {
	if row == nil {
		return ErrNilRow
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	rowAny := any(row)
	if key == "" {
		key, err = b.keyFromRow(rowAny)
		if err != nil {
			return err
		}
	}
	fields, err := b.encodeFields(rowAny)
	if err != nil {
		return err
	}
	rowKey := b.keys.rowPrefix + key
	if err := e.store.HSetAll(ctx, rowKey, b.keys.tombPrefix+key, fields, b.ttl); err != nil {
		// 立即写不能因 Redis 故障丢失：退化为直写数据库。
		if mustSave {
			if uerr := e.upsert(ctx, b, []any{rowAny}); uerr != nil {
				return fmt.Errorf("dbcache: save %s: redis=%v db=%w", table, err, uerr)
			}
			return nil
		}
		return fmt.Errorf("dbcache: save %s: %w", table, err)
	}

	due := time.Now().Add(b.interval).UnixMilli()
	if mustSave {
		due = time.Now().UnixMilli()
	}
	if err := e.store.ZAdd(ctx, b.keys.dirty, key, due); err != nil {
		// 立即写路径在脏账登记失败时同样退化为直写数据库，避免 Redis 有新值却永不落库。
		if mustSave {
			if uerr := e.upsert(ctx, b, []any{rowAny}); uerr != nil {
				return fmt.Errorf("dbcache: save %s: mark dirty=%v db=%w", table, err, uerr)
			}
			return nil
		}
		return fmt.Errorf("dbcache: save %s: mark dirty: %w", table, err)
	}
	if mustSave {
		return e.flushKey(ctx, b, key)
	}
	return nil
}

// flushKey 立即落库单个键；若并发写入刷新了脏账 score，则重试领取最新值。
func (e *Engine) flushKey(ctx context.Context, b *binding, key string) error {
	for attempt := 0; attempt < 3; attempt++ {
		score, ok, err := e.store.ZScore(ctx, b.keys.dirty, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		h, claimed, err := e.store.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+key, b.keys.tombPrefix+key, key, score)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		row, err := b.decode(h)
		if err != nil {
			e.markRetry(ctx, b, key)
			return err
		}
		if err := e.upsert(ctx, b, []any{row}); err != nil {
			e.markRetry(ctx, b, key)
			return err
		}
		_ = e.store.HDel(ctx, b.keys.retry, key)
		return nil
	}
	return nil
}
