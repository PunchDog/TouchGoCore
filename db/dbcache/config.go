package dbcache

import (
	"strings"
	"time"

	"touchgocore/config"
	"touchgocore/vars"
)

// Config 是缓存层的运行参数。零值字段由 normalize 补默认值。
type Config struct {
	KeyPrefix      string        // Redis 键前缀
	TTL            time.Duration // 行 hash 过期时间；权威模式必须 0（行过期=丢数据）
	FlushInterval  time.Duration // 延迟落库扫描间隔
	QueryTimeout   time.Duration // 单次查询超时
	FlushTimeout   time.Duration // 单次落库超时
	CloseBudget    time.Duration // 停机 final flush 预算
	MaxRetry       int           // 失败退避档位上限
	DirtyBatchSize int           // 每批领取的脏键数

	MaxScanRows  int    // 无索引扫描行数预算；超限 ErrScanBudgetExceeded（fail-closed）
	TxRowLocks   bool   // 事务/单op写是否取 Redis 行锁（资金链读己写安全前提）
	TxLockWaitMS int    // 行锁等待上限毫秒；超时 ErrTxConflict
	TxLockTTLMS  int    // 行锁 PX 存活毫秒；崩溃兜底
	SinkRequired bool   // true 时 mustSave 无 sink 报 ErrSinkRequired；false 告警放行
	GlobalSlot   bool   // true 时 hash tag 用 {prefix} 全表同槽（Cluster 跨表事务必须）
	SpecTimeZone string // DateBucket 分桶时区；默认 UTC
	RequireAOF   bool   // Init 时检查 Redis AOF；缺失响亮告警
}

// DefaultConfig 返回框架默认参数。
func DefaultConfig() Config {
	return Config{
		KeyPrefix:      "tgc",
		TTL:            0,
		FlushInterval:  3 * time.Second,
		QueryTimeout:   2 * time.Second,
		FlushTimeout:   10 * time.Second,
		CloseBudget:    5 * time.Second,
		MaxRetry:       5,
		DirtyBatchSize: 200,
		MaxScanRows:    100000,
		TxRowLocks:     true,
		TxLockWaitMS:   5000,
		TxLockTTLMS:    30000,
		SpecTimeZone:   "UTC",
		RequireAOF:     true,
	}
}

// ConfigFrom 把配置段转换为运行参数；nil 或缺失字段回落默认值。
func ConfigFrom(c *config.CacheConfig) Config {
	out := DefaultConfig()
	if c == nil {
		return out
	}
	if p := strings.TrimSpace(c.KeyPrefix); p != "" {
		out.KeyPrefix = p
	}
	if c.TTLMS > 0 {
		out.TTL = time.Duration(c.TTLMS) * time.Millisecond
	}
	if c.FlushIntervalMS > 0 {
		out.FlushInterval = time.Duration(c.FlushIntervalMS) * time.Millisecond
	}
	if c.QueryTimeoutMS > 0 {
		out.QueryTimeout = time.Duration(c.QueryTimeoutMS) * time.Millisecond
	}
	if c.FlushTimeoutMS > 0 {
		out.FlushTimeout = time.Duration(c.FlushTimeoutMS) * time.Millisecond
	}
	if c.CloseBudgetMS > 0 {
		out.CloseBudget = time.Duration(c.CloseBudgetMS) * time.Millisecond
	}
	if c.MaxRetry > 0 {
		out.MaxRetry = c.MaxRetry
	}
	if c.DirtyBatchSize > 0 {
		out.DirtyBatchSize = c.DirtyBatchSize
	}
	if c.MaxScanRows > 0 {
		out.MaxScanRows = c.MaxScanRows
	}
	if c.TxRowLocks != nil {
		out.TxRowLocks = *c.TxRowLocks
	}
	if c.TxLockWaitMS > 0 {
		out.TxLockWaitMS = c.TxLockWaitMS
	}
	if c.TxLockTTLMS > 0 {
		out.TxLockTTLMS = c.TxLockTTLMS
	}
	out.SinkRequired = c.SinkRequired
	out.GlobalSlot = c.GlobalSlot
	if strings.TrimSpace(c.SpecTimeZone) != "" {
		out.SpecTimeZone = c.SpecTimeZone
	}
	if c.RequireAOF != nil {
		out.RequireAOF = *c.RequireAOF
	}
	return out
}

// normalize 补齐零值，保证定时器与批量参数可用。
func (c Config) normalize() Config {
	def := DefaultConfig()
	if strings.TrimSpace(c.KeyPrefix) == "" {
		c.KeyPrefix = def.KeyPrefix
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = def.FlushInterval
	}
	if c.QueryTimeout <= 0 {
		c.QueryTimeout = def.QueryTimeout
	}
	if c.FlushTimeout <= 0 {
		c.FlushTimeout = def.FlushTimeout
	}
	if c.CloseBudget <= 0 {
		c.CloseBudget = def.CloseBudget
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = def.MaxRetry
	}
	if c.DirtyBatchSize <= 0 {
		c.DirtyBatchSize = def.DirtyBatchSize
	}
	if c.MaxScanRows <= 0 {
		c.MaxScanRows = def.MaxScanRows
	}
	if c.TxLockWaitMS <= 0 {
		c.TxLockWaitMS = def.TxLockWaitMS
	}
	if c.TxLockTTLMS <= 0 {
		c.TxLockTTLMS = def.TxLockTTLMS
	}
	if strings.TrimSpace(c.SpecTimeZone) == "" {
		c.SpecTimeZone = def.SpecTimeZone
	}
	if c.TTL < 0 {
		c.TTL = 0
	}
	if c.TTL > 0 {
		// Redis 即真源：行 hash 过期等于静默丢数据。配置残留 TTL（旧部署 conf）
		// 一律钳到 0 并响亮告警，而不是拒绝启动（兼容未改 conf 的消费方）。
		vars.Warning("dbcache: 权威模式 TTL 必须为 0，已把 %v 钳到 0（行过期=丢数据）", c.TTL)
		c.TTL = 0
	}
	return c
}
