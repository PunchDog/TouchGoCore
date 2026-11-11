package dbcache

// ============================================================================
// Save：Redis 权威整行写。
//
// 写成功 = 业务提交成功（Lua 原子维护行/索引/守卫/键集/秘密/脏账）；
// mustSave 只控制 sink 紧迫度（立即 flushKey），不再是 durability 开关。
// Redis 故障 fail-closed 返回错误，没有「退化直写数据库」路径——库只是滞后
// 物化视图，绕过 Redis 直写会制造双真源。
// ============================================================================

import (
	"context"
	"fmt"
	"time"

	"touchgocore/metrics"
	"touchgocore/vars"
)

// Save 全值写入一行：Redis 原子写全字段，再按 mustSave 决定立即落库或延迟落库。
//
// key 为空时按表的 KeyColumns 从 row 推导；mustSave=true 时返回前尝试同步 sink，
// sink 失败保留脏账并返回错误（Redis 值已是真源，重试落库即可）。
func Save[T any](ctx context.Context, table string, key string, row *T, mustSave bool, opt ...Option) error {
	e := defaultEngine.Load()
	if e == nil {
		return ErrNotInitialized
	}
	o := ParseOptions(opt)
	if st := txStateFrom(ctx); st != nil {
		atomic := o.Increment != "" || o.AtomicMax != "" || o.CASSet != nil
		var b *binding
		var err error
		if atomic {
			// 原子写只按表找绑定：调用方不需要构造缓存行类型，只给条件与列。
			b, err = e.lookupTable(table)
		} else {
			b, err = e.lookupTyped(table, typeOf[T]())
		}
		if err != nil {
			return err
		}
		if key == "" && !atomic {
			if row == nil {
				return ErrNilRow
			}
			key, err = b.keyFromRow(any(row))
			if err != nil {
				return err
			}
		}
		var payload any
		if row != nil {
			payload = any(row)
		}
		return st.saveInTx(ctx, b, key, payload, o)
	}
	if o.Sequence != nil {
		return fmt.Errorf("dbcache: 原子取号必须在事务内执行")
	}
	if o.Increment != "" || o.AtomicMax != "" || o.CASSet != nil {
		// 事务外的原子写：Redis Lua 对 live 行校验条件后原子应用（资金/状态列）。
		n, err := e.atomicWrite(ctx, table, o)
		if o.Affected != nil {
			*o.Affected = n
		}
		return err
	}
	return e.save[T](ctx, table, key, row, mustSave, o)
}

func (e *Engine) save[T any](ctx context.Context, table string, key string, row *T, mustSave bool, opts ...*Options) error {
	if row == nil {
		return ErrNilRow
	}
	var o *Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o == nil {
		o = &Options{}
	}
	b, err := e.lookupTyped(table, typeOf[T]())
	if err != nil {
		return err
	}
	rowAny := any(row)
	if key == "" {
		key, err = b.keyFromRow(rowAny)
		if err != nil {
			return err
		}
	}
	fields, err := b.encodeFields(rowAny)
	if err != nil {
		return err
	}
	secret, err := e.encodeSecrets(b, o.SecretSet)
	if err != nil {
		return err
	}
	due := time.Now().Add(b.interval).UnixMilli()
	if mustSave {
		due = time.Now().UnixMilli()
	}
	op := &writeOp{
		Op:      "save",
		Fields:  fieldsToMap(fields),
		Secret:  secret,
		Due:     due,
		TombTTL: e.tombTTL().Milliseconds(),
	}
	if _, err := e.applyWrite(ctx, b, key, op); err != nil {
		return fmt.Errorf("dbcache: save %s: %w", table, err)
	}
	if mustSave {
		return e.flushKey(ctx, b, key)
	}
	return nil
}

// encodeSecrets 校验并编码秘密列：列名须在白名单或模型里，值只进 :s: hash。
func (e *Engine) encodeSecrets(b *binding, secrets map[string]any) (map[string]string, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	for col := range secrets {
		if b.secrets[col] {
			continue
		}
		if err := validateColumn(b, col); err != nil {
			return nil, err
		}
	}
	return encodeFieldMap(secrets)
}

// fieldsToMap 把 encodeRow 的扁平列表转成 map。
func fieldsToMap(fields []string) map[string]string {
	out := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		out[fields[i]] = fields[i+1]
	}
	return out
}

// flushKey 立即落库单个键：Claim 租约 → 版本复核 → sink → 销租约。
// 并发写入刷新了脏账 score 时重试领取最新值；无 sink 时按 SinkRequired 报错或告警。
func (e *Engine) flushKey(ctx context.Context, b *binding, key string) error {
	if b.sinkBatch == nil {
		return e.sinkAbsent(b, key)
	}
	leaseDue := time.Now().Add(2 * e.cfg.FlushTimeout).UnixMilli()
	for attempt := 0; attempt < 3; attempt++ {
		score, ok, err := e.store.ZScore(ctx, b.keys.dirty, key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		h, claimed, err := e.store.Claim(ctx, b.keys.dirty, b.keys.rowPrefix+key, b.keys.tombPrefix+key, b.keys.inflight, key, score, leaseDue)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		// 版本复核：Claim 与 sink 之间若有新写，跳过陈旧值（新脏账会再落库）。
		cur, err := e.store.HGetAll(ctx, b.keys.rowPrefix+key)
		if err != nil {
			_ = e.store.ZRem(ctx, b.keys.inflight, key)
			return err
		}
		if len(cur) == 0 || cur[versionField] != h[versionField] {
			_ = e.store.ZRem(ctx, b.keys.inflight, key)
			continue
		}
		row, err := b.decode(h)
		if err != nil {
			e.markRetry(ctx, b, b.keys.dirty, key)
			return err
		}
		if err := e.upsert(ctx, b, []any{row}); err != nil {
			e.markRetry(ctx, b, b.keys.dirty, key)
			return err
		}
		// 秘密列不在模型/行 hash 里，行 upsert 后按行键 UPDATE 落库（与异步
		// flushRange 同口径）；失败重挂脏账，下一轮连行带秘密一起重试。
		if b.sinkSecrets != nil && len(b.secrets) > 0 {
			sec, gerr := e.store.HGetAll(ctx, b.keys.secretPrefix+key)
			if gerr != nil {
				e.markRetry(ctx, b, b.keys.dirty, key)
				return gerr
			}
			if len(sec) > 0 {
				if serr := e.flushSecrets(ctx, b, key, sec); serr != nil {
					e.markRetry(ctx, b, b.keys.dirty, key)
					return serr
				}
			}
		}
		_ = e.store.ZRem(ctx, b.keys.inflight, key)
		_ = e.store.HDel(ctx, b.keys.retry, key)
		return nil
	}
	return nil
}

// sinkAbsent 处理 mustSave 但无 sink：SinkRequired=true 报错，否则告警放行
// （Redis 权威写已成功，库只是缺席的物化视图）。每表只告警一次。
func (e *Engine) sinkAbsent(b *binding, key string) error {
	if e.cfg.SinkRequired {
		return fmt.Errorf("%w: %s/%s", ErrSinkRequired, b.table, key)
	}
	if _, loaded := e.sinkWarned.LoadOrStore(b.table, true); !loaded {
		vars.Warning("dbcache %s: mustSave 但无 sink（Redis 权威写已成功，物化视图缺席）", b.table)
	}
	metrics.DBCache.IncSinkAbsent(b.table)
	return nil
}
