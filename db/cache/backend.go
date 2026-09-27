package cache

import (
	"context"
	"errors"
)

var errNoSaver = errors.New("cache: saver not configured")

// ErrNotFound 回源确认「该键在源中不存在」→ 触发空值缓存；
// 其他 error 视为源故障 → 触发失败退避，不写空标记。
var ErrNotFound = errors.New("cache: not found")

// ErrMiss 仅查缓存（Get/MGet）未命中
var ErrMiss = errors.New("cache: miss")

// ErrSourceUnavailable 回源正处于失败退避窗口
var ErrSourceUnavailable = errors.New("cache: source unavailable (fail backoff)")

// Loader 读线回源接口
type Loader[K comparable, V any] interface {
	Load(ctx context.Context, key K) (*V, error)
}

// BatchLoader 可选批量回源；未实现时 MGetOrLoad 退化为并发单键 GetOrLoad
type BatchLoader[K comparable, V any] interface {
	Loader[K, V]
	// LoadBatch 返回 map，键为 KeyOf(key)；源中不存在的键不出现在结果里
	LoadBatch(ctx context.Context, keys []K) (map[string]*V, error)
}

// KeyBinder 是 BatchLoader 可选实现的「键映射延迟绑定」接口。
//
// LoadBatch 的结果 map 必须以 Cache.KeyOf(key) 为键（含命名空间前缀与 sanitize），
// 但 BatchLoader 被构造时还拿不到它将挂靠哪个 Cache、那个 Cache 的 prefix/group
// 又是什么——它手里只有 keyFn(key) 得到的裸键串。两边各算一半就会错位：
// 回填键无前缀/未 sanitize，查询侧用 KeyOf 去取永远取不到，于是批量回源
// 全部落空、静默退化成「每次 MGetOrLoad 都打一次 DB 但一个也用不上」。
//
// 因此由 Cache.New 在构造末尾类型断言探测并注入自己的 KeyOf（late-binding）；
// 实现方在未绑定时直接调 LoadBatch 必须报错而不是静默返回错键的结果。
type KeyBinder interface {
	// BindKeyOf 注入「裸键串 → 完整 Redis 键」的映射（即 Cache.keyOfRaw）。
	BindKeyOf(func(rawKey string) string)
}

// ErrKeyOfUnbound 批量回源尚未绑定 KeyOf：说明该 BatchLoader 未经 Cache.New/Register
// 注入就直接调了 LoadBatch。宁可报错也不静默返回一堆对不上键的结果。
var ErrKeyOfUnbound = errors.New("cache: BatchLoader 未绑定 KeyOf（须经 Cache.New/Register 注入）")

// OpKind 写线条目操作类型
type OpKind uint8

const (
	OpUpsert OpKind = iota + 1
	OpDelete
)

// Item 写缓冲→落库的一条操作
type Item[K comparable] struct {
	Key   K
	Op    OpKind
	Value any // *V；类型擦除换取跨后端（MySQL 实体 / Mongo bson）复用批接口
}

// Saver 写线单条落库接口
type Saver[K comparable, V any] interface {
	Save(ctx context.Context, key K, val *V) error
	Delete(ctx context.Context, key K) error
}

// BatchSaver 可选批量落库；实现后 flush 优先走批量一条 SQL
type BatchSaver[K comparable, V any] interface {
	Saver[K, V]
	SaveBatch(ctx context.Context, items []Item[K]) error
}

// LoaderFunc 闭包版 Loader
type LoaderFunc[K comparable, V any] func(ctx context.Context, key K) (*V, error)

func (f LoaderFunc[K, V]) Load(ctx context.Context, key K) (*V, error) { return f(ctx, key) }

// SaverFunc 闭包版 Saver；DeleteFn 为 nil 时 Delete 返回「不支持」错误
type SaverFunc[K comparable, V any] struct {
	SaveFn   func(ctx context.Context, key K, val *V) error
	DeleteFn func(ctx context.Context, key K) error
}

func (s SaverFunc[K, V]) Save(ctx context.Context, key K, val *V) error {
	if s.SaveFn == nil {
		return errNoSaver
	}
	return s.SaveFn(ctx, key, val)
}

func (s SaverFunc[K, V]) Delete(ctx context.Context, key K) error {
	if s.DeleteFn == nil {
		return errNoSaver
	}
	return s.DeleteFn(ctx, key)
}
