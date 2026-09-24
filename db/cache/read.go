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
		return c.reload(ks, key, ld)
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
	bg := context.WithoutCancel(context.Background())
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
		if err := c.kv.Set(sctx, ks, c.mustEncode(val, seq), ttl); err != nil {
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
		_, err, _ := c.g.Do(ks, func() (any, error) { return c.reload(ks, key, ld) })
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
			c.fails.Store(c.KeyOf(misses[0]), c.clock())
			return nil, lerr
		}
		c.st.Loads.Add(1)
		// 回填：miss 且源也没有的写空标记（NegativeTTL<=0 则跳过）
		for _, k := range misses {
			ks := c.KeyOf(k)
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
			err := c.setEnvelope(sctx, ks, v, ttl, c.seq.Add(1), v == nil)
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
