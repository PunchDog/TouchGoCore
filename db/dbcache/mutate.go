package dbcache

// ============================================================================
// 原子写原语：Redis Lua 对 live 行校验条件后原子应用，返回受影响行数。
//
// 为什么不能复用 Save：Save 是「把整行草稿覆盖回去」，表达不了
// `col = col + ?` 与依赖 affected 的 CAS。资金列、状态跃迁只能用这里。
//
// 语义：
//   - 条件必须完整覆盖行键（拒绝全表/多行更新）；
//   - Increment/CAS 在 Redis 侧 Lua 原子执行（HINCRBY 保 int64 精确），
//     校验失败零副作用（affected=0）；成功后脏账登记，sink 异步物化。
// ============================================================================

import (
	"context"
	"strconv"
	"time"
)

// Increment 原子给 column 加 delta（数据库侧 `column = column + ?`），返回受影响行数。
func Increment(ctx context.Context, table string, cond any, column string, delta int64) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrNotInitialized
	}
	return e.Increment(ctx, table, cond, column, delta)
}

// CompareAndSwap 条件更新：WHERE 行键 + expect 全部相等时写 set，返回受影响行数。
func CompareAndSwap(ctx context.Context, table string, cond any, expect, set map[string]any) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrNotInitialized
	}
	return e.CompareAndSwap(ctx, table, cond, expect, set)
}

func (e *Engine) Increment(ctx context.Context, table string, cond any, column string, delta int64) (int64, error) {
	return e.atomicWrite(ctx, table, &Options{Cond: cond, Increment: column, Delta: delta})
}

func (e *Engine) CompareAndSwap(ctx context.Context, table string, cond any, expect, set map[string]any) (int64, error) {
	return e.atomicWrite(ctx, table, &Options{Cond: cond, CASExpect: expect, CASSet: set})
}

// AtomicMax 原子取大：column = max(column, value)，返回受影响行数。
func AtomicMax(ctx context.Context, table string, cond any, column string, value int64) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrNotInitialized
	}
	return e.AtomicMax(ctx, table, cond, column, value)
}

func (e *Engine) AtomicMax(ctx context.Context, table string, cond any, column string, value int64) (int64, error) {
	return e.atomicWrite(ctx, table, &Options{Cond: cond, AtomicMax: column, MaxValue: value})
}

// atomicWrite 事务外原子写：精确定位行键 → 结构化条件 → Lua 原子应用。
func (e *Engine) atomicWrite(ctx context.Context, table string, o *Options) (int64, error) {
	b, err := e.lookupTable(table)
	if err != nil {
		return 0, err
	}
	vals, err := parseCond(b.meta, b.kind, o.Cond)
	if err != nil {
		return 0, err
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return 0, err
	}
	conds := make([]condDesc, 0, len(o.Conditions))
	for _, r := range o.Conditions {
		c, cerr := condDescFromRange(b, r)
		if cerr != nil {
			return 0, cerr
		}
		conds = append(conds, c)
	}
	var anyOf [][]condDesc
	for _, group := range o.AnyOfGroups {
		g := make([]condDesc, 0, len(group))
		for _, r := range group {
			c, cerr := condDescFromRange(b, r)
			if cerr != nil {
				return 0, cerr
			}
			g = append(g, c)
		}
		anyOf = append(anyOf, g)
	}
	op := &writeOp{
		Conds:   conds,
		AnyOf:   anyOf,
		Due:     time.Now().Add(b.interval).UnixMilli(),
		TombTTL: e.tombTTL().Milliseconds(),
	}
	if o.Increment != "" {
		if err := validateColumn(b, o.Increment); err != nil {
			return 0, err
		}
		op.Op = "increment"
		op.Column = o.Increment
		op.Delta = o.Delta
	} else if o.AtomicMax != "" {
		if err := validateColumn(b, o.AtomicMax); err != nil {
			return 0, err
		}
		op.Op = "max"
		op.Column = o.AtomicMax
		op.Max = strconv.FormatInt(o.MaxValue, 10)
	} else {
		if len(o.CASSet) == 0 {
			return 0, ErrBadColumn
		}
		set, serr := encodeFieldMap(o.CASSet)
		if serr != nil {
			return 0, serr
		}
		for col := range o.CASSet {
			if err := validateColumn(b, col); err != nil {
				return 0, err
			}
		}
		expect := make(map[string]expectDesc, len(o.CASExpect))
		for col, v := range o.CASExpect {
			d, derr := expectDescFrom(b, col, v)
			if derr != nil {
				return 0, derr
			}
			expect[col] = d
		}
		op.Op = "cas"
		op.Set = set
		op.Expect = expect
	}
	res, err := e.applyWrite(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// validateColumn 列名必须存在于绑定结构体（防注入、防写不存在的列）。
func validateColumn(b *binding, column string) error {
	if !columnNameRe.MatchString(column) {
		return ErrBadColumn
	}
	if _, ok := b.meta.ByColumn[column]; !ok {
		return ErrBadColumn
	}
	return nil
}
