package dbcache

// ============================================================================
// 原子写原语：数据库侧执行，成功后重读该行写穿缓存
//
// 为什么不能复用 Save：Save 是「把整行草稿覆盖回去」，表达不了
// `col = col + ?` 与依赖 RowsAffected 的 CAS。资金列、状态跃迁只能用这里。
//
// 语义：
//   - 条件必须完整覆盖行键（拒绝全表/多行更新）；
//   - 更新在 MySQL 同步执行，失败原样上抛；
//   - 成功且 affected>0 时重读该行刷新 Redis；刷新失败由 invalidateCached
//     主动失效该行（立墓碑 + 删旧行 hash + 销脏账/重试账）并写 vars.Error，
//     绝不留下「库新缓存旧」的静默不一致。
// ============================================================================

import (
	"context"
	"errors"
	"reflect"

	"gorm.io/gorm"

	mysqldb "touchgocore/db/mysql"
	"touchgocore/vars"
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
	b, c, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	return e.incrementOn(ctx, c.Engine().WithContext(ctx), b, cond, column, delta, nil)
}

func (e *Engine) CompareAndSwap(ctx context.Context, table string, cond any, expect, set map[string]any) (int64, error) {
	b, c, err := e.resolveMySQL(table)
	if err != nil {
		return 0, err
	}
	return e.casOn(ctx, c.Engine().WithContext(ctx), b, cond, expect, set, nil)
}

// resolveMySQL 取绑定 + 已注入的 MySQL 客户端。
//
// 仅 MySQL 绑定支持事务/原子写 options；PostgreSQL / MongoDB 保持直连仓储，
// 不参与本层迁移。
func (e *Engine) resolveMySQL(table string) (*binding, *mysqldb.Client, error) {
	b, err := e.lookupTable(table)
	if err != nil {
		return nil, nil, err
	}
	if b.kind != backendMySQL {
		return nil, nil, ErrTableUnbound
	}
	e.mu.RLock()
	c := e.mysqlClient
	e.mu.RUnlock()
	if c == nil {
		return nil, nil, ErrTableUnbound
	}
	return b, c, nil
}

// incrementOn 在给定 gorm 会话上执行原子累加；after 非空时把刷新动作延后（事务提交后）。
func (e *Engine) incrementOn(ctx context.Context, gx *gorm.DB, b *binding, cond any, column string, delta int64, after func(func())) (int64, error) {
	if err := validateColumn(b, column); err != nil {
		return 0, err
	}
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return 0, err
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	res := applyCond(gx.Table(b.table), vals).
		UpdateColumn(column, gorm.Expr("`"+column+"` + ?", delta))
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		e.scheduleRefresh(b, vals, after)
	}
	return res.RowsAffected, nil
}

// casOn 在给定 gorm 会话上执行条件更新；expect 全部命中才写 set。
func (e *Engine) casOn(ctx context.Context, gx *gorm.DB, b *binding, cond any, expect, set map[string]any, after func(func())) (int64, error) {
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
	vals, err := parseCond(b.meta, b.kind, cond)
	if err != nil {
		return 0, err
	}
	if !condIsExactKey(b.meta, vals) {
		return 0, ErrPartialKey
	}
	gx = applyCond(gx.Table(b.table), vals)
	for col, v := range expect {
		gx = applyExpect(gx, col, v)
	}
	res := gx.UpdateColumns(set)
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		e.scheduleRefresh(b, vals, after)
	}
	return res.RowsAffected, nil
}

// scheduleRefresh 立即刷新，或把刷新动作交给事务的提交后队列。
func (e *Engine) scheduleRefresh(b *binding, vals []condVal, after func(func())) {
	refresh := func() { e.refreshFromDB(context.Background(), b, vals) }
	if after != nil {
		after(refresh)
		return
	}
	refresh()
}

// refreshFromDB 重读该行并写穿缓存；行不存在时不动缓存（本次更新 affected=0）。
func (e *Engine) refreshFromDB(ctx context.Context, b *binding, vals []condVal) {
	row, err := b.srcFindOne(ctx, vals)
	if err != nil {
		if !errors.Is(err, ErrNoRows) {
			vars.Warning("dbcache %s: 写后重读失败: %v", b.table, err)
		}
		return
	}
	key, err := b.keyFromRow(row)
	if err != nil {
		return
	}
	fields, err := b.encodeFields(row)
	if err != nil {
		return
	}
	if err := e.store.HSetAll(ctx, b.keys.rowPrefix+key, b.keys.tombPrefix+key, fields, b.ttl); err != nil {
		// 库里已是新值、Redis 里还是旧行 hash 且无墓碑；queryOne 的点查命中行 hash
		// 就直接返回、完全不检查墓碑，所以只告警会留下「库新缓存旧」的静默不一致。
		// 升级为 Error 并主动失效整行，让下次读必然回源。
		vars.Error("dbcache %s: 写后刷新缓存失败（库已提交，主动失效行 %s）: %v", b.table, key, err)
		e.invalidateCached(ctx, b, vals)
	}
}

// invalidateCached 主动失效一行的缓存：立墓碑 + 删行 hash + 销脏账/重试账。
//
// 写侧刷新 / 删除清理失败时必须调用。kvStore.DelRow 对应的 deleteScript 在一个
// Lua 脚本里一次完成上述四件事：SET 墓碑、ZREM 脏账、HDEL 重试计数、DEL 行 hash；
// 生产与内存 fake 实现语义一致。墓碑只防在途脏账回写，真正让点查回源的是删行 hash。
func (e *Engine) invalidateCached(ctx context.Context, b *binding, vals []condVal) {
	key, err := keyFromCond(b.meta, vals)
	if err != nil {
		vars.Error("dbcache %s: 主动失效缓存无法推导行键: %v", b.table, err)
		return
	}
	if _, err := e.store.DelRow(ctx, b.keys.dirty, b.keys.rowPrefix+key, b.keys.retry, b.keys.tombPrefix+key, key, e.tombTTL()); err != nil {
		vars.Error("dbcache %s: 主动失效缓存行 %s 失败（库已提交）: %v", b.table, key, err)
	}
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

// applyCond 把等值/IN 条件挂到 gorm 会话。
func applyCond(gx *gorm.DB, vals []condVal) *gorm.DB {
	for _, v := range vals {
		col := "`" + v.Column + "`"
		switch len(v.Values) {
		case 0:
		case 1:
			gx = gx.Where(col+" = ?", v.Values[0])
		default:
			gx = gx.Where(col+" IN ?", v.Values)
		}
	}
	return gx
}

// applyExpect 把 CAS 期望值挂到 gorm 会话：nil → IS NULL，切片 → IN，其余 → =。
func applyExpect(gx *gorm.DB, col string, v any) *gorm.DB {
	quoted := "`" + col + "`"
	if v == nil {
		return gx.Where(quoted + " IS NULL")
	}
	if reflect.ValueOf(v).Kind() == reflect.Slice {
		return gx.Where(quoted+" IN ?", v)
	}
	return gx.Where(quoted+" = ?", v)
}
