package syncmap

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Map 是带原子计数的并发安全 map，读写均需持锁。
// 提供键值对的存取、删除与遍历。
//
// Deprecated: 写多读多的热点表请用 [ShardedMap]（键分片、各片独立加锁，实测并行读
// 与读写混合快 2.1~2.8 倍）；只读为主的场景用 Go 内置的 sync.Map。
// 仍要选 Map 的理由只剩一个：Range/RangeBySort/List 拿的是单锁下的全局一致快照，
// ShardedMap 逐片快照，不保证跨片的整体一致性。
type Map[K comparable, V any] struct {
	mp  map[K]V
	num atomic.Int64
	mu  sync.RWMutex
}

// Length 返回当前元素数。计数由原子变量维护，取数不加锁，
// 因此可能与增删动作并行发生偏移（读到的可能是瞬时值）。
func (m *Map[K, V]) Length() int {
	return int(m.num.Load())
}

// Store 写入键值对。键已存在时只覆盖值，计数不变。
func (m *Map[K, V]) Store(k K, v V) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mp == nil {
		m.mp = make(map[K]V)
	}
	if _, h := m.mp[k]; !h {
		m.num.Add(1)
	}
	m.mp[k] = v
}

// Delete 按键删除。仅当键确实存在时计数才减一。
func (m *Map[K, V]) Delete(k K) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, h := m.mp[k]; h {
		delete(m.mp, k)
		m.num.Add(-1)
	}
}

func (m *Map[K, V]) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.mp = make(map[K]V)
	m.num.Store(0)
}

// ClearAll 先对每个元素回调、再整体清空。fn 返回 true 继续，false 提前停止；
// fn 为 nil 时只清空不回调。
//
// 面向的是「删除前必须先处理每个元素」的清理场景，例如关闭连接、释放引用。
// 注意它全程持写锁跑回调，回调里不得再触碰本 Map。
func (m *Map[K, V]) ClearAll(fn func(k K, v V) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn == nil {
		m.mp = make(map[K]V)
		m.num.Store(0)
		return
	}
	for k, v := range m.mp {
		if !fn(k, v) {
			break
		}
	}
	m.mp = make(map[K]V)
	m.num.Store(0)
}

// LoadOrStore 键已存在则返回现值（loaded=true），否则写入并返回给定值
// （loaded=false）。整个判断与写入在同一次持锁里完成。
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mp == nil {
		m.mp = make(map[K]V)
	}

	if actual, loaded = m.mp[key]; loaded {
		return actual, true
	}
	m.mp[key] = value
	m.num.Add(1)
	return value, false
}

// Load 返回键对应的值，以及键是否存在。
func (m *Map[K, V]) Load(k K) (v V, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok = m.mp[k]
	return
}

// Range 对每个键值对调用 fn；fn 返回 false 时停止遍历。
//
// 迭代基于锁内快照、锁外回调：回调中可以安全调用 Delete/Store/Range，
// 不会再因持有读锁而自死锁。代价是回调看到的是一致性快照，
// 遍历期间的并发修改不反映到本次遍历。
func (m *Map[K, V]) Range(fn func(k K, v V) bool) {
	if fn == nil {
		return
	}

	m.mu.RLock()
	keys := make([]K, 0, len(m.mp))
	vals := make([]V, 0, len(m.mp))
	for k, v := range m.mp {
		keys = append(keys, k)
		vals = append(vals, v)
	}
	m.mu.RUnlock()

	for i := range keys {
		if !fn(keys[i], vals[i]) {
			return
		}
	}
}

func (m *Map[K, V]) List(sortFunc func(d1, d2 V) bool) []V {
	m.mu.RLock()
	pairs := make([]V, 0, len(m.mp))
	for _, v := range m.mp {
		pairs = append(pairs, v)
	}
	m.mu.RUnlock()

	if sortFunc != nil {
		sort.Slice(pairs, func(i, j int) bool {
			return sortFunc(pairs[i], pairs[j])
		})
	}
	return pairs
}

// RangeBySort 按序遍历：sortFunc 比较的是值，d1 应排在 d2 之前时返回 true；
// sortFunc 为 nil 时等价于 Range。
// 与 Range 一致：快照与排序在锁内完成，回调在锁外执行，避免回调内改表死锁。
func (m *Map[K, V]) RangeBySort(fn func(k K, v V) bool, sortFunc func(d1, d2 V) bool) {
	if fn == nil {
		return
	}
	if sortFunc == nil {
		m.Range(fn)
		return
	}

	type sortTemp struct {
		key   K
		value V
	}

	m.mu.RLock()
	pairs := make([]sortTemp, 0, len(m.mp))
	for k, v := range m.mp {
		pairs = append(pairs, sortTemp{k, v})
	}
	m.mu.RUnlock()

	sort.Slice(pairs, func(i, j int) bool {
		return sortFunc(pairs[i].value, pairs[j].value)
	})

	for _, pair := range pairs {
		if !fn(pair.key, pair.value) {
			return
		}
	}
}

type MapAny struct {
	Map[any, any]
}

func NewMap[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		mp: make(map[K]V),
	}
}

func NewAny() *MapAny {
	return &MapAny{
		Map: Map[any, any]{mp: make(map[any]any)},
	}
}
