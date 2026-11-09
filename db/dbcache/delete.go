package dbcache

import (
	"context"
	"fmt"
	"time"

	"touchgocore/vars"
)

// Delete 物理删除一行。
//
// cond 与 Query 的条件结构体同形（指针字段 + cache:"col" tag），且必须恰好覆盖
// 全部 KeyColumns：只允许按完整行键删除，拒绝可能命中多行的条件。删除顺序为
// 「立墓碑 → 删数据库 → 清 Redis 行 hash / 脏账 / 重试账」；数据库删除失败时
// 回滚墓碑并原样保留 Redis 内容，由调用方重试。
func Delete(ctx context.Context, table string, cond any, opt ...Option) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	o := ParseOptions(opt)
	if st := txStateFrom(ctx); st != nil {
		// 结构化批量删：调用方不给单行条件，只给 Conditions（如 id IN (...)）。
		if cond == nil && len(o.Conditions) > 0 {
			n, derr := st.deleteWhereInTx(ctx, b, o.Conditions)
			if o.Affected != nil {
				*o.Affected = n
			}
			return derr
		}
		vals, perr := parseCond(b.meta, b.kind, cond)
		if perr != nil {
			return perr
		}
		if len(o.Conditions) > 0 {
			vals, perr = applyRangeConds(b, vals, o.Conditions)
			if perr != nil {
				return perr
			}
		}
		if !condIsExactKey(b.meta, vals) {
			return fmt.Errorf("%w: %s", ErrPartialKey, b.table)
		}
		return st.deleteInTx(ctx, b, vals)
	}
	if cond == nil && len(o.Conditions) > 0 {
		return fmt.Errorf("%w: %s", ErrPartialKey, b.table)
	}
	return e.remove(ctx, b, cond)
}

// applyRangeConds 把结构化条件并进删除条件：等值并入原条件，其余拒绝。
//
// 事务内批量删除只支持「单列等值」并进行键；IN 走 deleteWhereInTx。
func applyRangeConds(b *binding, vals []condVal, ranges []Range) ([]condVal, error) {
	for _, r := range ranges {
		if err := validateColumn(b, r.Column); err != nil {
			return nil, err
		}
		if r.Op != "=" {
			return nil, ErrBadColumn
		}
		vals = append(vals, condVal{Column: r.Column, Values: []any{r.Value}})
	}
	return vals, nil
}

// remove 执行单行删除。
func (e *Engine) remove(ctx context.Context, b *binding, cond any) error {
	if b.sinkDelete == nil {
		return fmt.Errorf("%w: table %s has no delete source", ErrBadCond, b.table)
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	if !condIsExactKey(b.meta, vals) {
		return fmt.Errorf("%w: %s", ErrPartialKey, b.table)
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return err
	}
	tombKey := b.keys.tombPrefix + key
	ttl := e.tombTTL()
	if err := e.store.SetTomb(ctx, tombKey, ttl); err != nil {
		return fmt.Errorf("dbcache: delete %s: tomb: %w", b.table, err)
	}
	if err := e.deleteRows(ctx, b, vals); err != nil {
		if derr := e.store.DelTomb(ctx, tombKey); derr != nil {
			vars.Error("dbcache %s: rollback tomb %s: %v", b.table, key, derr)
		}
		return fmt.Errorf("dbcache: delete %s: %w", b.table, err)
	}
	if _, err := e.store.DelRow(ctx, b.keys.dirty, b.keys.rowPrefix+key, b.keys.retry, tombKey, key, ttl); err != nil {
		// 数据库已删除、墓碑已立，但行 hash 可能还在：queryOne 的点查命中行 hash
		// 就直接返回、不检查墓碑，只告警会留下「库已删、缓存仍返回旧行」的静默不一致。
		// 与 mutate.go 的刷新失败统一口径：升级为 Error + 主动失效行（重试 DelRow）。
		vars.Error("dbcache %s: clear cached row %s failed (row invalidated): %v", b.table, key, err)
		e.invalidateCached(ctx, b, vals)
	}
	return nil
}

// deleteRows 以落库超时预算调用绑定的删除闭包。
func (e *Engine) deleteRows(ctx context.Context, b *binding, vals []condVal) error {
	fctx, cancel := context.WithTimeout(ctx, e.cfg.FlushTimeout)
	defer cancel()
	return b.sinkDelete(fctx, vals)
}

// tombTTL 是删除墓碑的存活时间：覆盖一次在途落库的最长耗时尚有余量。
func (e *Engine) tombTTL() time.Duration {
	ttl := 3 * e.cfg.FlushTimeout
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	return ttl
}
