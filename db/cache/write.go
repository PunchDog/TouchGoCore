package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"touchgocore/vars"
)

// ==================== 写线（Redis 优先 + write-behind 落库） ====================
//
// Write：先同步写 Redis——成功即对调用方返回，随后条目进分片写缓冲，
// 即「通知下层数据库准备更新」；定时器（FlushInterval）批量落库。
// Remove：同步 DEL + 缓冲 tombstone（定时器落库删除）。
// 落库失败的条目回插重试，超过 MaxRetry 丢弃并 Del 缓存——
// 绝不让 Redis 长期展示一个从未落库的值。

type opEntry[K comparable, V any] struct {
	ks  string // KeyOf(key)，回插/日志/Redis 操作用
	key K
	val *V
	op  OpKind
	seq int64
	ts  time.Time // 入缓冲时间（老化告警/新鲜度对齐用）
	try int
}

type bufShard[K comparable, V any] struct {
	mu sync.Mutex
	m  map[string]*opEntry[K, V]
}

type buffer[K comparable, V any] struct {
	shards []*bufShard[K, V]
	mask   int
	signal chan struct{}
}

func newBuffer[K comparable, V any](shards int) *buffer[K, V] {
	b := &buffer[K, V]{
		shards: make([]*bufShard[K, V], 0, shards),
		mask:   shards - 1,
		signal: make(chan struct{}, 1),
	}
	for range shards {
		b.shards = append(b.shards, &bufShard[K, V]{m: make(map[string]*opEntry[K, V])})
	}
	return b
}

func (b *buffer[K, V]) shard(ks string) *bufShard[K, V] {
	return b.shards[fnv64(ks)&uint64(b.mask)]
}

// put 覆盖式入缓冲（同 key 天然去重取最新）。返回是否新增（dirty 计数用）。
func (b *buffer[K, V]) put(ks string, e *opEntry[K, V]) (added bool) {
	sh := b.shard(ks)
	sh.mu.Lock()
	_, exists := sh.m[ks]
	sh.m[ks] = e
	sh.mu.Unlock()
	return !exists
}

func (b *buffer[K, V]) has(ks string) bool {
	sh := b.shard(ks)
	sh.mu.Lock()
	_, ok := sh.m[ks]
	sh.mu.Unlock()
	return ok
}

// get 读缓冲条目（reload 脏 key 回填 Redis 用）；调用方勿修改返回指针。
func (b *buffer[K, V]) get(ks string) *opEntry[K, V] {
	sh := b.shard(ks)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.m[ks]
}

// hasNewer 缓冲里是否已有同 key 且 seq 更新的条目（drain 后并发 Write 场景）。
func (b *buffer[K, V]) hasNewer(ks string, seq int64) bool {
	sh := b.shard(ks)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	cur, ok := sh.m[ks]
	return ok && cur.seq > seq
}

// dropIfOlder WriteSync 落库成功后踢掉同 key 的旧缓冲条目，
// 避免后续 flush 用过期缓冲值盖掉 WriteSync 已写入的 DB 行。
// 仅当缓冲 seq < 本次 WriteSync seq 时删除；更新的并发 Write 保留。
func (b *buffer[K, V]) dropIfOlder(ks string, seq int64) bool {
	sh := b.shard(ks)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	cur, ok := sh.m[ks]
	if !ok || cur.seq >= seq {
		return false
	}
	delete(sh.m, ks)
	return true
}

// drain 换出整个分片的条目（锁外干活）
func (b *buffer[K, V]) drain() []*opEntry[K, V] {
	var out []*opEntry[K, V]
	for _, sh := range b.shards {
		sh.mu.Lock()
		if len(sh.m) > 0 {
			for _, e := range sh.m {
				out = append(out, e)
			}
			sh.m = make(map[string]*opEntry[K, V])
		}
		sh.mu.Unlock()
	}
	return out
}

// reinsert 落库失败回插。若同 key 已有更高 seq（drain 后并发 Write），
// 保留新值并返回丢弃条数——调用方 Dirty.Add(-n)，因新 Write 已自计 dirty。
func (b *buffer[K, V]) reinsert(entries []*opEntry[K, V]) (discarded int) {
	for _, e := range entries {
		sh := b.shard(e.ks)
		sh.mu.Lock()
		cur, exists := sh.m[e.ks]
		if exists && cur.seq > e.seq {
			discarded++
			sh.mu.Unlock()
			continue
		}
		sh.m[e.ks] = e
		sh.mu.Unlock()
	}
	return discarded
}

func (b *buffer[K, V]) notifySignal() {
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

// --- Cache 对外写接口 ---

// Write 写线主入口：同步写 Redis，成功后进写缓冲等定时器落库。
// Redis 写失败 → 返回错误且不入缓冲（不制造「DB 有 Redis 无」的静默脏态）。
func (c *Cache[K, V]) Write(ctx context.Context, key K, val *V) error {
	if val == nil {
		return errors.New("cache: Write 值不能为 nil（删除请用 Remove）")
	}
	if c.sn == nil {
		return errNoSaver
	}
	ks := c.KeyOf(key)
	seq := c.seq.Add(1)

	if c.cfg.Enabled {
		wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
		var err error
		if c.jr != nil {
			err = c.jr.JAddUpsert(wctx, ks, c.mustEncode(val, seq), c.jitteredTTL(ks), c.jz, c.keyFn(key), seq)
		} else {
			err = c.setEnvelope(wctx, ks, val, c.jitteredTTL(ks), seq, false)
		}
		cancel()
		if err != nil {
			c.st.KVErr.Add(1)
			vars.Error("cache[%s] Write 同步写Redis失败 key=%s: %v", c.name, ks, err)
			return err
		}
	} else {
		// 降级：write-through 直连落库，不丢数据
		sctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout*10)
		defer cancel()
		return c.sn.Save(sctx, key, val)
	}

	e := &opEntry[K, V]{key: key, val: val, op: OpUpsert, seq: seq, ts: c.clock()}
	e.ks = ks
	if c.buf.put(ks, e) {
		c.st.Dirty.Add(1)
	}
	c.enforceCapacity(ctx)
	return nil
}

// WriteBatch 批量入写缓冲（逐条同步写 Redis；单条失败即返回，已成功条目保留在缓冲）。
func (c *Cache[K, V]) WriteBatch(ctx context.Context, keys []K, vals []*V) error {
	if len(keys) != len(vals) {
		return errors.New("cache: WriteBatch keys/vals 长度不一致")
	}
	for i, k := range keys {
		if err := c.Write(ctx, k, vals[i]); err != nil {
			return err
		}
	}
	return nil
}

// WriteSync 强一致写：同步写 Redis + 同步落库，绕过写缓冲与账本。
// 资金等「绝不允许只活在 Redis 里」的路径用；任一环节失败即报错，
// 且落库失败会把刚写的缓存失效掉——不留「Redis 有、库里没有」的展示态。
// 与普通 Write 的取舍：多一次 DB RTT 的延迟换零丢失窗口。
// 成功后踢掉同 key 更旧的缓冲条目并销账——否则后续 flush 会把旧缓冲值盖回 DB。
func (c *Cache[K, V]) WriteSync(ctx context.Context, key K, val *V) error {
	if val == nil {
		return errors.New("cache: WriteSync 值不能为 nil（删除请用 Remove）")
	}
	if c.sn == nil {
		return errNoSaver
	}
	ks := c.KeyOf(key)
	var seq int64
	if c.cfg.Enabled {
		seq = c.seq.Add(1)
		wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
		err := c.setEnvelope(wctx, ks, val, c.jitteredTTL(ks), seq, false)
		cancel()
		if err != nil {
			c.st.KVErr.Add(1)
			return err
		}
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout*10)
	defer cancel()
	if err := c.sn.Save(sctx, key, val); err != nil {
		// 落库失败：Del 刚写的 Redis；不复活此前已踢掉的旧缓冲（此处尚未踢）
		if c.cfg.Enabled {
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.WriteTimeout)
			_ = c.kv.Del(dctx, ks)
			dcancel()
		}
		vars.Error("cache[%s] WriteSync 落库失败 key=%s: %v", c.name, ks, err)
		return err
	}
	if c.cfg.Enabled && c.buf != nil && seq > 0 {
		if c.buf.dropIfOlder(ks, seq) {
			c.st.Dirty.Add(-1)
		}
		// DB 已有新值：无条件销掉该键账本（WriteSync 本身不记账，清的是先前 Write 的账）
		c.jClearKey(ctx, key)
	}
	return nil
}

// Remove 删除：同步 DEL Redis + 缓冲 tombstone，等定时器落库删除。
// Enabled=false 时与 Write 对称：同步走 Saver.Delete，不留永不 flush 的 tombstone。
func (c *Cache[K, V]) Remove(ctx context.Context, key K) error {
	if c.sn == nil {
		return errNoSaver
	}
	if !c.cfg.Enabled {
		sctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout*10)
		defer cancel()
		return c.sn.Delete(sctx, key)
	}
	ks := c.KeyOf(key)
	seq := c.seq.Add(1)
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	var err error
	if c.jr != nil {
		err = c.jr.JAddDelete(wctx, ks, c.jz, c.keyFn(key), seq)
	} else {
		err = c.kv.Del(wctx, ks)
	}
	cancel()
	if err != nil {
		c.st.KVErr.Add(1)
		return err
	}
	e := &opEntry[K, V]{key: key, op: OpDelete, seq: seq, ts: c.clock()}
	e.ks = ks
	if c.buf.put(ks, e) {
		c.st.Dirty.Add(1)
	}
	c.enforceCapacity(ctx)
	return nil
}

// DeleteCache 只失效 Redis，不通知数据库。
func (c *Cache[K, V]) DeleteCache(ctx context.Context, key K) error {
	wctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	if err := c.kv.Del(wctx, c.KeyOf(key)); err != nil {
		c.st.KVErr.Add(1)
		return err
	}
	return nil
}

// enforceCapacity 缓冲超限：signal 催 flush；block 模式内联落一批形成背压。
func (c *Cache[K, V]) enforceCapacity(ctx context.Context) {
	if int(c.st.Dirty.Load()) <= c.cfg.MaxDirtyKeys {
		return
	}
	c.buf.notifySignal()
	if c.cfg.Overflow == OverflowBlock {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.ReadTimeout*20)
		defer cancel()
		_ = c.flushOnce(fctx, false)
		return
	}
	vars.Warning("cache[%s] 写缓冲超限(%d>%d)，drop 模式不阻塞写入", c.name, c.st.Dirty.Load(), c.cfg.MaxDirtyKeys)
}

// --- 定时器与落库 ---

// Run 驱动该 Cache 的写线定时器（阻塞直到 ctx 取消并完成 final flush）。
// 通常由 Layer/App 服务起协程调用；无写线能力（未配 Saver）时立即返回。
// 启动开头先扫脏账本恢复上次崩溃的未落库数据（方案：journal.go）。
func (c *Cache[K, V]) Run(ctx context.Context) error {
	if c.buf == nil || !c.cfg.Enabled {
		return nil
	}
	c.recoverJournal(ctx)
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// final flush 用独立 ctx：app.ctx 已取消，但残余脏数据必须落库
			fctx, cancel := context.WithTimeout(context.Background(), c.cfg.ReadTimeout*60)
			err := c.flushOnce(fctx, true)
			cancel()
			return err
		case <-ticker.C:
			_ = c.flushOnce(ctx, false)
		case <-c.buf.signal:
			_ = c.flushOnce(ctx, false)
		}
	}
}

// Flush 立即把写缓冲落库（同步）。
func (c *Cache[K, V]) Flush(ctx context.Context) error {
	return c.flushOnce(ctx, false)
}

// Close 停止后的收尾：把残余脏数据全部落库。等待预热协程结束。
func (c *Cache[K, V]) Close(ctx context.Context) error {
	c.runWg.Wait()
	if c.buf == nil {
		return nil
	}
	return c.flushOnce(ctx, true)
}

// flushOnce drain 全缓冲 → 分批落库 → 成功清脏、失败回插/超限丢弃。
// flushMu 串行化：定时器、信号、内联背压、Close 共用一条落库线（天然保序）。
// final=true（停服）：落库仍失败时保留账本与 Redis，返回错误——下次启动走 recoverJournal，
// 与宕机恢复同一路径；不可清账（清账留 Redis 会使值不可恢复）。
func (c *Cache[K, V]) flushOnce(ctx context.Context, final bool) error {
	if c.buf == nil {
		return nil
	}
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	entries := c.buf.drain()
	if len(entries) == 0 {
		return nil
	}
	now := c.clock()
	var retry []*opEntry[K, V]

	// drain 后并发 Write 可能已放入更高 seq：跳过 Save/销账，Dirty 减 1（新 Write 已自计）
	entries = c.filterSuperseded(entries)

	// 老化告警：长时间未落库
	for _, e := range entries {
		if c.cfg.MaxDirtyAge > 0 && now.Sub(e.ts) > c.cfg.MaxDirtyAge {
			vars.Error("cache[%s] 脏数据滞留超龄 key=%s age=%s（落库持续失败？）", c.name, e.ks, now.Sub(e.ts))
		}
	}

	batches := splitBatches(entries, c.cfg.BatchSize)
	for _, batch := range batches {
		ok, failed := c.saveBatch(ctx, batch)
		c.st.Dirty.Add(-int64(len(ok)))
		for _, e := range ok {
			c.renewIfAged(ctx, e)
		}
		c.jClearEntries(ctx, ok)
		for _, e := range failed {
			e.try++
			if !final && e.try > c.cfg.MaxRetry {
				c.st.Dropped.Add(1)
				c.st.Dirty.Add(-1)
				dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.WriteTimeout)
				_ = c.kv.Del(dctx, e.ks)
				dcancel()
				// 正常 flush「放弃」：Del Redis + 销账（与停服失败保留相对）
				c.jClearEntries(ctx, []*opEntry[K, V]{e})
				vars.Error("cache[%s] 脏数据超过最大重试(%d)丢弃 key=%s seq=%d", c.name, c.cfg.MaxRetry, e.ks, e.seq)
			} else {
				retry = append(retry, e)
			}
		}
	}
	if len(retry) > 0 {
		if final {
			// 停服再试一轮；仍失败则回插并报错，账本/Redis 不动
			retry = c.filterSuperseded(retry)
			ok2, stillFailed := c.saveBatch(ctx, retry)
			c.st.Dirty.Add(-int64(len(ok2)))
			for _, e := range ok2 {
				c.renewIfAged(ctx, e)
			}
			c.jClearEntries(ctx, ok2)
			if len(stillFailed) > 0 {
				n := c.buf.reinsert(stillFailed)
				if n > 0 {
					c.st.Dirty.Add(-int64(n))
				}
				kept := len(stillFailed) - n
				for _, e := range stillFailed {
					vars.Error("cache[%s] final flush 落库失败，保留账本与 Redis key=%s seq=%d", c.name, e.ks, e.seq)
				}
				if kept > 0 {
					return fmt.Errorf("cache[%s]: final flush 仍有 %d 条未落库", c.name, kept)
				}
			}
		} else {
			n := c.buf.reinsert(retry)
			if n > 0 {
				c.st.Dirty.Add(-int64(n))
			}
		}
	}
	return nil
}

// filterSuperseded 丢掉已被缓冲内更新 Write 覆盖的 drain 条目。
func (c *Cache[K, V]) filterSuperseded(entries []*opEntry[K, V]) []*opEntry[K, V] {
	if len(entries) == 0 {
		return entries
	}
	out := entries[:0]
	for _, e := range entries {
		if c.buf.hasNewer(e.ks, e.seq) {
			c.st.Dirty.Add(-1)
			continue
		}
		out = append(out, e)
	}
	return out
}

// jClearEntries 落库成功/丢弃后销账。账本 member 无 seq，必须按 score==本条 seq
// 条件删除——否则旧 flush 会 ZREM 掉更新 Write 刚记上的账。
func (c *Cache[K, V]) jClearEntries(ctx context.Context, entries []*opEntry[K, V]) {
	if c.jr == nil || len(entries) == 0 {
		return
	}
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.WriteTimeout)
	defer cancel()
	for _, e := range entries {
		if c.buf != nil && c.buf.hasNewer(e.ks, e.seq) {
			continue
		}
		member := jMember(e.op, c.keyFn(e.key))
		if err := c.jr.JClearIfSeq(jctx, c.jz, member, e.seq); err != nil {
			c.st.JournalErr.Add(1)
			vars.Warning("cache[%s] 条件销账失败 key=%s seq=%d: %v", c.name, e.ks, e.seq, err)
		}
	}
}

// jClearKey WriteSync 成功后无条件清掉该键 upsert/tombstone 账（DB 已追上）。
func (c *Cache[K, V]) jClearKey(ctx context.Context, key K) {
	if c.jr == nil {
		return
	}
	ks := c.keyFn(key)
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.WriteTimeout)
	defer cancel()
	if err := c.jr.JClear(jctx, c.jz, jMember(OpUpsert, ks), jMember(OpDelete, ks)); err != nil {
		c.st.JournalErr.Add(1)
		vars.Warning("cache[%s] WriteSync 销账失败 key=%s: %v", c.name, ks, err)
	}
}

// saveBatch 落库一批：优先 BatchSaver 一条 SQL；否则 SaveConcurrency 并发单条。
// 返回成功集合与失败集合（不回滚部分成功）。
// Save 前再查一次缓冲：若已有更高 seq，跳过（不算失败，也不销账）。
func (c *Cache[K, V]) saveBatch(ctx context.Context, entries []*opEntry[K, V]) (ok, failed []*opEntry[K, V]) {
	if len(entries) == 0 {
		return nil, nil
	}
	// 入口再滤一次：batch 组装到 Save 之间可能又有更新 Write
	live := make([]*opEntry[K, V], 0, len(entries))
	for _, e := range entries {
		if c.buf.hasNewer(e.ks, e.seq) {
			c.st.Dirty.Add(-1)
			continue
		}
		live = append(live, e)
	}
	entries = live
	if len(entries) == 0 {
		return nil, nil
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout*20)
	defer cancel()

	if bs, isBatch := any(c.sn).(BatchSaver[K, V]); isBatch {
		items := make([]Item[K], 0, len(entries))
		for _, e := range entries {
			items = append(items, Item[K]{Key: e.key, Op: e.op, Value: e.val})
		}
		if err := bs.SaveBatch(sctx, items); err != nil {
			c.st.FlushErr.Add(1)
			vars.Warning("cache[%s] 批量落库失败(%d条): %v", c.name, len(entries), err)
			return nil, entries
		}
		c.st.FlushOK.Add(1)
		return entries, nil
	}

	// 单条并发
	var mu sync.Mutex
	sem := make(chan struct{}, c.cfg.SaveConcurrency)
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e *opEntry[K, V]) {
			defer wg.Done()
			if c.buf.hasNewer(e.ks, e.seq) {
				mu.Lock()
				c.st.Dirty.Add(-1)
				mu.Unlock()
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			var err error
			if e.op == OpDelete {
				err = c.sn.Delete(sctx, e.key)
			} else {
				err = c.sn.Save(sctx, e.key, e.val)
			}
			mu.Lock()
			if err != nil {
				vars.Warning("cache[%s] 落库失败 key=%s: %v", c.name, e.ks, err)
				failed = append(failed, e)
			} else {
				ok = append(ok, e)
			}
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	if len(ok) > 0 {
		c.st.FlushOK.Add(1)
	}
	return ok, failed
}

// renewIfAged 落库成功后，若该条目在 Redis 里已活过逻辑新鲜期
// （长重试/超龄场景），重写一次对齐新鲜度；正常情况下 Write 已同步写过，不动。
func (c *Cache[K, V]) renewIfAged(ctx context.Context, e *opEntry[K, V]) {
	if !c.cfg.Enabled || e.op != OpUpsert {
		return
	}
	if c.clock().Sub(e.ts) < c.cfg.logicalTTL() {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	if err := c.setEnvelope(rctx, e.ks, e.val, c.jitteredTTL(e.ks), e.seq, false); err != nil {
		c.st.KVErr.Add(1)
	}
}

func splitBatches[K comparable, V any](entries []*opEntry[K, V], size int) [][]*opEntry[K, V] {
	if size <= 0 {
		size = 200
	}
	var out [][]*opEntry[K, V]
	for len(entries) > size {
		out = append(out, entries[:size])
		entries = entries[size:]
	}
	if len(entries) > 0 {
		out = append(out, entries)
	}
	return out
}
