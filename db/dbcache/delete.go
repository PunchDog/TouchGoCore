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
func Delete(ctx context.Context, table string, cond any) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	b, err := e.lookupTable(table)
	if err != nil {
		return err
	}
	return e.remove(ctx, b, cond)
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
		// 数据库已删除，Redis 清理失败只告警：墓碑随 TTL 过期，读路径回源即修正。
		vars.Warning("dbcache %s: clear cached row %s: %v", b.table, key, err)
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
