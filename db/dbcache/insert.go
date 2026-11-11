package dbcache

// ============================================================================
// 插入原语：唯一键冲突视为 affected=0，不覆盖已有行
//
// 与 Save/Upsert 的区别：Save 是「整行覆盖」，适合读改写；Insert 是
// 「只插不改」，给幂等占位（op_id / client_request_id）用：冲突时调用方
// 必须回读既有行，而不是把对方的终态覆盖成自己的草稿。
// ============================================================================

import (
	"context"
	"reflect"
	"time"
)

// Insert 插入一行，唯一键冲突返回 affected=0（无错误）。
func Insert[T any](ctx context.Context, table string, row *T, opt ...Option) (int64, error) {
	e := Default()
	if e == nil {
		return 0, ErrNotInitialized
	}
	if row == nil {
		return 0, ErrNilRow
	}
	if st := txStateFrom(ctx); st != nil {
		b, err := e.lookupTyped(table, typeOf[T]())
		if err != nil {
			return 0, err
		}
		return st.insertInTx(ctx, b, any(row), ParseOptions(opt))
	}
	return e.Insert(ctx, table, row)
}

func (e *Engine) Insert[T any](ctx context.Context, table string, row *T) (int64, error) {
	if row == nil {
		return 0, ErrNilRow
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	// 自增列零值 → Redis INCR 即时取号并回填结构体（调用方 fn 内就要用 ID）。
	// 取号后冲突/回滚留空洞，与 MySQL auto-inc 回滚同构。
	if b.autoInc != "" {
		rv := reflect.ValueOf(row).Elem()
		fv := rv.Field(b.meta.Fields[b.meta.ByColumn[b.autoInc]].Index)
		if isZeroValue(fv) {
			n, aerr := e.store.SeqAlloc(ctx, b.keys.seq, 1)
			if aerr != nil {
				return 0, aerr
			}
			switch fv.Kind() {
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				fv.SetUint(uint64(n))
			default:
				fv.SetInt(n)
			}
		}
	}
	key, err := b.keyFromRow(any(row))
	if err != nil {
		return 0, err
	}
	fields, err := b.encodeFields(any(row))
	if err != nil {
		return 0, err
	}
	op := &writeOp{
		Op:      "insert",
		Fields:  fieldsToMap(fields),
		Due:     time.Now().Add(b.interval).UnixMilli(),
		TombTTL: e.tombTTL().Milliseconds(),
	}
	res, err := e.applyWrite(ctx, b, key, op)
	if err != nil {
		return 0, err
	}
	return res.Affected, nil
}

// TxInsert 事务内插入一行，唯一键冲突返回 affected=0；结果即时进 overlay。
func TxInsert[T any](t *Tx, ctx context.Context, table string, row *T) (int64, error) {
	if row == nil {
		return 0, ErrNilRow
	}
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	return st.insertInTx(ctx, b, any(row), &Options{})
}

// TxInsertBatch 事务内批量插入；唯一键冲突的行跳过（不覆盖），逐行即时进 overlay。
func TxInsertBatch[T any](t *Tx, ctx context.Context, table string, rows []*T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	st, err := t.state()
	if err != nil {
		return 0, err
	}
	b, err := st.e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	var total int64
	for _, row := range rows {
		if row == nil {
			return 0, ErrNilRow
		}
		n, ierr := st.insertInTx(ctx, b, any(row), &Options{})
		if ierr != nil {
			return 0, ierr
		}
		total += n
	}
	return total, nil
}
