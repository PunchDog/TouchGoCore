package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/db/cache/internal/singleflight"
)

// Cache 是「Redis 一级 + 数据库回源」的两级缓存实例，一个数据类型一个实例。
// 并发安全；读线经 GetOrLoad，写线经 Write（需 WithWriteBehind 或配置 Saver）。
type Cache[K comparable, V any] struct {
	name  string
	kv    KV
	codec Codec
	clock func() time.Time
	cfg   Config

	keyFn func(K) string
	g     singleflight.Group

	ld Loader[K, V]
	sn Saver[K, V]

	jr     Journaler               // KV 支持且配置启用时的脏账本；nil=关
	jz     string                  // 账本 ZSET 键
	keyDec func(string) (K, error) // 恢复重放时 keyStr→K

	sq SeqKV      // KV 支持「按 seq 条件写」时的 CAS 句柄；nil=退回无条件 Set/Del
	js JournalSeq // KV 支持「按 seq 条件记账」时的句柄；nil=退回 JAddUpsert/JAddDelete

	fails sync.Map // string(keyString) → time.Time，回源失败退避（防打穿 DB）
	seq   atomic.Int64
	st    Stats

	buf     *buffer[K, V] // 写缓冲；无落库能力时为 nil
	flushMu sync.Mutex    // 串行化 flushOnce（定时器/信号/内联背压/Close 共用）
	runWg   sync.WaitGroup
}

// Stats 观测计数（原子，快照读取）
type Stats struct {
	Hits     atomic.Int64 // 读线命中
	Miss     atomic.Int64 // 读线 miss（含逻辑过期走同步）
	Loads    atomic.Int64 // 回源次数（singleflight 合并后）
	LoadErr  atomic.Int64 // 回源失败
	KVErr    atomic.Int64 // Redis 操作失败（读/写）
	Prefetch atomic.Int64 // 异步预热触发
	Dropped  atomic.Int64 // 超过 MaxRetry 被丢弃的脏条目
	FlushOK  atomic.Int64
	FlushErr atomic.Int64
	Dirty    atomic.Int64 // 当前写缓冲脏键数

	Recovered   atomic.Int64 // 启动扫账成功重放落库的条目
	RecoverMiss atomic.Int64 // 账本在但 envelope 已过期/坏值/键解不开——不可恢复，已告警
	JournalErr  atomic.Int64 // 账本操作失败（尽力而为路径，不阻断读写）
	Superseded  atomic.Int64 // 并发写被更大 seq 超越而丢弃（Redis CAS 未生效/缓冲保留新值）
}

// StatsSnapshot 计数快照
type StatsSnapshot struct {
	Hits, Miss, Loads, LoadErr, KVErr, Prefetch, Dropped, FlushOK, FlushErr, Dirty int64
	Recovered, RecoverMiss, JournalErr, Superseded                                 int64
}

func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Hits: s.Hits.Load(), Miss: s.Miss.Load(), Loads: s.Loads.Load(),
		LoadErr: s.LoadErr.Load(), KVErr: s.KVErr.Load(), Prefetch: s.Prefetch.Load(),
		Dropped: s.Dropped.Load(), FlushOK: s.FlushOK.Load(), FlushErr: s.FlushErr.Load(),
		Dirty: s.Dirty.Load(), Recovered: s.Recovered.Load(),
		RecoverMiss: s.RecoverMiss.Load(), JournalErr: s.JournalErr.Load(),
		Superseded: s.Superseded.Load(),
	}
}

// Option 构造选项，按给出顺序应用
type Option[K comparable, V any] func(*Cache[K, V])

func WithConfig[K comparable, V any](c Config) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg = c }
}
func WithKeyFunc[K comparable, V any](f func(K) string) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.keyFn = f }
}
func WithPrefix[K comparable, V any](p string) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.KeyPrefix = p }
}
func WithGroup[K comparable, V any](g string) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.Group = g }
}
func WithTTL[K comparable, V any](d time.Duration) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.TTL = d }
}

// WithNegativeTTL 只覆盖空值标记 TTL，不动命名空间与 Enabled —— 与 WithTTL 同一条理由：
// 单个 Cache 想改的是「存多久」，把整份 Config 换掉会连带改写 Layer 给的 group/prefix，
// 于是两个不同 Layer 的注册会互相覆盖键空间（表现成「关了缓存却仍读到 Redis 命中」）。
func WithNegativeTTL[K comparable, V any](d time.Duration) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.NegativeTTL = d }
}
func WithCodec[K comparable, V any](c Codec) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.codec = c }
}
func WithLoader[K comparable, V any](l Loader[K, V]) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.ld = l }
}
func WithSaver[K comparable, V any](s Saver[K, V]) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.sn = s }
}
func WithStalePolicy[K comparable, V any](p StalePolicy) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.Stale = p }
}
func WithRequireRedis[K comparable, V any](b bool) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.RequireRedis = b }
}
func WithEnabled[K comparable, V any](b bool) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.Enabled = b }
}

// WithWriteBehind 启用/调参写线定时器（不配置时默认 2s/200 也生效，只要给了 Saver）
func WithWriteBehind[K comparable, V any](interval time.Duration, batchSize int) Option[K, V] {
	return func(cc *Cache[K, V]) {
		if interval > 0 {
			cc.cfg.FlushInterval = interval
		}
		if batchSize > 0 {
			cc.cfg.BatchSize = batchSize
		}
	}
}

// WithClock 注入时钟（测试用）
func WithClock[K comparable, V any](f func() time.Time) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.clock = f }
}

// WithKeyDecoder 提供 keyStr→K 的解码函数（脏账本启动恢复重放用）。
// 不提供时框架自动支持 string 与整数族键；其它类型必须提供，否则恢复只能告警。
func WithKeyDecoder[K comparable, V any](f func(string) (K, error)) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.keyDec = f }
}

// WithJournal 显式开关脏账本（默认跟随 Config.Journal；KV 不支持时自动关）。
func WithJournal[K comparable, V any](b bool) Option[K, V] {
	return func(cc *Cache[K, V]) { cc.cfg.Journal = b }
}

// New 构造缓存实例。kv 为空返回错误；配置了 Saver 时自动建写缓冲（写线可用，
// 需调用 Run 或由 Layer 驱动定时器）。
func New[K comparable, V any](name string, kv KV, opts ...Option[K, V]) (*Cache[K, V], error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("cache: name 不能为空")
	}
	c := &Cache[K, V]{
		name:  name,
		kv:    kv,
		codec: NewJSONCodec(),
		clock: time.Now,
		cfg:   DefaultConfig(),
	}
	for _, o := range opts {
		o(c)
	}
	if c.kv == nil {
		return nil, errors.New("cache: KV 未配置（Redis 连接缺失？）")
	}
	if err := c.cfg.Validate(); err != nil {
		return nil, err
	}
	if c.codec == nil {
		c.codec = NewJSONCodec()
	}
	if c.keyFn == nil {
		c.keyFn = defaultKeyFunc[K]()
	}
	if c.keyDec == nil {
		c.keyDec = decodeKeyDefault[K]
	}
	if s, ok := c.kv.(SeqKV); ok {
		c.sq = s
	}
	if c.sn != nil {
		c.buf = newBuffer[K, V](c.cfg.Shards)
		if c.cfg.Journal {
			if j, ok := c.kv.(Journaler); ok {
				c.jr = j
				c.jz = c.journalKey()
				if js, ok := c.kv.(JournalSeq); ok {
					c.js = js
				}
			}
		}
	}
	// 批量回源键延迟绑定（late-binding）：BatchLoader 的结果 map 必须以 KeyOf(key)
	// 为键，但它被构造时还拿不到本 Cache 的命名空间/sanitize 规则。这里把 KeyOf
	// 注入进去，杜绝「回填键与查询键不一致 → 批量回源全部落空」的静默退化。
	// 注意：同一个 BatchLoader 实例被多个 Cache 共用时后绑定者胜出，应一 Cache 一实例。
	if kb, ok := c.ld.(KeyBinder); ok {
		kb.BindKeyOf(c.keyOfRaw)
	}
	return c, nil
}

// Name 返回类型名
func (c *Cache[K, V]) Name() string { return c.name }

// Stats 返回计数器（原子字段，可 Snapshot）
func (c *Cache[K, V]) Stats() *Stats { return &c.st }

// keyBase 键的命名空间段：{prefix}:{group}:{name} 或 {prefix}:{name}
func (c *Cache[K, V]) keyBase() string {
	if c.cfg.Group != "" {
		return c.cfg.KeyPrefix + ":" + c.cfg.Group + ":" + c.name
	}
	return c.cfg.KeyPrefix + ":" + c.name
}

// KeyOf 生成 Redis 键：{prefix}:{group}:{name}:{key}
func (c *Cache[K, V]) KeyOf(key K) string {
	return c.keyOfRaw(c.keyFn(key))
}

// keyOfRaw 把「已经由 keyFn 得到的原始键串」套上命名空间与 sanitize。
// 与 KeyOf 是同一条路径，单独拆出来给只有字符串、拿不到 K 的一侧复用
// （批量回源经 KeyBinder 注入的就是它），保证两边键形完全一致。
func (c *Cache[K, V]) keyOfRaw(raw string) string {
	return c.keyBase() + ":" + sanitizeKey(raw)
}

// sanitizeKey 键串安全化：":" 会破坏命名空间分段，超长键取 sha1 摘要收敛长度。
func sanitizeKey(s string) string {
	s = strings.ReplaceAll(s, ":", "_")
	if len(s) > 128 {
		sum := sha1.Sum([]byte(s))
		s = s[:96] + "#" + hex.EncodeToString(sum[:8])
	}
	return s
}

func defaultKeyFunc[K comparable]() func(K) string {
	return func(k K) string { return fmt.Sprintf("%v", k) }
}

// ---------- TTL / envelope 辅助 ----------

// jitteredTTL 物理 TTL + 按 key 确定性抖动（同 key 稳定，免随机注入且防惊群）
func (c *Cache[K, V]) jitteredTTL(ks string) time.Duration {
	return jitterDuration(c.cfg.TTL, c.cfg.JitterPct, ks)
}

// jitteredNegativeTTL 空值标记 TTL（同样带确定性抖动）；NegativeTTL<=0 时返回 0。
func (c *Cache[K, V]) jitteredNegativeTTL(ks string) time.Duration {
	if c.cfg.NegativeTTL <= 0 {
		return 0
	}
	return jitterDuration(c.cfg.NegativeTTL, c.cfg.JitterPct, ks)
}

func jitterDuration(base time.Duration, jitterPct int, ks string) time.Duration {
	if jitterPct <= 0 {
		return base
	}
	span := uint64(base) * uint64(jitterPct) / 100
	if span == 0 {
		return base
	}
	h := fnv64(ks)
	return time.Duration(uint64(base) - span/2 + h%(span+1))
}

// buildEnvelope 组装 envelope 原始串（setEnvelope / setEnvelopeSeq 共用）。
// val==nil 时写零值 + Null 由调用方决定。
func (c *Cache[K, V]) buildEnvelope(val *V, seq int64, null bool) (string, error) {
	e := &envelope[V]{ExpMS: c.clock().UnixMilli() + int64(c.cfg.logicalTTL().Milliseconds()), Seq: seq, Null: null}
	if val != nil {
		e.Value = *val
	} else {
		var zero V
		e.Value = zero
	}
	return encode(c.codec, e)
}

// setEnvelope 把值编码为 envelope 写入 Redis（带物理 TTL 与逻辑过期戳）。
// val==nil 且 nullTTl>0 表示写空标记。无条件覆盖，仅用于读线回填等
// 「本次 seq 一定是最新」的路径；写线请用 setEnvelopeSeq。
func (c *Cache[K, V]) setEnvelope(ctx context.Context, ks string, val *V, ttl time.Duration, seq int64, null bool) error {
	raw, err := c.buildEnvelope(val, seq, null)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	return c.kv.Set(sctx, ks, raw, ttl)
}

// setEnvelopeSeq 同 setEnvelope，但按 seq 条件写（Redis 侧 Lua CAS）：
// 只有「键不存在 / 值不可解析 / 现有 envelope.Seq 不大于本次 seq」才真正落地。
// written=false 表示 Redis 里已有更大 seq 的值，本次是过期写——调用方不得再
// 把该值放进写缓冲，否则旧值会在随后 flush 时盖掉库里的新行。
// KV 不实现 SeqKV 时退回无条件 Set，written 恒为 true（旧行为）。
func (c *Cache[K, V]) setEnvelopeSeq(ctx context.Context, ks string, val *V, ttl time.Duration, seq int64, null bool) (written bool, err error) {
	if c.sq == nil {
		return true, c.setEnvelope(ctx, ks, val, ttl, seq, null)
	}
	raw, err := c.buildEnvelope(val, seq, null)
	if err != nil {
		return false, err
	}
	sctx, cancel := context.WithTimeout(ctx, c.cfg.WriteTimeout)
	defer cancel()
	return c.sq.SetSeq(sctx, ks, raw, ttl, seq)
}

// delSeq 按 seq 条件删除（Remove 用）：现有值 seq 严格更大时不删。
// KV 不实现 SeqKV 时退回无条件 Del，written 恒为 true。
func (c *Cache[K, V]) delSeq(ctx context.Context, ks string, seq int64) (written bool, err error) {
	if c.sq == nil {
		return true, c.kv.Del(ctx, ks)
	}
	return c.sq.DelSeq(ctx, ks, seq)
}

func fnv64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
