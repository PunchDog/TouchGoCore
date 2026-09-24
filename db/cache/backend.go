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
