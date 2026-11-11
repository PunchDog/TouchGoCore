package dbcache

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"time"

	"touchgocore/metrics"
	"touchgocore/vars"
)

// typeOf 返回类型参数 T 的非指针类型。
func typeOf[T any]() reflect.Type { return reflect.TypeOf((*T)(nil)).Elem() }

// decodeRows 把行 hash 列表按 out 形状（*T 单条 / *[]T 列表）解码回填。
// 单条无命中返回 ErrNoRows；out 形状不符返回 ErrBadOut。
func decodeRows[T any](b *binding, rows []map[string]string, out any) error {
	switch o := out.(type) {
	case *T:
		if len(rows) == 0 {
			return ErrNoRows
		}
		row, err := b.decode(rows[0])
		if err != nil {
			return err
		}
		*o = *row.(*T)
		return nil
	case *[]T:
		res := make([]T, 0, len(rows))
		for _, h := range rows {
			row, err := b.decode(h)
			if err != nil {
				return err
			}
			res = append(res, *row.(*T))
		}
		*o = res
		return nil
	default:
		return ErrBadOut
	}
}

// Query 按条件查询：out 传 *T 查单条，传 *[]T 查列表。
//
// 单条且条件恰好覆盖全部行键列时优先读 Redis；命中即返回，读错误 fail-closed。
// 未命中默认返回 ErrNoRows（Redis 即真源）；仅 AllowSourceBackfill=true 的迁移期
// 表才回源数据库并回填，且墓碑存活（已删除）时拒绝回源以防复活。
// 列表与含非键列的查询扫 Redis 键集并按条件过滤（MaxScanRows fail-closed）；仅冷
// 启动迁移表（键集空 + AllowSourceBackfill）才回源数据库捞历史行并回填。
func Query[T any](ctx context.Context, table string, cond any, out any, opt ...Option) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	o := ParseOptions(opt)
	// 事务内优先走 overlay：带 Equal/Order/Limit 的读若被 spec 引擎接管，会完全
	// 看不到本事务刚插入/修改的行（读己写失效）。聚合/投影/JOIN 等 spec 专属形状
	// 暂不支持 overlay，退回 spec 引擎（保持既有语义）。
	if st := txStateFrom(ctx); st != nil {
		if txSpecUnsupported(o) {
			return querySpec[T](e, ctx, table, cond, out, o)
		}
		b, err := e.lookupTyped(table, typeOf[T]())
		if o.AllowAnyDest {
			// AnyDest 允许把绑定行按列名扫描到调用方模型，不要求类型相同。
			b, err = e.lookupTable(table)
		}
		if err != nil {
			return err
		}
		var vals []condVal
		if cond != nil {
			vals, err = parseCond(b.meta, b.kind, cond)
			if err != nil {
				return err
			}
		}
		// ForUpdate 只影响 exact-key 单行的行锁；列表形状在 overlay 上按行逐条读取，
		// 不在这里强制 *T（旧 spec 路径也不做该限制）。
		rows, err := st.matchingRows(ctx, b, vals, o)
		if err != nil {
			return err
		}
		if o.AllowAnyDest {
			return scanSpecResult(out, rows, o)
		}
		return decodeRows[T](b, rows, out)
	}
	if o.HasSpec() {
		return querySpec[T](e, ctx, table, cond, out, o)
	}
	return e.query[T](ctx, table, cond, out)
}

// txSpecUnsupported 标记事务路径无法在 overlay 上求值的读形状。
func txSpecUnsupported(o *Options) bool {
	return o.Agg != nil || len(o.Selects) > 0 || len(o.Groups) > 0 ||
		len(o.GroupAliases) > 0 || len(o.Joins) > 0 || o.WithSecrets
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
			// Redis 即真源：读错误 fail-closed 上抛，绝不静默回源 MySQL 制造双真源。
			if herr != nil {
				return herr
			}
			if len(h) > 0 {
				row, derr := b.decode(h)
				if derr != nil {
					return derr
				}
				*out = *row.(*T)
				return nil
			}
			// 未命中：默认不回源（资金表 fail-closed，库只是滞后物化视图）。
			if !b.allowSourceBackfill {
				return ErrNoRows
			}
			// 迁移期回源：墓碑存活=该行已删除，拒绝复活与陈旧读。
			alive, terr := e.store.TombAlive(ctx, b.keys.tombPrefix+key)
			if terr != nil {
				return terr
			}
			if alive {
				return ErrNoRows
			}
			row, err := b.srcFindOne(ctx, vals)
			if err != nil {
				return err
			}
			fields, ferr := b.encodeFields(row)
			if ferr == nil {
				if h := e.backfillRow(ctx, b, key, fields); len(h) > 0 {
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

	// 非键条件单条：Redis 键集扫描取首条命中（Redis 权威）；仅冷启动迁移表
	// （键集空 + AllowSourceBackfill）才回源 srcFindOne 并回填，保留 orderBy 语义。
	rows, cold, err := e.scanMatching(ctx, b, vals)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		row, derr := b.decode(rows[0])
		if derr != nil {
			return derr
		}
		*out = *row.(*T)
		return nil
	}
	if cold && b.allowSourceBackfill && b.srcFindOne != nil {
		row, serr := b.srcFindOne(ctx, vals)
		if serr != nil {
			return serr
		}
		if key, kerr := b.keyFromRow(row); kerr == nil {
			if fields, ferr := b.encodeFields(row); ferr == nil {
				e.backfillRow(ctx, b, key, fields)
			}
		}
		*out = *row.(*T)
		return nil
	}
	return ErrNoRows
}

// queryList 执行列表查询：Redis 键集扫描 + 条件过滤（Redis 权威，MaxScanRows
// fail-closed）。仅冷启动迁移表（键集空 + AllowSourceBackfill）回源 srcFindList
// 捞历史行并逐条回填；资金表（默认）键集空即返回空列表，绝不回源。
func (e *Engine) queryList[T any](ctx context.Context, b *binding, vals []condVal, out *[]T) error {
	rows, cold, err := e.scanMatching(ctx, b, vals)
	if err != nil {
		return err
	}
	if len(rows) == 0 && cold && b.allowSourceBackfill && b.srcFindList != nil {
		srcRows, serr := b.srcFindList(ctx, vals)
		if serr != nil {
			return serr
		}
		result := make([]T, 0, len(srcRows))
		for _, rowAny := range srcRows {
			if key, kerr := b.keyFromRow(rowAny); kerr == nil {
				if fields, ferr := b.encodeFields(rowAny); ferr == nil {
					e.backfillRow(ctx, b, key, fields)
				}
			}
			result = append(result, *rowAny.(*T))
		}
		*out = result
		return nil
	}
	return decodeRows[T](b, rows, out)
}

// scanMatching 扫 Redis 键集（MaxScanRows fail-closed），逐行读 hash 并按等值/IN
// 条件过滤，返回命中行 hash 与「键集是否为空（冷启动）」标志。Redis 读错误直接
// 上抛（fail-closed），绝不静默回源。这是非键单条 / 列表查询的 Redis 权威读基础。
func (e *Engine) scanMatching(ctx context.Context, b *binding, vals []condVal) ([]map[string]string, bool, error) {
	conds, err := condValsToDescs(b, vals)
	if err != nil {
		return nil, false, err
	}
	keys, err := e.store.KeySetScan(ctx, b.keys.keySet, e.cfg.MaxScanRows)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExceeded) {
			metrics.DBCache.IncScanBudget(b.table)
		}
		return nil, false, err
	}
	cold := len(keys) == 0
	sort.Strings(keys)
	rows := make([]map[string]string, 0, len(keys))
	for _, key := range keys {
		h, herr := e.store.HGetAll(ctx, b.keys.rowPrefix+key)
		if herr != nil {
			return nil, false, herr
		}
		if len(h) == 0 || !evalConds(h, conds, nil) {
			continue
		}
		rows = append(rows, h)
	}
	return rows, cold, nil
}

// backfillRow 尽力回填一行（save NX：已有不覆盖，维护索引/键集/版本）；
// 失败只告警，不改变调用方已从数据库拿到的返回值。
func (e *Engine) backfillRow(ctx context.Context, b *binding, key string, fields []string) map[string]string {
	op := &writeOp{
		Op:      "save",
		NX:      true,
		Fields:  fieldsToMap(fields),
		Due:     time.Now().Add(b.interval).UnixMilli(),
		TombTTL: e.tombTTL().Milliseconds(),
	}
	res, err := e.applyWrite(ctx, b, key, op)
	if err != nil {
		vars.Warning("dbcache %s: backfill %s: %v", b.table, key, err)
		return nil
	}
	return res.Row
}
