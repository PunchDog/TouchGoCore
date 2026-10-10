package cache

import (
	"context"
	"errors"
	"sync"
	"time"

	"touchgocore/vars"
)

// ==================== 读线 ====================
// Redis 优先原则：client 拿到的值必须是「经过 Redis」的值——
// miss 时受控协程回源数据库 → 先写 Redis → 再从 Redis 读回返回，
// 不把数据库原始值直接透传给 client（RequireRedis=false 时才降级直返）。

// Get 只查 Redis，不回源。miss → ErrMiss。
func (c *Cache[K, V]) Get(ctx context.Context, key K) (*V, error) {
	ks := c.KeyOf(key)
	raw, err := c.kv.Get(ctx, ks)
	if errors.Is(err, ErrNoEntry) {
		c.st.Miss.Add(1)
		return nil, ErrMiss
	}
	if err != nil {
		c.st.KVErr.Add(1)
		return nil, err
	}
	e, derr := decode[V](c.codec, raw)
	if derr != nil {
		c.st.Miss.Add(1)
		return nil, ErrMiss // 坏值当 miss，交给 GetOrLoad 覆盖自愈
	}
	c.st.Hits.Add(1)
	if e.Null {
		return nil, ErrNotFound
	}
	return &e.Value, nil
}

// GetOrLoad 读线主入口：Redis → miss/逻辑过期时按策略回源。
// 需要 WithLoader 配置过 Loader，否则 miss 直接报错。
func (c *Cache[K, V]) GetOrLoad(ctx context.Context, key K) (*V, error) {
	return c.GetOrLoadWith(ctx, key, c.ld)
}

// GetOrLoadWith 同 GetOrLoad，但本次用显式传入的 Loader（nil 则报错）。
func (c *Cache[K, V]) GetOrLoadWith(ctx context.Context, key K, ld Loader[K, V]) (*V, error) {
	// Enabled=false：必须走 loader，不得返回 Redis 命中（缓存可能是陈旧展示值）
	if !c.cfg.Enabled {
		if ld == nil {
			return nil, errNoLoader
		}
		return ld.Load(ctx, key)
	}
	if ld == nil {
		return nil, errNoLoader
	}
	ks := c.KeyOf(key)

	// 1) 查 Redis
	raw, getErr := c.kv.Get(ctx, ks)
	switch {
	case getErr == nil:
		e, derr := decode[V](c.codec, raw)
		if derr == nil {
			c.st.Hits.Add(1)
			if e.Null {
				return nil, ErrNotFound
			}
			if !e.stale(c.clock().UnixMilli()) {
				v := e.Value
				return &v, nil
			}
			// 命中但逻辑过期：async 直接回旧值 + 协程预热；wait 落入下方同步链路
			if c.cfg.Stale == StaleAsync {
				c.prefetch(ctx, key, ks, ld)
				v := e.Value
				return &v, nil
			}
			c.st.Miss.Add(1)
		} else {
			c.st.Miss.Add(1) // decode 失败当 miss，回源后覆盖坏值
		}
	case errors.Is(getErr, ErrNoEntry):
		c.st.Miss.Add(1)
	default:
		c.st.KVErr.Add(1)
		c.st.Miss.Add(1) // Redis 读错误当 miss；RequireRedis 时后续写回/读回会如实报错
	}

	// 2) 失败退避：回源正在挂 → 不重复打 DB
	if fa, ok := c.fails.Load(ks); ok && c.clock().Sub(fa.(time.Time)) < c.cfg.FailBackoff {
		return nil, ErrSourceUnavailable
	}

	// 3) 单飞同步链路：回源(受控协程) → 写 Redis → 读回返回
	v, err, _ := c.g.Do(ks, func() (any, error) {
		return c.reloadGuarded(ks, key, ld)
	})
	if err != nil {
		return nil, err
	}
	out, _ := v.(*V)
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// reload 是 singleflight 的 leader 执行体（本函数所在协程即 leader）：
// 回源 → 写 Redis（脏 key 跳过，写线优先）→ 从 Redis 读回。
// 返回 *V；确认不存在返回 (nil, ErrNotFound)。
func (c *Cache[K, V]) reload(ks string, key K, ld Loader[K, V]) (any, error) {
	// 后台回填刻意脱离调用方的取消语义：reload 是 singleflight 的 leader 执行体，
	// 其结果被所有并发 waiter 共享——若沿用触发本次回源的那个请求 ctx，请求一旦
	// 被取消（客户端断开/上游超时）就会连带中断回填，让同批 waiter 一起拿不到值。
	// 因此这里直接用 context.Background() 起一个不受调用方取消影响的根 ctx。
	// 它并非无界：下面每一步都各自套 cfg 超时——回源与读回受 cfg.ReadTimeout 约束、
	// 写 Redis 受 cfg.WriteTimeout 约束，超时即释放，不会泄漏成永久挂起的协程。
	bg := context.Background()
	lctx, cancel := context.WithTimeout(bg, c.cfg.ReadTimeout)
	defer cancel()

	val, err := ld.Load(lctx, key)
	switch {
	case err == nil:
		c.fails.Delete(ks)
	case errors.Is(err, ErrNotFound):
		c.fails.Delete(ks)
	default:
		c.fails.Store(ks, c.clock())
		c.st.LoadErr.Add(1)
		vars.Error("cache[%s] 回源失败 key=%s: %v", c.name, ks, err)
		return nil, err
	}
	c.st.Loads.Add(1)
	seq := c.seq.Add(1)

	sctx, scancel := context.WithTimeout(bg, c.cfg.WriteTimeout)
	defer scancel()

	// 脏 key（写缓冲在途、Redis 里应是 Write 同步写的新值）→ 绝不 Set 源值
	dirty := c.buf != nil && c.buf.has(ks)
	if !dirty {
		if val == nil && c.cfg.NegativeTTL <= 0 {
			// 空值缓存关闭：不写 null marker
			return nil, ErrNotFound
		}
		ttl := c.jitteredTTL(ks)
		if val == nil {
			ttl = c.jitteredNegativeTTL(ks)
		}
		// 回填必须走 seq-guarded CAS：脏检查与写入之间若并发 Write 落了更高 seq
		// 的新值，裸 Set 会用源库旧值把它盖掉（违 Redis 优先）。CAS 未生效
		// （written=false）说明 Redis 已有更新的值，静默放弃本次回填即可——
		// 下方读回路径拿到的正是那个新值。
		if _, err := c.setEnvelopeSeq(sctx, ks, val, ttl, seq, val == nil); err != nil {
			c.st.KVErr.Add(1)
			if c.cfg.RequireRedis {
				vars.Error("cache[%s] 回填Redis失败 key=%s: %v", c.name, ks, err)
				return nil, err
			}
		}
	}

	// Redis 优先：client 拿到的值从 Redis 读回
	if c.cfg.RequireRedis {
		gctx, gcancel := context.WithTimeout(bg, c.cfg.ReadTimeout)
		defer gcancel()
		raw, gerr := c.kv.Get(gctx, ks)
		if gerr != nil {
			if errors.Is(gerr, ErrNoEntry) && dirty {
				// Redis TTL 恰好过期：把缓冲里的写线值重新写回，禁止用源行盖掉
				be := c.buf.get(ks)
				if be == nil {
					return nil, errors.New("cache: 脏 key Redis 已过期且缓冲条目不可读")
				}
				if be.op == OpDelete {
					return nil, ErrNotFound
				}
				if be.val == nil {
					return nil, errors.New("cache: 脏 key Redis 已过期且缓冲无有效值")
				}
				if err := c.kv.Set(gctx, ks, c.mustEncode(be.val, be.seq), c.jitteredTTL(ks)); err != nil {
					c.st.KVErr.Add(1)
					return nil, err
				}
				raw, gerr = c.kv.Get(gctx, ks)
			}
			if gerr != nil {
				c.st.KVErr.Add(1)
				return nil, gerr
			}
		}
		e, derr := decode[V](c.codec, raw)
		if derr != nil {
			return nil, derr
		}
		if e.Null {
			return nil, ErrNotFound
		}
		return &e.Value, nil
	}
	// 降级：直返源值
	if val == nil {
		return nil, ErrNotFound
	}
	return val, nil
}

// prefetch 逻辑过期的异步预热：不阻塞本次返回，后台走同一条 reload。
func (c *Cache[K, V]) prefetch(ctx context.Context, key K, ks string, ld Loader[K, V]) {
	if ld == nil || !c.cfg.Enabled {
		return
	}
	c.st.Prefetch.Add(1)
	c.runWg.Add(1)
	go func() {
		defer c.runWg.Done()
		_, err, _ := c.g.Do(ks, func() (any, error) { return c.reloadGuarded(ks, key, ld) })
		if err != nil && !errors.Is(err, ErrNotFound) {
			vars.Warning("cache[%s] 异步预热失败 key=%s: %v", c.name, ks, err)
		}
	}()
}

// MGet 批量只读 Redis，不回源。返回：命中 map（KeyOf→值）、miss 键列表。
// 空标记（Null）按 miss 处理——源确认不存在的键不会出现在命中结果里。
func (c *Cache[K, V]) MGet(ctx context.Context, keys []K) (map[string]*V, []K, error) {
	ksList := make([]string, len(keys))
	for i, k := range keys {
		ksList[i] = c.KeyOf(k)
	}
	raws, err := c.kv.MGet(ctx, ksList)
	if err != nil {
		c.st.KVErr.Add(1)
		return nil, keys, err
	}
	hits := make(map[string]*V, len(keys))
	var misses []K
	for i, raw := range raws {
		if raw == "" {
			misses = append(misses, keys[i])
			continue
		}
		e, derr := decode[V](c.codec, raw)
		if derr != nil || e.Null {
			misses = append(misses, keys[i])
			continue
		}
		v := e.Value
		hits[ksList[i]] = &v
	}
	c.st.Hits.Add(int64(len(hits)))
	c.st.Miss.Add(int64(len(misses)))
	return hits, misses, nil
}

// MGetOrLoad 批量读：Redis 批量 → miss 集合优先 BatchLoader 一次回源
// （逐个写 Redis）→ 再批量从 Redis 读回；无 BatchLoader 则并发单键 GetOrLoad。
func (c *Cache[K, V]) MGetOrLoad(ctx context.Context, keys []K) (map[string]*V, error) {
	if c.ld == nil {
		hits, _, err := c.MGet(ctx, keys)
		return hits, err
	}
	// Enabled=false：全部走源，不返回仅 Redis 命中（与 GetOrLoadWith 一致）
	if !c.cfg.Enabled {
		out := make(map[string]*V, len(keys))
		for _, k := range keys {
			v, err := c.ld.Load(ctx, k)
			switch {
			case err == nil:
				out[c.KeyOf(k)] = v
			case errors.Is(err, ErrNotFound):
				// 不进结果
			default:
				return nil, err
			}
		}
		return out, nil
	}
	hits, misses, err := c.MGet(ctx, keys)
	if err != nil {
		return nil, err
	}
	if len(misses) == 0 {
		return hits, nil
	}
	if bl, ok := c.ld.(BatchLoader[K, V]); ok {
		lctx, cancel := context.WithTimeout(ctx, c.cfg.ReadTimeout)
		defer cancel()
		got, lerr := bl.LoadBatch(lctx, misses)
		if lerr != nil {
			// 批量回源整体失败 = 源不可用，本次所有 miss 键都在同一次失败里：
			// 必须逐个登记退避。只记 misses[0] 会让其余键在退避窗口内
			// 继续打一个已经挂掉的 DB（单键路径查的就是这张表）。
			now := c.clock()
			for _, k := range misses {
				c.fails.Store(c.KeyOf(k), now)
			}
			c.st.LoadErr.Add(1)
			vars.Error("cache[%s] 批量回源失败（%d 个键进入退避 %s）: %v", c.name, len(misses), c.cfg.FailBackoff, lerr)
			return nil, lerr
		}
		// 成功则清退避：源已恢复，不得让旧失败把后面的请求继续挡在门外。
		// 预计算所有 miss 键的 KeyOf 结果，避免三次循环重复调用（长键含 SHA1 开销）。
		missKeys := make([]string, len(misses))
		for i, k := range misses {
			missKeys[i] = c.KeyOf(k)
		}
		for _, ks := range missKeys {
			c.fails.Delete(ks)
		}
		c.st.Loads.Add(1)
		// 键对齐自检：LoadBatch 的结果 map 必须以 KeyOf(key) 为键。第三方实现若没走
		// KeyBinder（或自己拼键，比如只用 keyFn 的裸键串），回填会一条都对不上，
		// 表现成「每次 MGetOrLoad 都打一次 DB 但一个也用不上、还顺手写了一堆空标记」
		// 的静默退化。这里把它变响亮：只告警不改行为，避免影响正常路径。
		if len(got) > 0 {
			aligned := 0
			for _, ks := range missKeys {
				if _, ok := got[ks]; ok {
					aligned++
				}
			}
			if aligned == 0 {
				vars.Error("cache[%s] 批量回源键不对齐：LoadBatch 返回 %d 条但没有一条以 KeyOf 为键（BatchLoader 须经 KeyBinder 绑定 KeyOf），本次回源结果全部作废", c.name, len(got))
			}
		}
		// 回填：miss 且源也没有的写空标记（NegativeTTL<=0 则跳过）
		for _, ks := range missKeys {
			v := got[ks]
			if c.buf != nil && c.buf.has(ks) {
				continue
			}
			if v == nil && c.cfg.NegativeTTL <= 0 {
				continue
			}
			ttl := c.jitteredTTL(ks)
			if v == nil {
				ttl = c.jitteredNegativeTTL(ks)
			}
			sctx, scancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
			// 与单键回填同理：批量回填也走 seq-guarded CAS，防旧值降级覆盖
			_, err := c.setEnvelopeSeq(sctx, ks, v, ttl, c.seq.Add(1), v == nil)
			scancel()
			if err != nil {
				c.st.KVErr.Add(1)
				vars.Warning("cache[%s] 批量回填失败 key=%s: %v", c.name, ks, err)
			}
		}
		// 读回（Redis 优先），空标记键读回为 miss → ErrNotFound 语义：不进结果 map
		out, _, err := c.MGet(ctx, misses)
		if err != nil {
			return nil, err
		}
		for k, v := range out {
			hits[k] = v
		}
		return hits, nil
	}
	// 退化：并发单键（受 MGetConcurrency 闸）
	var mu sync.Mutex
	sem := make(chan struct{}, c.cfg.MGetConcurrency)
	var wg sync.WaitGroup
	var firstErr error
	for _, k := range misses {
		wg.Add(1)
		go func(k K) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := c.GetOrLoad(ctx, k)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				hits[c.KeyOf(k)] = v
			case errors.Is(err, ErrNotFound), errors.Is(err, ErrMiss):
				// 不存在：不进结果
			case firstErr == nil:
				firstErr = err
			}
		}(k)
	}
	wg.Wait()
	return hits, firstErr
}

var errNoLoader = errors.New("cache: 未配置 Loader，无法回源")

// mustEncode 编码 envelope；val==nil 写空标记
func (c *Cache[K, V]) mustEncode(val *V, seq int64) string {
	e := &envelope[V]{ExpMS: c.clock().UnixMilli() + int64(c.cfg.logicalTTL().Milliseconds()), Seq: seq, Null: val == nil}
	if val != nil {
		e.Value = *val
	}
	raw, _ := encode(c.codec, e)
	return raw
}
