package dbcache

// ============================================================================
// 事务原语：同一 MySQL 连接内读写，缓存刷新在 COMMIT 成功后执行
//
// 与 Save 的延迟回写不同：事务内的写直接落 MySQL（真源），提交成功后才把
// 变更行重读并写穿 Redis；回滚/panic 不刷新缓存。
// ============================================================================

import (
	"context"
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	mysqldb "touchgocore/db/mysql"
	"touchgocore/vars"
)

// Tx 是一组在单连接上执行的读写；由 BeginTx 创建。
type Tx struct {
	e         *Engine
	raw       *mysqldb.Tx
	refreshes []func()
}

// BeginTx 开启缓存层事务。
func BeginTx(ctx context.Context) (*Tx, error) {
	e := Default()
	if e == nil {
		return nil, ErrNotInitialized
	}
	e.mu.RLock()
	c := e.mysqlClient
	e.mu.RUnlock()
	if c == nil {
		return nil, ErrTableUnbound
	}
	raw, err := c.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{e: e, raw: raw}, nil
}

// WrapTx 用已有事务句柄构造缓存层事务（测试/集成场景复用外部连接）。
func WrapTx(raw *mysqldb.Tx) *Tx { return &Tx{e: Default(), raw: raw} }

// Commit 提交并执行提交后刷新；刷新失败只影响缓存新鲜度，不回滚已提交事务。
func (t *Tx) Commit() error {
	if err := t.raw.Commit(); err != nil {
		return err
	}
	for _, fn := range t.refreshes {
		fn()
	}
	return nil
}

// Rollback 回滚事务；丢弃全部提交后刷新动作。
func (t *Tx) Rollback() error { return t.raw.Rollback() }

// Context 返回事务 ctx。
func (t *Tx) Context() context.Context { return t.raw.Context() }

// DB 暴露事务内的 gorm 会话（迁移期兼容直连写；新代码优先用 CacheTx* 原语）。
func (t *Tx) DB() *gorm.DB { return t.raw.DB }

// TxQuery 事务内查询：单条（*T）或列表（*[]T），始终读事务连接，不读 Redis。
func TxQuery[T any](t *Tx, ctx context.Context, table string, cond any, out any) error {
	e, err := t.engine()
	if err != nil {
		return err
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	repo := mysqldb.NewTxRepository[T](t.raw)
	q := buildMySQLQuery(vals, b.orderBy)
	switch o := out.(type) {
	case *T:
		row, ferr := repo.FindOne(ctx, q)
		if ferr != nil {
			return ferr
		}
		*o = *row
	case *[]T:
		rows, ferr := repo.FindAll(ctx, q)
		if ferr != nil {
			return ferr
		}
		res := make([]T, 0, len(rows))
		for _, r := range rows {
			res = append(res, *r)
		}
		*o = res
	default:
		return ErrBadOut
	}
	return nil
}

// TxLockOne 事务内按行键 SELECT ... FOR UPDATE。
func TxLockOne[T any](t *Tx, ctx context.Context, table string, cond any, out *T) error {
	e, err := t.engine()
	if err != nil {
		return err
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return err
	}
	if !condIsExactKey(b.meta, vals) {
		return ErrPartialKey
	}
	gx := applyCond(t.raw.DB.WithContext(ctx).Table(b.table), vals).
		Clauses(clause.Locking{Strength: "UPDATE"})
	return gx.Take(out).Error
}

// TxSave 事务内整行 upsert（冲突列 = 绑定的 KeyColumns），提交后刷新该行缓存。
func TxSave[T any](t *Tx, ctx context.Context, table string, row *T) error {
	if row == nil {
		return ErrNilRow
	}
	e, err := t.engine()
	if err != nil {
		return err
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	cols := make([]clause.Column, len(b.conflictCols))
	for i, c := range b.conflictCols {
		cols[i] = clause.Column{Name: c}
	}
	gx := t.raw.DB.WithContext(ctx).Table(table).
		Clauses(clause.OnConflict{Columns: cols, UpdateAll: true})
	if err := gx.Create(row).Error; err != nil {
		return err
	}
	if vals := keyValsFromRow(b, any(row)); len(vals) > 0 {
		t.refreshes = append(t.refreshes, func() {
			e.refreshFromDB(context.Background(), b, vals)
		})
	}
	return nil
}

// TxIncrement 事务内原子累加，提交后刷新该行。
func TxIncrement(t *Tx, ctx context.Context, table string, cond any, column string, delta int64) (int64, error) {
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, _, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	return e.incrementOn(ctx, t.raw.DB.WithContext(ctx), b, cond, column, delta, t.afterCommit)
}

// TxCompareAndSwap 事务内条件更新，提交后刷新该行。
func TxCompareAndSwap(t *Tx, ctx context.Context, table string, cond any, expect, set map[string]any) (int64, error) {
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, _, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	return e.casOn(ctx, t.raw.DB.WithContext(ctx), b, cond, expect, set, t.afterCommit)
}

// TxUpdateIn 事务内按单列 id 集合做条件更新（多行 CAS），提交后逐行刷新缓存。
func TxUpdateIn(t *Tx, ctx context.Context, table, keyColumn string, ids []any, expect, set map[string]any) (int64, error) {
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, _, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := validateColumn(b, keyColumn); err != nil {
		return 0, err
	}
	if len(set) == 0 {
		return 0, ErrBadColumn
	}
	for col := range expect {
		if err := validateColumn(b, col); err != nil {
			return 0, err
		}
	}
	for col := range set {
		if err := validateColumn(b, col); err != nil {
			return 0, err
		}
	}
	gx := t.raw.DB.WithContext(ctx).Table(table).Where("`"+keyColumn+"` IN ?", ids)
	for col, v := range expect {
		gx = applyExpect(gx, col, v)
	}
	res := gx.UpdateColumns(set)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		for _, id := range ids {
			vals := []condVal{{Column: keyColumn, Values: []any{id}}}
			t.refreshes = append(t.refreshes, func() {
				e.refreshFromDB(context.Background(), b, vals)
			})
		}
	}
	return res.RowsAffected, nil
}

// engine 返回已初始化的引擎；未装配缓存时返回 ErrNotInitialized（调用方降级直连）。
func (t *Tx) engine() (*Engine, error) {
	if t == nil || t.e == nil {
		return nil, ErrNotInitialized
	}
	return t.e, nil
}

// afterCommit 把刷新动作排进提交后队列。
func (t *Tx) afterCommit(fn func()) { t.refreshes = append(t.refreshes, fn) }

// TxDeleteIn 事务内按单列 id 集合删除，提交后清对应缓存行。
func TxDeleteIn(t *Tx, ctx context.Context, table, keyColumn string, ids []any) (int64, error) {
	e, err := t.engine()
	if err != nil {
		return 0, err
	}
	b, _, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if err := validateColumn(b, keyColumn); err != nil {
		return 0, err
	}
	res := t.raw.DB.WithContext(ctx).Table(table).Where("`"+keyColumn+"` IN ?", ids).Delete(nil)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		for _, id := range ids {
			vals := []condVal{{Column: keyColumn, Values: []any{id}}}
			t.refreshes = append(t.refreshes, func() { e.dropCached(b, vals) })
		}
	}
	return res.RowsAffected, nil
}

// dropCached 在事务提交后清掉一行的缓存（墓碑 + 行 hash + 脏账）。
func (e *Engine) dropCached(b *binding, vals []condVal) {
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		return
	}
	tombKey := b.keys.tombPrefix + key
	ttl := e.tombTTL()
	ctx := context.Background()
	if err := e.store.SetTomb(ctx, tombKey, ttl); err != nil {
		return
	}
	if _, err := e.store.DelRow(ctx, b.keys.dirty, b.keys.rowPrefix+key, b.keys.retry, tombKey, key, ttl); err != nil {
		vars.Warning("dbcache %s: 删除后清缓存失败（库已提交）: %v", b.table, err)
	}
}

// keyValsFromRow 从行结构体按行键列取值，用于提交后重读。
func keyValsFromRow(b *binding, row any) []condVal {
	v := reflect.ValueOf(row)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	out := make([]condVal, 0, len(b.meta.KeyCols))
	for _, col := range b.meta.KeyCols {
		fi, ok := b.meta.ByColumn[col]
		if !ok {
			return nil
		}
		out = append(out, condVal{Column: col, Values: []any{v.Field(b.meta.Fields[fi].Index).Interface()}})
	}
	return out
}
