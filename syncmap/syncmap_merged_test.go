package syncmap

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNewMap_Empty(t *testing.T) {
	m := NewMap[string, int]()
	if m.Length() != 0 {
		t.Fatalf("新建 map 长度应为 0，实际: %d", m.Length())
	}
}

func TestMap_StoreLoad(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	if v, ok := m.Load("a"); !ok || v != 1 {
		t.Fatalf("Load(a) 失败: %v, %v", v, ok)
	}
	if m.Length() != 2 {
		t.Fatalf("Length 应为 2，实际: %d", m.Length())
	}
}

func TestMap_Store_UpdateNoInc(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Store("a", 99)
	if m.Length() != 1 {
		t.Fatalf("覆盖不应增加计数，实际: %d", m.Length())
	}
	if v, _ := m.Load("a"); v != 99 {
		t.Fatalf("应读到新值，实际: %d", v)
	}
}

func TestMap_Delete(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)
	m.Delete("a")
	if _, ok := m.Load("a"); ok {
		t.Fatal("删除后不应命中")
	}
	if m.Length() != 0 {
		t.Fatalf("Length 应为 0，实际: %d", m.Length())
	}
	m.Delete("nonexistent") // 不应 panic
}

func TestMap_Clear(t *testing.T) {
	m := NewMap[int, string]()
	m.Store(1, "a")
	m.Store(2, "b")
	m.Clear()
	if m.Length() != 0 {
		t.Fatalf("Clear 后 Length 应为 0，实际: %d", m.Length())
	}
}

func TestMapAny_Basic(t *testing.T) {
	m := NewAny()
	m.Store("a", 1)
	m.Store("b", "two")
	if v, ok := m.Load("a"); !ok || v.(int) != 1 {
		t.Fatalf("Load 失败: %v, %v", v, ok)
	}
}

func TestMap_Range(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 5; i++ {
		m.Store(i, i*10)
	}
	count := 0
	m.Range(func(k, v int) bool {
		count++
		if v != k*10 {
			t.Fatalf("值异常: k=%d v=%d", k, v)
		}
		return true
	})
	if count != 5 {
		t.Fatalf("Range 应遍历 5 次，实际: %d", count)
	}
}

func TestMap_Range_Stop(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 10; i++ {
		m.Store(i, i)
	}
	count := 0
	m.Range(func(k, v int) bool {
		count++
		return count < 3 // 第三次停止
	})
	if count != 3 {
		t.Fatalf("Range 应在 3 次后停止，实际: %d", count)
	}
}

func TestMap_LoadOrStore_InsertReturnsValue(t *testing.T) {
	m := NewMap[string, *int]()
	v := new(int)
	*v = 42

	actual, loaded := m.LoadOrStore("a", v)
	if loaded {
		t.Fatal("首次插入 loaded 应为 false")
	}
	if actual != v {
		t.Fatalf("首次插入应返回传入的值，而非零值，实际: %v", actual)
	}
	if m.Length() != 1 {
		t.Fatalf("Length 应为 1，实际: %d", m.Length())
	}
}

func TestMap_LoadOrStore_ExistingWins(t *testing.T) {
	m := NewMap[string, int]()
	m.Store("a", 1)

	actual, loaded := m.LoadOrStore("a", 99)
	if !loaded {
		t.Fatal("已存在的 key，loaded 应为 true")
	}
	if actual != 1 {
		t.Fatalf("应返回已存在的值 1，实际: %d", actual)
	}
	if v, _ := m.Load("a"); v != 1 {
		t.Fatalf("原值不应被覆盖，实际: %d", v)
	}
	if m.Length() != 1 {
		t.Fatalf("Length 应为 1，实际: %d", m.Length())
	}
}

// golua/lua_table.go 的 SubTable 依赖「首次插入后直接类型断言」，
// MapAny 内嵌 Map[any,any]，这里覆盖它的同一条路径。
func TestMapAny_LoadOrStore_TypeAssertable(t *testing.T) {
	type sub struct{ n int }
	m := NewAny()

	data, _ := m.LoadOrStore("k", &sub{n: 7})
	got, ok := data.(*sub)
	if !ok {
		t.Fatalf("首次插入后应可直接断言，实际拿到: %#v", data)
	}
	if got.n != 7 {
		t.Fatalf("值不符，实际: %d", got.n)
	}
}

// keyed 非字符串、非整型的 comparable 键：内置散列必须按类型自动处理，
// 不能只覆盖 string/int 然后把结构体键全压到同一片上。
type keyed struct {
	UID  int64
	Name string
}

func TestShardedMap_ZeroValueUsable(t *testing.T) {
	var m ShardedMap[string, int]
	m.Store("a", 1)
	if v, ok := m.Load("a"); !ok || v != 1 {
		t.Fatalf("✘ 零值表读写失败: v=%v ok=%v", v, ok)
	}
	if m.Length() != 1 {
		t.Fatalf("✘ 零值表计数不符: %d", m.Length())
	}
	if len(m.shards) != defShards {
		t.Fatalf("✘ 零值表片数不符: got=%d want=%d", len(m.shards), defShards)
	}
}

func TestShardedMap_CountersFollowRealChanges(t *testing.T) {
	m := NewShardedMap[string, int](8)
	m.Store("a", 1)
	m.Store("a", 2) // 覆盖同名键不该重复计数
	if m.Length() != 1 {
		t.Fatalf("✘ 覆盖写重复计数: %d", m.Length())
	}
	if v, _ := m.Load("a"); v != 2 {
		t.Fatalf("✘ 覆盖写未生效: %d", v)
	}
	m.Delete("missing") // 删不存在的键不该扣计数
	if m.Length() != 1 {
		t.Fatalf("✘ 删除缺席键扣了计数: %d", m.Length())
	}
	m.Delete("a")
	m.Delete("a")
	if m.Length() != 0 {
		t.Fatalf("✘ 重复删除计数变负或残留: %d", m.Length())
	}
}

func TestShardedMap_LoadOrStore(t *testing.T) {
	m := NewShardedMap[int, string](4)
	if v, loaded := m.LoadOrStore(1, "first"); loaded || v != "first" {
		t.Fatalf("✘ 首次 LoadOrStore 结果不符: v=%v loaded=%v", v, loaded)
	}
	if v, loaded := m.LoadOrStore(1, "second"); !loaded || v != "first" {
		t.Fatalf("✘ 重复 LoadOrStore 覆盖了现值: v=%v loaded=%v", v, loaded)
	}
	if m.Length() != 1 {
		t.Fatalf("✘ LoadOrStore 计数不符: %d", m.Length())
	}
}

// TestShardedMap_ShardCountRounding 片数必须是 2 的幂：indexOf 用按位与代替取模，
// 一旦片数不是 2 的幂，mask 就会把键送到越界的片下标上。
func TestShardedMap_ShardCountRounding(t *testing.T) {
	cases := []struct{ want, got int }{
		{0, defShards},
		{-5, defShards},
		{1, 1},
		{3, 4},
		{16, 16},
		{17, 32},
		{5000, 1 << maxShardsLog},
	}
	for _, c := range cases {
		m := NewShardedMap[int, int](c.want)
		m.Store(1, 1)
		if len(m.shards) != c.got {
			t.Fatalf("✘ 片数取整不符: want=%d got=%d", c.want, len(m.shards))
		}
		// mask 与片数必须自洽，否则 indexOf 会越界
		for i := 0; i < 256; i++ {
			idx := m.indexOf(i)
			if idx >= uint64(len(m.shards)) {
				t.Fatalf("✘ 片下标越界: shards=%d idx=%d", len(m.shards), idx)
			}
		}
	}
}

func TestShardedMap_KeysSpreadAcrossShards(t *testing.T) {
	// 三档精度的键型都要真的散开：内置散列若只对 string 有效，整型 uid 表就退化成了单锁。
	assertSpread(t, "string", func(i int) string { return "uid-" + strconv.Itoa(i) })
	assertSpread(t, "int", func(i int) int { return i })
	assertSpread(t, "struct", func(i int) keyed { return keyed{UID: int64(i), Name: "n"} })
}

// assertSpread 用同一套键分别写入、复算片下标、再逐项读回：
// 散布过窄说明散列退化，读不回说明「同一个键两次算出不同片」。
func assertSpread[K comparable](t *testing.T, name string, key func(i int) K) {
	t.Helper()
	const (
		shards = 64
		count  = 1000
	)
	m := NewShardedMap[K, int](shards)
	for i := 0; i < count; i++ {
		m.Store(key(i), i)
	}
	used := make(map[uint64]bool, shards)
	for i := 0; i < count; i++ {
		idx := m.indexOf(key(i))
		used[idx] = true
		if v, ok := m.Load(key(i)); !ok || v != i {
			t.Fatalf("✘ %s 键读不回来（同键两次落到不同片）: i=%d v=%v ok=%v", name, i, v, ok)
		}
	}
	if len(used) < shards/2 {
		t.Fatalf("✘ %s 键分布过窄: %d 个键只落到 %d/%d 片", name, count, len(used), shards)
	}
}

func TestShardedMap_CustomHashControlsPlacement(t *testing.T) {
	// 常数散列：全部落在片 0。正确性必须照旧，只是没有并行度——
	// 这条同时钉住「自定义散列优先于内置散列」。
	m := NewShardedMapFunc[int, int](16, func(int) uint64 { return 7 })
	for i := 0; i < 50; i++ {
		m.Store(i, i)
	}
	if m.Length() != 50 {
		t.Fatalf("✘ 常数散列下计数不符: %d", m.Length())
	}
	for i := 0; i < 50; i++ {
		if idx := m.indexOf(i); idx != 7&15 {
			t.Fatalf("✘ 自定义散列未被采用: idx=%d", idx)
		}
	}
	// 同一片也不能漏读漏删
	loaded := 0
	m.Range(func(k, v int) bool {
		loaded++
		return true
	})
	if loaded != 50 {
		t.Fatalf("✘ 单片表遍历条数不符: %d", loaded)
	}
}

func TestShardedMap_RangeCallbacksMayMutate(t *testing.T) {
	m := NewShardedMap[int, int](8)
	for i := 0; i < 100; i++ {
		m.Store(i, i)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Range(func(k, v int) bool {
			m.Delete(k)
			m.Store(k+1000, v)
			return true
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("✘ Range 回调内改表死锁（快照必须在锁外回调）")
	}
	if m.Length() != 100 {
		t.Fatalf("✘ 回调内改表后计数不符: %d", m.Length())
	}
}

// TestShardedMap_ClearAllStopsCallbackButClears 钉住两处语义：
// fn 返回 false 之后不再回调（跨片也算），以及名字里的 All——元素照样全部移除。
// Map.ClearAll 就是这个结果，替代者若悄悄变成「只删回调过的」会留下永远没人关的连接。
func TestShardedMap_ClearAllStopsCallbackButClears(t *testing.T) {
	m := NewShardedMap[int, int](8)
	for i := 0; i < 80; i++ {
		m.Store(i, i)
	}
	calls := 0
	m.ClearAll(func(k, v int) bool {
		calls++
		return calls < 10
	})
	if calls != 10 {
		t.Fatalf("✘ 回调未在返回 false 后停止: calls=%d", calls)
	}
	if m.Length() != 0 {
		t.Fatalf("✘ ClearAll 中途停止后仍有残留: %d", m.Length())
	}
	empty := true
	m.Range(func(k, v int) bool {
		empty = false
		return false
	})
	if !empty {
		t.Fatal("✘ 计数已归零但片内仍有元素")
	}
}

// TestShardedMap_ClearAllCallbackMayDeleteItself 回调在锁外跑，所以「回调里自己关掉客户端、
// 顺手把键删了」是合法写法。若回片删除不复核键还在，同一项会被扣两次计数，
// 表就永久带着负数跑下去。
func TestShardedMap_ClearAllCallbackMayDeleteItself(t *testing.T) {
	m := NewShardedMap[int, int](8)
	for i := 0; i < 40; i++ {
		m.Store(i, i)
	}
	m.ClearAll(func(k, v int) bool {
		m.Delete(k)
		return true
	})
	if m.Length() != 0 {
		t.Fatalf("✘ 回调自删后计数变负或残留: %d", m.Length())
	}
}

func TestShardedMap_ClearAndClearAllNil(t *testing.T) {
	m := NewShardedMap[string, int](4)
	for i := 0; i < 20; i++ {
		m.Store(strconv.Itoa(i), i)
	}
	m.ClearAll(nil)
	if m.Length() != 0 {
		t.Fatalf("✘ ClearAll(nil) 未清空计数: %d", m.Length())
	}
	for i := 0; i < 20; i++ {
		m.Store(strconv.Itoa(i), i)
	}
	m.Clear()
	if m.Length() != 0 {
		t.Fatalf("✘ Clear 未清空计数: %d", m.Length())
	}
	if _, ok := m.Load("3"); ok {
		t.Fatal("✘ Clear 后仍能读到旧键")
	}
}

func TestShardedMap_ListAndRangeBySort(t *testing.T) {
	m := NewShardedMap[int, int](8)
	for i := 1; i <= 20; i++ {
		m.Store(i, i)
	}
	if got := len(m.List(nil)); got != 20 {
		t.Fatalf("✘ List 条数不符: %d", got)
	}
	sorted := m.List(func(a, b int) bool { return a > b })
	for i, v := range sorted {
		if v != 20-i {
			t.Fatalf("✘ List 排序参数未生效: %v", sorted)
		}
	}
	seen := make([]int, 0, 20)
	m.RangeBySort(func(k, v int) bool {
		seen = append(seen, v)
		return true
	}, func(a, b int) bool { return a < b })
	for i, v := range seen {
		if v != i+1 {
			t.Fatalf("✘ RangeBySort 顺序不符: %v", seen)
		}
	}
	// sortFunc 为 nil 时退化为无序遍历，但条数不能变
	count := 0
	m.RangeBySort(func(k, v int) bool {
		count++
		return true
	}, nil)
	if count != 20 {
		t.Fatalf("✘ RangeBySort(nil) 退化不符: %d", count)
	}
}

// TestShardedMap_ConcurrentConsistency 并发增删查之后，计数必须与真实元素数一致：
// 分片表最容易出错的地方是「总数一把原子量、各片各持一张 map」两者脱钩。
func TestShardedMap_ConcurrentConsistency(t *testing.T) {
	const (
		workers  = 8
		perJob   = 500
		keySpace = 300
	)
	m := NewShardedMap[int, int](32)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perJob; i++ {
				k := (base*131 + i) % keySpace
				m.Store(k, k)
				_, _ = m.Load(k)
				m.LoadOrStore(k+keySpace, k)
				if i%3 == 0 {
					m.Delete(k)
				}
				m.Range(func(_, _ int) bool { return true })
			}
		}(w)
	}
	wg.Wait()

	actual := 0
	m.Range(func(k, v int) bool {
		actual++
		return true
	})
	if m.Length() != actual {
		t.Fatalf("✘ 并发后计数与片内元素脱钩: num=%d actual=%d", m.Length(), actual)
	}
	for k := 0; k < keySpace; k++ {
		if _, ok := m.Load(k + keySpace); !ok {
			t.Fatalf("✘ 并发后键丢失: %d", k+keySpace)
		}
	}
}
