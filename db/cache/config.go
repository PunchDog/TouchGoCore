// Package cache 提供 Redis 一级缓存 + MySQL/Mongo 回源的两级缓存封装。
//
// 两条读写线（Redis 优先）：
//
//	读线：client → GetOrLoad → Redis → miss 时受控协程回源数据库 → 写 Redis → 从 Redis 读回返回。
//	写线：Write → 同步写 Redis 成功后进写缓冲（通知数据库）→ 定时器批量落库。
//
// 本包只依赖 redis.Cmdable（经 KV 窄接口）与 Loader/Saver 抽象，不 import db 根包；
// Redis/Mongo 适配器由 db 包门面（db/cache_api.go、db/cache_mongo_adapter.go）提供。
package cache

import (
	"fmt"
	"time"
)

// StalePolicy 命中但逻辑过期时的行为
type StalePolicy int

const (
	// StaleAsync 立即返回 Redis 中的旧值，另起协程回源刷新（默认）
	StaleAsync StalePolicy = iota
	// StaleWait 像 miss 一样同步回源→写 Redis→读回
	StaleWait
)

// OverflowPolicy 写缓冲超过 MaxDirtyKeys 时的行为
type OverflowPolicy int

const (
	// OverflowBlock 在 Write 内联触发一次 flush，形成背压（默认，宁慢勿丢）
	OverflowBlock OverflowPolicy = iota
	// OverflowDrop 仅催 flush 并告警，不阻塞写入方
	OverflowDrop
)

// Config 单个 Cache 的行为配置。零值无效，使用 DefaultConfig() 起步。
type Config struct {
	// KeyPrefix Redis 键前缀，默认 "tg"；最终键形如 {prefix}:{group}:{name}:{key}
	KeyPrefix string
	// Group 分组建（多服共 Redis 时隔离用），一般取 util.GameGroup；空则省略该段
	Group string

	// TTL 物理过期时间（Redis EXPIRE），所有写入必带，默认 5m
	TTL time.Duration
	// LogicalPct 逻辑过期占物理 TTL 的百分比（1..99），默认 80。
	// 逻辑过期只影响「是否提前异步预热」，不改物理 TTL。
	LogicalPct int
	// JitterPct TTL 抖动百分比（0..50），按 key 确定性散列，防同批集中过期惊群，默认 10
	JitterPct int
	// NegativeTTL 空值标记（防穿透）的过期时间，<=0 关闭空值缓存，默认 30s
	NegativeTTL time.Duration

	// Stale 逻辑过期策略，默认 StaleAsync
	Stale StalePolicy
	// ReadTimeout 回源超时，默认 300ms
	ReadTimeout time.Duration
	// WriteTimeout 写/回填 Redis 超时，默认 300ms
	WriteTimeout time.Duration
	// FailBackoff 回源失败后的退避窗口（不重复打 DB），默认 2s
	FailBackoff time.Duration
	// RequireRedis 为 true（默认）时，读线的值必须经过 Redis（回填+读回），
	// Redis 写不进则本次请求报错——不回退透传数据库值；写线 Write 同理先写 Redis 成功才入缓冲。
	RequireRedis bool

	// FlushInterval 写线定时器间隔，默认 2s
	FlushInterval time.Duration
	// BatchSize 单次落库批量上限，默认 200
	BatchSize int
	// MaxDirtyKeys 写缓冲脏键上限，超过时按 Overflow 策略处理，默认 100000
	MaxDirtyKeys int
	// MaxDirtyAge 脏键最老滞留时间，超过仅告警（数据长时间未落库），默认 5m
	MaxDirtyAge time.Duration
	// Shards 写缓冲分片数，必须是 2 的幂，默认 16
	Shards int
	// MaxRetry 单条脏数据落库最大重试轮数，超过后丢弃并 Del 缓存，默认 3
	MaxRetry int
	// SaveConcurrency 无 BatchSaver 时单条落库的并发度，默认 4
	SaveConcurrency int
	// MGetConcurrency 集群模式批量读退化后的并发度，默认 16
	MGetConcurrency int
	// Overflow 缓冲溢出策略，默认 OverflowBlock
	Overflow OverflowPolicy

	// Journal 脏账本：Write/Remove 同步写 Redis 时顺带把「待落库」记进
	// Redis ZSET 账本，重启时扫账恢复，防非优雅退出丢缓冲数据。
	// 默认 true；KV 不实现 Journaler 时自动失效。
	Journal bool

	// Enabled 总开关；false 时 Write/Remove 退化为直接同步落库（write-through），
	// GetOrLoad/MGetOrLoad 直连回源且不信任 Redis 命中。默认 true
	Enabled bool
}

// DefaultConfig 返回框架默认配置
func DefaultConfig() Config {
	return Config{
		KeyPrefix:       "tg",
		TTL:             5 * time.Minute,
		LogicalPct:      80,
		JitterPct:       10,
		NegativeTTL:     30 * time.Second,
		Stale:           StaleAsync,
		ReadTimeout:     300 * time.Millisecond,
		WriteTimeout:    300 * time.Millisecond,
		FailBackoff:     2 * time.Second,
		RequireRedis:    true,
		FlushInterval:   2 * time.Second,
		BatchSize:       200,
		MaxDirtyKeys:    100000,
		MaxDirtyAge:     5 * time.Minute,
		Shards:          16,
		MaxRetry:        3,
		SaveConcurrency: 4,
		MGetConcurrency: 16,
		Overflow:        OverflowBlock,
		Journal:         true,
		Enabled:         true,
	}
}

// Validate 校验配置合法性
func (c Config) Validate() error {
	if c.TTL <= 0 {
		return fmt.Errorf("cache: TTL 必须大于 0")
	}
	if c.LogicalPct < 1 || c.LogicalPct > 99 {
		return fmt.Errorf("cache: LogicalPct=%d 必须在 1..99", c.LogicalPct)
	}
	if c.JitterPct < 0 || c.JitterPct > 50 {
		return fmt.Errorf("cache: JitterPct=%d 必须在 0..50", c.JitterPct)
	}
	if c.ReadTimeout <= 0 || c.WriteTimeout <= 0 {
		return fmt.Errorf("cache: ReadTimeout/WriteTimeout 必须大于 0")
	}
	if c.FlushInterval <= 0 {
		return fmt.Errorf("cache: FlushInterval 必须大于 0")
	}
	if c.BatchSize <= 0 {
		return fmt.Errorf("cache: BatchSize 必须大于 0")
	}
	if c.MaxDirtyKeys <= 0 {
		return fmt.Errorf("cache: MaxDirtyKeys 必须大于 0")
	}
	if c.Shards <= 0 || c.Shards&(c.Shards-1) != 0 {
		return fmt.Errorf("cache: Shards=%d 必须是 2 的幂", c.Shards)
	}
	if c.MaxRetry < 0 {
		return fmt.Errorf("cache: MaxRetry 不能为负")
	}
	if c.SaveConcurrency <= 0 || c.MGetConcurrency <= 0 {
		return fmt.Errorf("cache: SaveConcurrency/MGetConcurrency 必须大于 0")
	}
	return nil
}

// logicalTTL 逻辑过期时长（物理 TTL 的一部分）
func (c Config) logicalTTL() time.Duration {
	return c.TTL * time.Duration(c.LogicalPct) / 100
}
