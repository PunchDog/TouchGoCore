package dbcache

import (
	"context"
	"math"
	"time"

	"touchgocore/vars"
)

// flushDue 处理到期脏键；由每表定时器的异步 Tick 调用。
func (e *Engine) flushDue(b *binding) {
	if e.stopped.Load() {
		return
	}
	ctx := e.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if !b.busy.CompareAndSwap(false, true) {
		return
	}
	defer b.busy.Store(false)
	if err := e.flushRange(ctx, b, time.Now().UnixMilli()); err != nil {
		vars.Warning("dbcache %s: flush due: %v", b.table, err)
	}
}

// flushAll 无视到期时间，把本表全部脏键落库。
func (e *Engine) flushAll(ctx context.Context, b *binding) error {
	if !b.busy.CompareAndSwap(false, true) {
		return nil
	}
	defer b.busy.Store(false)
	return e.flushRange(ctx, b, math.MaxInt64)
}

// flushRange 循环领取 <= maxScore 的脏键，解码后批量 upsert。
func (e *Engine) flushRange(ctx context.Context, b *binding, maxScore int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		members, err := e.store.DueMembers(ctx, b.keys.dirty, maxScore, e.cfg.DirtyBatchSize)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		rows := make([]any, 0, len(members))
		claimed := make([]dirtyMember, 0, len(members))
		for _, m := range members {
			h, ok, cerr := e.store.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+m.Member, m.Member, m.Score)
			if cerr != nil {
				return cerr
			}
			if !ok {
				continue
			}
			row, derr := b.decode(h)
			if derr != nil {
				vars.Error("dbcache %s: decode dirty row %s: %v", b.table, m.Member, derr)
				e.markRetry(ctx, b, m.Member)
				continue
			}
			rows = append(rows, row)
			claimed = append(claimed, m)
		}
		if len(claimed) == 0 {
			return nil
		}
		if err := e.upsert(ctx, b, rows); err != nil {
			for _, m := range claimed {
				e.markRetry(ctx, b, m.Member)
			}
			return err
		}
		for _, m := range claimed {
			_ = e.store.HDel(ctx, b.keys.retry, m.Member)
		}
	}
}

// upsert 以落库超时预算调用绑定的批量写入闭包。
func (e *Engine) upsert(ctx context.Context, b *binding, rows []any) error {
	fctx, cancel := context.WithTimeout(ctx, e.cfg.FlushTimeout)
	defer cancel()
	return b.sinkBatch(fctx, rows)
}

// markRetry 记录失败次数并按指数退避重新挂回脏账；超过上限也不丢弃。
func (e *Engine) markRetry(ctx context.Context, b *binding, key string) {
	n, err := e.store.HIncrBy(ctx, b.keys.retry, key)
	if err != nil {
		vars.Error("dbcache %s: mark retry %s: %v", b.table, key, err)
		return
	}
	level := n
	if level > int64(e.cfg.MaxRetry) {
		level = int64(e.cfg.MaxRetry)
	}
	backoff := b.interval
	for i := int64(1); i < level; i++ {
		backoff *= 2
		if backoff >= 5*time.Minute {
			backoff = 5 * time.Minute
			break
		}
	}
	if err := e.store.ZAdd(ctx, b.keys.dirty, key, time.Now().Add(backoff).UnixMilli()); err != nil {
		vars.Error("dbcache %s: requeue dirty %s: %v", b.table, key, err)
	}
	if n > int64(e.cfg.MaxRetry) {
		vars.Error("dbcache %s: key %s flush retry=%d，继续退避重试（不丢弃）", b.table, key, n)
	}
}

// pendingCount 返回本表仍待落库的脏键数（最多探测 1 条）。
func (e *Engine) pendingCount(ctx context.Context, b *binding) int {
	members, err := e.store.DueMembers(ctx, b.keys.dirty, math.MaxInt64, 1)
	if err != nil {
		return 0
	}
	return len(members)
}
