package syncmap

// 分片并发表（S69）。
//
// Map 用一把 RWMutex 护一张原生 map：读多写少的热点表上，读锁本身虽不互斥，
// 但每次 RLock/RUnlock 都要在同一处字上做原子加减，几十个协程按消息频率查表时，
// 这个字所在的缓存行会在核间来回失效，锁的排队时间最终变成消息处理时间的一部分。
//
// ShardedMap 按键切成 2^N 片，每片各带一把锁和一张小 map，读写只落在自己那片上，
// 锁竞争与缓存行失效都被摊薄。语义与 Map 逐一对应，差异只有两处（见各方法注释）：
//   - 遍历（Range/RangeBySort/List）不保证跨片的整体顺序，也不是一次全局一致快照；
//   - 键的散列默认按类型自动完成，只有想自己控制分布时才用 NewShardedMapFunc。

import (
	"hash/maphash"
	"sort"
	"sync"
	"sync/atomic"
)

// 分片数上限：再往上每片的 map 已经小到不值得多一把锁，且 Clear/Range 的逐片开销线性上升
const (
	maxShardsLog = 13 // 8192 片
	defShards    = 16
)

// shardedShard 一个分片：自己的锁 + 自己的小表。
//
// _ 是刻意填到一缓存行的：分片对象同尺寸连续分配，不填的话相邻两片的锁会落在同一个
// 64 字节行里，一片的加锁让另一片的行失效——分片就白做了。
type shardedShard[K comparable, V any] struct {
	mu sync.RWMutex
	mp map[K]V
	_  [64]byte
}

// ShardedMap 是按键分片的并发表。零值可直接使用（默认 16 片），
// 需要指定片数或自定义散列时用 NewShardedMap / NewShardedMapFunc。
type ShardedMap[K comparable, V any] struct {
	once    sync.Once      // 惰性建片，保证零值可用
	seed    maphash.Seed   // 由构造设置；零值时在 ensure 里补
	want    int            // 期望片数，构造时写入，必须先于任何并发访问
	hashKey func(K) uint64 // 自定义散列，nil 表示用内置的按键类型散列

	num    atomic.Int64
	shards []*shardedShard[K, V]
	mask   uint64 // 片数-1；片数恒为 2 的幂，取模退化成一次按位与
}

// NewShardedMap 建一张分片表。shards 会向上取到 2 的幂，并钳在 [1, 1<<maxShardsLog]。
func NewShardedMap[K comparable, V any](shards int) *ShardedMap[K, V] {
	return &ShardedMap[K, V]{want: shards}
}

// NewShardedMapFunc 与 NewShardedMap 同，但由调用方给散列函数。
// 用于分布特征只有业务知道的键（例如按 uid 分段、想让同一用户的键落在同一片以复用缓存行），
// 或想刻意减少片数以提高命中局部性。自定义散列必须对同一键稳定返回同一值。
func NewShardedMapFunc[K comparable, V any](shards int, hash func(K) uint64) *ShardedMap[K, V] {
	return &ShardedMap[K, V]{want: shards, hashKey: hash}
}

// ensure 首次使用时建片。构造函数写完 want 才把表交给别的协程，
// 因此这里不存在「want 还没定就并发建片」的窗口。
func (m *ShardedMap[K, V]) ensure() {
	m.once.Do(func() {
		n := m.want
		if n <= 0 {
			n = defShards
		}
		if n > 1<<maxShardsLog {
			n = 1 << maxShardsLog
		}
		// 向上取到 2 的幂
		pow := 1
		for pow < n {
			pow <<= 1
		}
		// 每个实例自己的随机种子：内置的 Map 靠 runtime 自己散列，这里若用零种子，
		// 敌意键值（同一批构造出来的字符串键）就能把负载全压到一片上。
		if m.hashKey == nil {
			m.seed = maphash.MakeSeed()
		}
		m.shards = make([]*shardedShard[K, V], pow)
		for i := range m.shards {
			m.shards[i] = &shardedShard[K, V]{mp: make(map[K]V)}
		}
		m.mask = uint64(pow - 1)
	})
}

// indexOf 键所在片。
func (m *ShardedMap[K, V]) indexOf(k K) uint64 {
	return m.hashOf(k) & m.mask
}

// hashOf 键散列。自定义散列优先；否则用 maphash.Comparable，它按 K 的实际类型散列，
// string、各宽度整数、结构体乃至数组都能得到稳定且分散的结果。
//
// 不要改回「类型 switch + 整型散列」的写法：switch 覆盖不到的键型只能退化成固定片
// （正确但丢掉并行度），而按类型自动散列对任何 comparable 键都成立。
// 唯一要求是同一键两次散列必然同片，这是分片表正确性的底线。
func (m *ShardedMap[K, V]) hashOf(k K) uint64 {
	if m.hashKey != nil {
		return m.hashKey(k)
	}
	return maphash.Comparable(m.seed, k)
}

// Length 返回当前元素个数。总数是原子量，与片数无关。
func (m *ShardedMap[K, V]) Length() int {
	return int(m.num.Load())
}

// Store 写入或更新一个键。已存在的键只改值，不重复计数。
func (m *ShardedMap[K, V]) Store(k K, v V) {
	m.ensure()
	s := m.shards[m.indexOf(k)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mp == nil {
		s.mp = make(map[K]V)
	}
	if _, exist := s.mp[k]; !exist {
		m.num.Add(1)
	}
	s.mp[k] = v
}

// Delete 按键删除。只有键确实存在时才扣计数。
func (m *ShardedMap[K, V]) Delete(k K) {
	m.ensure()
	s := m.shards[m.indexOf(k)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exist := s.mp[k]; exist {
		delete(s.mp, k)
		m.num.Add(-1)
	}
}

// Clear 清空整表。逐片清、逐片扣计数，因此与并发 Store 的净效果仍是「数与表一致」：
// 某个键若在它的片被清之后才写进来，它会被正常计入总数。
func (m *ShardedMap[K, V]) Clear() {
	m.ensure()
	for _, s := range m.shards {
		s.mu.Lock()
		n := len(s.mp)
		s.mp = make(map[K]V)
		s.mu.Unlock()
		if n > 0 {
			m.num.Add(-int64(n))
		}
	}
}

// ClearAll 对每个元素回调，随后把表清空。fn 返回 false 只表示「不用再回调了」，
// 元素仍会被移除（与 Map.ClearAll 同结果，名字里的 All 优先）；fn 为 nil 等价于 Clear。
//
// 与 Map.ClearAll 的关键差别：回调在锁外执行。Map 版持写锁跑回调，回调里只要碰一次
// 本表（Delete/Store/Range 都要同一把锁）就自死锁，而清理路径恰恰最容易写成
// 「遍历中把每个客户端关掉」。分片版逐片快照、锁外回调、再回片删除，回调里可以
// 安全调用其它方法。代价是移除范围只到「该片快照所见」：回调期间新写进来的键不保证
// 被清掉（Map 版做不到这件事，因为它根本不允许回调里改表）。
func (m *ShardedMap[K, V]) ClearAll(fn func(k K, v V) bool) {
	m.ensure()
	if fn == nil {
		m.Clear()
		return
	}
	stop := false
	for _, s := range m.shards {
		s.mu.RLock()
		keys := make([]K, 0, len(s.mp))
		vals := make([]V, 0, len(s.mp))
		for k, v := range s.mp {
			keys = append(keys, k)
			vals = append(vals, v)
		}
		s.mu.RUnlock()
		if !stop {
			for i := range keys {
				if !fn(keys[i], vals[i]) {
					stop = true
					break
				}
			}
		}
		if len(keys) == 0 {
			continue
		}
		// 删除回片内做：回调期间这个键可能已被并发 Delete 掉，重复扣计数会把总数打成负数。
		s.mu.Lock()
		for _, k := range keys {
			if _, exist := s.mp[k]; exist {
				delete(s.mp, k)
				m.num.Add(-1)
			}
		}
		s.mu.Unlock()
	}
}

// LoadOrStore 已有则返回现值，没有则写入并返回给定值。
func (m *ShardedMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	m.ensure()
	s := m.shards[m.indexOf(key)]
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mp == nil {
		s.mp = make(map[K]V)
	}
	if actual, loaded = s.mp[key]; loaded {
		return actual, true
	}
	s.mp[key] = value
	m.num.Add(1)
	return value, false
}

// Load 读取一个键。
func (m *ShardedMap[K, V]) Load(k K) (v V, ok bool) {
	m.ensure()
	s := m.shards[m.indexOf(k)]
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok = s.mp[k]
	return
}

// Range 对每个键值对调用 fn，fn 返回 false 时停止。
//
// 逐片「锁内快照、锁外回调」：回调里可以安全改表（Map.Range 的这条契约保留），
// 但不保证跨片的一致快照——片 0 快照之后、片 1 快照之前写进片 1 的键会被本次遍历
// 看到，反之亦然。需要全局一致快照的场景只有排序遍历，见 RangeBySort 的说明。
func (m *ShardedMap[K, V]) Range(fn func(k K, v V) bool) {
	if fn == nil {
		return
	}
	m.ensure()
	for _, s := range m.shards {
		s.mu.RLock()
		keys := make([]K, 0, len(s.mp))
		vals := make([]V, 0, len(s.mp))
		for k, v := range s.mp {
			keys = append(keys, k)
			vals = append(vals, v)
		}
		s.mu.RUnlock()
		for i := range keys {
			if !fn(keys[i], vals[i]) {
				return
			}
		}
	}
}

// List 返回全部值的切片，sortFunc 非空时按其排序。
func (m *ShardedMap[K, V]) List(sortFunc func(d1, d2 V) bool) []V {
	m.ensure()
	pairs := make([]V, 0, m.Length())
	for _, s := range m.shards {
		s.mu.RLock()
		for _, v := range s.mp {
			pairs = append(pairs, v)
		}
		s.mu.RUnlock()
	}
	if sortFunc != nil {
		sort.Slice(pairs, func(i, j int) bool {
			return sortFunc(pairs[i], pairs[j])
		})
	}
	return pairs
}

// RangeBySort 按 sortFunc 指定的顺序遍历。fn 或 sortFunc 为 nil 时分别退化为
// 不遍历 / 无序的 Range。
//
// 快照是逐片取的：排序用的这份列表整体上不保证是某一时刻的一致快照。
// 需要一致快照的场景应当用 Map（单表、单锁），而不是本类型。
func (m *ShardedMap[K, V]) RangeBySort(fn func(k K, v V) bool, sortFunc func(d1, d2 V) bool) {
	if fn == nil {
		return
	}
	if sortFunc == nil {
		m.Range(fn)
		return
	}
	m.ensure()
	type sortTemp struct {
		key   K
		value V
	}
	pairs := make([]sortTemp, 0, m.Length())
	for _, s := range m.shards {
		s.mu.RLock()
		for k, v := range s.mp {
			pairs = append(pairs, sortTemp{k, v})
		}
		s.mu.RUnlock()
	}
	sort.Slice(pairs, func(i, j int) bool {
		return sortFunc(pairs[i].value, pairs[j].value)
	})
	for _, pair := range pairs {
		if !fn(pair.key, pair.value) {
			return
		}
	}
}
