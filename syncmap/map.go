package syncmap

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Map is a concurrent-safe map implementation with atomic counter.
// It provides thread-safe operations for storing, retrieving, and iterating
// over key-value pairs.
//
// Deprecated: Prefer [MapGeneric] for type-safe operations, or use
// Go's built-in sync.Map for better performance in read-heavy workloads.
type Map[K comparable, V any] struct {
	mp  map[K]V
	num atomic.Int64
	mu  sync.RWMutex
}

// Length returns the current number of elements in the map.
// This operation is atomic and lock-free.
func (m *Map[K, V]) Length() int {
	return int(m.num.Load())
}

// Store adds or updates a key-value pair.
// If the key already exists, its value is updated without changing the count.
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

// Delete removes a key-value pair by key.
// The counter is decremented only if the key existed.
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

// ClearAll removes all entries from the map after invoking the callback for each.
// The callback function 'fn' returns true to continue iteration, false to stop.
// The callback is invoked while holding the write lock - no Store/Delete operations
// on this map are allowed within the callback.
//
// ClearAll is designed for cleanup scenarios where elements need to be
// processed before removal, such as closing resources or releasing references.
func (m *Map[K, V]) ClearAll(fn func(k K, v V) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range m.mp {
		if !fn(k, v) {
			break
		}
	}
	m.mp = make(map[K]V)
	m.num.Store(0)
}

// LoadOrStore returns the existing value for key if present.
// Otherwise, it stores the given value and returns the value as loaded.
// This operation is atomic.
func (m *Map[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mp == nil {
		m.mp = make(map[K]V)
	}

	actual, loaded = m.mp[key]
	if !loaded {
		m.mp[key] = value
		m.num.Add(1)
	}
	return
}

// Load returns the value associated with the key and a boolean indicating
// whether the key was found.
func (m *Map[K, V]) Load(k K) (v V, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok = m.mp[k]
	return
}

// Range calls fn for each key-value pair in the map.
// If fn returns false, Range stops the iteration.
// The callback is invoked while holding a read lock.
func (m *Map[K, V]) Range(fn func(k K, v V) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k, v := range m.mp {
		if !fn(k, v) {
			break
		}
	}
}

// RangeBySort iterates over key-value pairs in sorted order.
// If sortFunc is nil, behaves like Range.
// sortFunc should compare values: return true if d1 should come before d2.
func (m *Map[K, V]) RangeBySort(fn func(k K, v V) bool, sortFunc func(d1, d2 V) bool) {
	if sortFunc == nil {
		m.Range(fn)
		return
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	type sortTemp struct {
		key   K
		value V
	}

	pairs := make([]sortTemp, 0, len(m.mp))
	for k, v := range m.mp {
		pairs = append(pairs, sortTemp{k, v})
	}

	sort.Slice(pairs, func(i, j int) bool {
		return sortFunc(pairs[i].value, pairs[j].value)
	})

	for _, pair := range pairs {
		if !fn(pair.key, pair.value) {
			break
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
