package dbcache

import (
	"context"
	"fmt"
	"time"
)

// Delete 物理删除一行。
//
// cond 与 Query 的条件结构体同形（指针字段 + cache:"col" tag），且必须恰好覆盖
// 全部 KeyColumns：只允许按完整行键删除，拒绝可能命中多行的条件。删除在 Redis
// 侧一条 Lua 原子完成「立墓碑 + 删行/秘密/索引/守卫/键集 + 销脏账/重试/租约」，
// 有 sink 时入 dirtyDel 异步删库（失败退避重试不丢）；Redis 写成功即删除成功。
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

// remove 执行单行删除：Redis 原子删 + 有 sink 时入 dirtyDel 异步物化。
func (e *Engine) remove(ctx context.Context, b *binding, cond any) error {
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
	op := &writeOp{
		Op:      "delete",
		Sink:    b.sinkDelete != nil,
		Due:     time.Now().Add(b.interval).UnixMilli(),
		TombTTL: e.tombTTL().Milliseconds(),
	}
	if _, err := e.applyWrite(ctx, b, key, op); err != nil {
		return fmt.Errorf("dbcache: delete %s: %w", b.table, err)
	}
	return nil
}

// tombTTL 是删除墓碑的存活时间：覆盖一次在途落库的最长耗时尚有余量。
func (e *Engine) tombTTL() time.Duration {
	ttl := 3 * e.cfg.FlushTimeout
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	return ttl
}
