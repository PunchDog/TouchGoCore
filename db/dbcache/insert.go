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

	"gorm.io/gorm/clause"
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
	_, c, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	res := c.Engine().WithContext(ctx).Table(table).
		Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		if vals := keyValsFromRow(b, any(row)); len(vals) > 0 {
			e.refreshFromDB(ctx, b, vals)
		}
	}
	return res.RowsAffected, nil
}

// TxInsert 事务内插入一行，唯一键冲突返回 affected=0；提交后刷新该行。
func TxInsert[T any](t *Tx, ctx context.Context, table string, row *T) (int64, error) {
	if row == nil {
		return 0, ErrNilRow
	}
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	res := t.raw.DB.WithContext(ctx).Table(table).
		Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 && b != nil {
		if vals := keyValsFromRow(b, any(row)); len(vals) > 0 {
			t.refreshes = append(t.refreshes, func() {
				e.refreshFromDB(context.Background(), b, vals)
			})
		}
	}
	return res.RowsAffected, nil
}

// TxInsertBatch 事务内批量插入；唯一键冲突的行跳过（不覆盖），提交后刷新插入行。
func TxInsertBatch[T any](t *Tx, ctx context.Context, table string, rows []*T) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return 0, err
	}
	res := t.raw.DB.WithContext(ctx).Table(table).
		Clauses(clause.OnConflict{DoNothing: true}).Create(&rows)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 && b != nil {
		for _, row := range rows {
			vals := keyValsFromRow(b, any(row))
			if len(vals) == 0 {
				continue
			}
			t.refreshes = append(t.refreshes, func() {
				e.refreshFromDB(context.Background(), b, vals)
			})
		}
	}
	return res.RowsAffected, nil
}
