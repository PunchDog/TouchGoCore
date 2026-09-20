package syncmap

import (
	"sync"
	"testing"
	"time"
)

// TestRangeCallbackMayDelete 回归：Range 曾在持有读锁时执行回调，
// 回调内 Delete 需写锁 → 读锁不可重入，直接死锁（websocket 关服链路已实际触发）。
func TestRangeCallbackMayDelete(t *testing.T) {
	m := NewMap[int, string]()
	for i := 0; i < 100; i++ {
		m.Store(i, "v")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Range(func(k int, v string) bool {
			m.Delete(k)
			return true
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Range with Delete in callback deadlocked")
	}

	if n := m.Length(); n != 0 {
		t.Fatalf("expected all entries deleted from callback, got %d", n)
	}
}

// TestRangeCallbackMayStoreAndRange 回调内再行存储与嵌套遍历同样不得死锁。
func TestRangeCallbackMayStoreAndRange(t *testing.T) {
	m := NewMap[int, string]()
	m.Store(1, "a")
	m.Store(2, "b")

	visited := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Range(func(k int, v string) bool {
			m.Store(k+100, v)
			m.Range(func(k2 int, v2 string) bool {
				visited++
				return false // 提前中断也不应死锁
			})
			return true
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("nested Range / Store in callback deadlocked")
	}
	if visited == 0 {
		t.Fatalf("nested Range never invoked the callback")
	}
}

// TestRangeBySortCallbackMayDelete 排序遍历也必须支持遍历中改表。
func TestRangeBySortCallbackMayDelete(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 20; i++ {
		m.Store(i, i)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.RangeBySort(func(k, v int) bool {
			m.Delete(k)
			return true
		}, func(d1, d2 int) bool {
			return d1 > d2
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("RangeBySort with Delete in callback deadlocked")
	}
	if n := m.Length(); n != 0 {
		t.Fatalf("expected all deleted, got %d", n)
	}
}

// TestRangeBySortOrderPreserved 快照化后排序语义保持不变。
func TestRangeBySortOrderPreserved(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 10; i++ {
		m.Store(i, i)
	}

	var got []int
	m.RangeBySort(func(k, v int) bool {
		got = append(got, v)
		return true
	}, func(d1, d2 int) bool { return d1 < d2 })

	if len(got) != 10 {
		t.Fatalf("expected 10 visited, got %d", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("expected ascending order, got %v", got)
		}
	}
}

// TestRangeSnapshotStable 遍历期间的并发增删不影响本次快照的条目数，
// 也不会 panic（旧实现直接迭代活 map）。
func TestRangeSnapshotStable(t *testing.T) {
	m := NewMap[int, int]()
	const seed = 200
	for i := 0; i < seed; i++ {
		m.Store(i, i)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 1000
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.Store(i, i)
			m.Delete(i - 500)
			i++
		}
	}()

	for round := 0; round < 20; round++ {
		seen := make(map[int]bool, seed)
		m.Range(func(k, v int) bool {
			seen[k] = true
			return true
		})
		if len(seen) < seed/2 {
			close(stop)
			wg.Wait()
			t.Fatalf("range snapshot lost most entries: %d/%d", len(seen), seed)
		}
	}

	close(stop)
	wg.Wait()
}

// TestClearAllNilCallback 回归：ClearAll(nil) 曾直接调用空函数 → panic。
func TestClearAllNilCallback(t *testing.T) {
	m := NewMap[int, int]()
	m.Store(1, 1)
	m.Store(2, 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("ClearAll(nil) panicked: %v", r)
			}
		}()
		m.ClearAll(nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ClearAll(nil) deadlocked")
	}

	if n := m.Length(); n != 0 {
		t.Fatalf("expected map cleared, got %d entries", n)
	}
}

// TestClearAllEarlyStopStillClears 保持既有语义：回调返回 false 提前中止仍然清空全表。
func TestClearAllEarlyStopStillClears(t *testing.T) {
	m := NewMap[int, int]()
	for i := 0; i < 10; i++ {
		m.Store(i, i)
	}

	called := 0
	m.ClearAll(func(k, v int) bool {
		called++
		return false
	})

	if called != 1 {
		t.Fatalf("expected callback invoked once before stopping, got %d", called)
	}
	if n := m.Length(); n != 0 {
		t.Fatalf("expected map cleared, got %d entries", n)
	}
}

// TestMapAnyInheritsFixes MapAny 通过内嵌复用同一实现，死锁修复必须同样生效。
func TestMapAnyInheritsFixes(t *testing.T) {
	m := NewAny()
	m.Store("a", 1)
	m.Store("b", 2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Range(func(k, v any) bool {
			m.Delete(k)
			return true
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("MapAny.Range with Delete deadlocked")
	}
	if n := m.Length(); n != 0 {
		t.Fatalf("expected MapAny cleared, got %d", n)
	}
}
