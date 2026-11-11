package dbcache

// ============================================================================
// 异步 sink 重放：dirty（upsert）/ dirtyDel（delete）两本账 + inflight 租约。
//
// Claim 把成员移入 inflight（score=租约到期）而非直接销账；sink 成功才 ZREM。
// Claim 后崩溃 → 租约过期 → RequeueInflight 重挂 dirty，关闭丢更新窗口。
// sink 前复核行版本：Claim 与落库之间若有新写，跳过陈旧值（新脏账会再落库）。
// 失败 markRetry 指数退避重挂本账，超上限也不丢弃。
// ============================================================================

import (
	"context"
	"fmt"
	"math"
	"time"

	"touchgocore/metrics"
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
	// 回收上次崩溃遗留的过期租约（幂等，无过期成员时零副作用）。
	if _, err := e.store.RequeueInflight(ctx, b.keys.inflight, b.keys.dirty, time.Now().UnixMilli()); err != nil {
		vars.Warning("dbcache %s: requeue inflight: %v", b.table, err)
	}
	// sink lag 仪表：两本账的待物化成员总数（探测失败不阻断 flush）。
	if dirty, err := e.store.IndexCard(ctx, b.keys.dirty); err == nil {
		if dels, derr := e.store.IndexCard(ctx, b.keys.dirtyDel); derr == nil {
			metrics.DBCache.SetSinkLag(b.table, float64(dirty+dels))
		}
	}
	if err := e.flushRange(ctx, b, time.Now().UnixMilli()); err != nil {
		vars.Warning("dbcache %s: flush due: %v", b.table, err)
	}
}

// flushAll 无视到期时间，把本表全部脏键落库。显式 flush 必须等定时器让出 busy，
// 否则与后台 flush 撞上会静默 no-op；等待上限为 FlushTimeout，超时返回错误而不是
// 无限挂起（ctx 常是 Background，不能只靠它兜底）。
func (e *Engine) flushAll(ctx context.Context, b *binding) error {
	deadline := time.Now().Add(e.cfg.FlushTimeout)
	for {
		if b.busy.CompareAndSwap(false, true) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dbcache: flush %s busy timeout", b.table)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer b.busy.Store(false)
	return e.flushRange(ctx, b, math.MaxInt64)
}

// flushRange 循环领取 <= maxScore 的脏键落库，再处理异步删除账。
func (e *Engine) flushRange(ctx context.Context, b *binding, maxScore int64) error {
	if b.sinkBatch == nil && b.sinkDelete == nil {
		return nil
	}
	leaseDue := time.Now().Add(2 * e.cfg.FlushTimeout).UnixMilli()
	if b.sinkBatch != nil {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			members, err := e.store.DueMembers(ctx, b.keys.dirty, maxScore, e.cfg.DirtyBatchSize)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				break
			}
			rows := make([]any, 0, len(members))
			claimed := make([]dirtyMember, 0, len(members))
			claimedSecrets := make([]map[string]string, 0, len(members))
			for _, m := range members {
				h, ok, cerr := e.store.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+m.Member, b.keys.tombPrefix+m.Member, b.keys.inflight, m.Member, m.Score, leaseDue)
				if cerr != nil {
					return cerr
				}
				if !ok {
					continue
				}
				// 版本复核：陈旧值跳过，销租约即可（新写的脏账会再落库）。
				cur, gerr := e.store.HGetAll(ctx, b.keys.rowPrefix+m.Member)
				if gerr != nil {
					_ = e.store.ZRem(ctx, b.keys.inflight, m.Member)
					return gerr
				}
				if len(cur) == 0 || cur[versionField] != h[versionField] {
					_ = e.store.ZRem(ctx, b.keys.inflight, m.Member)
					continue
				}
				row, derr := b.decode(h)
				if derr != nil {
					vars.Error("dbcache %s: decode dirty row %s: %v", b.table, m.Member, derr)
					e.markRetry(ctx, b, b.keys.dirty, m.Member)
					continue
				}
				rows = append(rows, row)
				claimed = append(claimed, m)
				// 秘密列不在模型/行 hash 里，单独读 :s: 供 sinkSecrets 落库。
				var sec map[string]string
				if b.sinkSecrets != nil && len(b.secrets) > 0 {
					sec, gerr = e.store.HGetAll(ctx, b.keys.secretPrefix+m.Member)
					if gerr != nil {
						_ = e.store.ZRem(ctx, b.keys.inflight, m.Member)
						return gerr
					}
				}
				claimedSecrets = append(claimedSecrets, sec)
			}
			if len(claimed) == 0 {
				break
			}
			if err := e.upsert(ctx, b, rows); err != nil {
				for _, m := range claimed {
					e.markRetry(ctx, b, b.keys.dirty, m.Member)
				}
				return err
			}
			for i, m := range claimed {
				// 行已 upsert；秘密列随后 UPDATE。失败重挂脏账并退出本轮（与
				// flushDeletes 同口径：flushAll 用 MaxInt64 领取，继续循环会把刚
				// 退避的成员立即再领走死循环）。upsert 幂等，下一轮连秘密一起重试。
				if len(claimedSecrets[i]) > 0 {
					if serr := e.flushSecrets(ctx, b, m.Member, claimedSecrets[i]); serr != nil {
						vars.Warning("dbcache %s: sink secrets %s: %v", b.table, m.Member, serr)
						e.markRetry(ctx, b, b.keys.dirty, m.Member)
						return serr
					}
				}
				_ = e.store.ZRem(ctx, b.keys.inflight, m.Member)
				_ = e.store.HDel(ctx, b.keys.retry, m.Member)
			}
		}
	}
	return e.flushDeletes(ctx, b, maxScore)
}

// flushDeletes 处理 dirtyDel：逐键销账 → 重建守卫（行已重建则跳过）→ sinkDelete。
func (e *Engine) flushDeletes(ctx context.Context, b *binding, maxScore int64) error {
	if b.sinkDelete == nil {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		members, err := e.store.DueMembers(ctx, b.keys.dirtyDel, maxScore, e.cfg.DirtyBatchSize)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return nil
		}
		for _, m := range members {
			if err := e.store.ZRem(ctx, b.keys.dirtyDel, m.Member); err != nil {
				return err
			}
			// 删除后同键已重建（save 清了墓碑）→ 这次删除作废，不能删掉新行。
			cur, gerr := e.store.HGetAll(ctx, b.keys.rowPrefix+m.Member)
			if gerr != nil {
				return gerr
			}
			if len(cur) > 0 {
				continue
			}
			vals, verr := valsFromKey(b.meta, m.Member)
			if verr != nil {
				vars.Error("dbcache %s: dirtyDel key %s 反解失败: %v", b.table, m.Member, verr)
				continue
			}
			if err := e.deleteSink(ctx, b, vals); err != nil {
				vars.Warning("dbcache %s: sink delete %s: %v", b.table, m.Member, err)
				e.markRetry(ctx, b, b.keys.dirtyDel, m.Member)
				// 与 dirty 路径同口径：本轮退出，退避到期后下一轮再试。
				// flushAll 用 MaxInt64 领取，若继续循环会把刚退避的成员立即再领走，死循环。
				return err
			}
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

// deleteSink 以落库超时预算调用绑定的删除闭包。
func (e *Engine) deleteSink(ctx context.Context, b *binding, vals []condVal) error {
	fctx, cancel := context.WithTimeout(ctx, e.cfg.FlushTimeout)
	defer cancel()
	return b.sinkDelete(fctx, vals)
}

// flushSecrets 以落库超时预算把一行的秘密列（:s: hash）按行键 UPDATE 进 sink。
func (e *Engine) flushSecrets(ctx context.Context, b *binding, key string, secrets map[string]string) error {
	vals, err := valsFromKey(b.meta, key)
	if err != nil {
		return err
	}
	secAny := make(map[string]any, len(secrets))
	for k, v := range secrets {
		secAny[k] = v
	}
	fctx, cancel := context.WithTimeout(ctx, e.cfg.FlushTimeout)
	defer cancel()
	return b.sinkSecrets(fctx, vals, secAny)
}

// markRetry 记录失败次数并按指数退避重挂指定账本；超过上限也不丢弃。
// dirty 账失败先销 inflight 租约；dirtyDel 账成员已在领取时销账，直接重挂。
func (e *Engine) markRetry(ctx context.Context, b *binding, ledger, key string) {
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
	if ledger == b.keys.dirty {
		_ = e.store.ZRem(ctx, b.keys.inflight, key)
	}
	if err := e.store.ZAdd(ctx, ledger, key, time.Now().Add(backoff).UnixMilli()); err != nil {
		vars.Error("dbcache %s: requeue %s: %v", b.table, key, err)
	}
	if n > int64(e.cfg.MaxRetry) {
		vars.Error("dbcache %s: key %s flush retry=%d，继续退避重试（不丢弃）", b.table, key, n)
	}
}

// pendingCount 返回本表仍待落库的脏键数（dirty + dirtyDel，最多各探测 1 条）。
func (e *Engine) pendingCount(ctx context.Context, b *binding) int {
	if b.sinkBatch == nil && b.sinkDelete == nil {
		return 0
	}
	members, err := e.store.DueMembers(ctx, b.keys.dirty, math.MaxInt64, 1)
	if err != nil {
		return 0
	}
	dels, err := e.store.DueMembers(ctx, b.keys.dirtyDel, math.MaxInt64, 1)
	if err != nil {
		return len(members)
	}
	return len(members) + len(dels)
}
