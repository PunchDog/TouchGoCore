package dbcache

import (
	"strings"
	"time"

	"touchgocore/config"
)

// Config 是缓存层的运行参数。零值字段由 normalize 补默认值。
type Config struct {
	KeyPrefix      string        // Redis 键前缀
	TTL            time.Duration // 行 hash 过期时间；0 表示不过期
	FlushInterval  time.Duration // 延迟落库扫描间隔
	QueryTimeout   time.Duration // 单次查询超时
	FlushTimeout   time.Duration // 单次落库超时
	CloseBudget    time.Duration // 停机 final flush 预算
	MaxRetry       int           // 失败退避档位上限
	DirtyBatchSize int           // 每批领取的脏键数
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
	if c.TTL < 0 {
		c.TTL = 0
	}
	return c
}
