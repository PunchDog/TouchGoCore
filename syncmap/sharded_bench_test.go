package syncmap

// 阶段13（S69）分片收益计量。
//
// 对照必须同形状：同一套键、同一个操作序列、同样的并行度，只换表的实现。
// 一律用 b.RunParallel 而不是单协程循环——单协程下两把锁都不竞争，测出来的差异
// 只是多一次按位与，而真正要消掉的是几十个读协程挤在同一处字上的缓存行失效。

import (
	"strconv"
	"testing"
)

const (
	benchKeys    = 4096 // 键空间：与在线客户端量级同阶
	benchKeyStep = 7    // 每次操作推进的步长，与片数互质，让相邻协程不散落在同一片上
)

// benchStringKeys 预生成键，把 strconv 的开销留在计时区外。
func benchStringKeys() []string {
	ks := make([]string, benchKeys)
	for i := range ks {
		ks[i] = "client-" + strconv.Itoa(i)
	}
	return ks
}

func BenchmarkParallelMap_ReadHeavy(b *testing.B) {
	ks := benchStringKeys()
	m := NewMap[string, int]()
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.Load(ks[i])
		}
	})
}

func BenchmarkParallelSharded_ReadHeavy(b *testing.B) {
	ks := benchStringKeys()
	m := NewShardedMap[string, int](0)
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.Load(ks[i])
		}
	})
}

// 读写混合：每 8 次读穿插一次覆盖写（对应「收到消息查表 + 顺带更新时间戳」）。
func BenchmarkParallelMap_Mixed(b *testing.B) {
	ks := benchStringKeys()
	m := NewMap[string, int]()
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			m.Store(ks[i], i)
		}
	})
}

func BenchmarkParallelSharded_Mixed(b *testing.B) {
	ks := benchStringKeys()
	m := NewShardedMap[string, int](0)
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			_, _ = m.Load(ks[i])
			m.Store(ks[i], i)
		}
	})
}

// 增删 churn：连接建立/断开路径（Store + Delete + LoadOrStore 交替）。
func BenchmarkParallelMap_Churn(b *testing.B) {
	ks := benchStringKeys()
	m := NewMap[string, int]()
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.LoadOrStore(ks[i], i)
			m.Delete(ks[i])
			m.Store(ks[i], i)
		}
	})
}

func BenchmarkParallelSharded_Churn(b *testing.B) {
	ks := benchStringKeys()
	m := NewShardedMap[string, int](0)
	for i, k := range ks {
		m.Store(k, i)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i = (i + benchKeyStep) % benchKeys
			_, _ = m.LoadOrStore(ks[i], i)
			m.Delete(ks[i])
			m.Store(ks[i], i)
		}
	})
}

// 遍历是分片表的弱项（逐片快照、每片各分配一次），一并量出来，别只报好看的那半。
func BenchmarkMap_Range1000(b *testing.B) { benchmarkRangeMap(b, false) }
func BenchmarkSharded_Range1000(b *testing.B) {
	benchmarkRangeMap(b, true)
}

func benchmarkRangeMap(b *testing.B, sharded bool) {
	ks := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		ks = append(ks, "client-"+strconv.Itoa(i))
	}
	sm := NewShardedMap[string, int](0)
	m := NewMap[string, int]()
	for i, k := range ks {
		if sharded {
			sm.Store(k, i)
		} else {
			m.Store(k, i)
		}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if sharded {
			sm.Range(func(k string, v int) bool { return true })
		} else {
			m.Range(func(k string, v int) bool { return true })
		}
	}
}
