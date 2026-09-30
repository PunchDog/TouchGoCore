package list

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// QA-GL: Range 延迟删除清理窗口（use-after-free 到对象池）压测。
//
// 旧实现的窗口：Range A 的 defer 里 rangeCount.Add(-1)==0 之后、取得 mu 之前，
// 新 Range B 可能已 Add(1) 并在 RLock 下把仍挂在链上的 delPending 节点收进快照；
// A 随后把这些节点 removeNodeLocked(n, true) 归还对象池（data/nodeType 置 nil），
// B 手里的快照即指向已归还节点 —— 回调里 GetData() 读到 nil，或该节点被
// NewNode 复用后读到别人的数据。
//
// 修复后的不变量：cleanup 在持 mu 时复查 rangeCount，仍为 0 才归还池；否则
// 保留 rangeDelList 交给最后退出的 Range 清理。任何 Range 持有快照期间，
// 快照里的节点绝不会被归还池，回调读到的 data 必然仍是入链时的 *qaGLItem。
//
// 编队说明（无 gcc / -race 不可用，靠结构放大窗口）：
//   - 快读方 R1 持续 Range：每次退出都可能把计数打到 0、触发清理路径；
//   - 慢读方 R2 周期性发起「每节点 1ms」的长 Range：一旦它的快照落在
//     R1 的「计数归 0 → 取得 mu」窗口内，旧实现必在 R2 遍历中途归还节点，
//     R2 读到 data==nil 即复现；修复后 R1 复查到 R2 仍活跃，跳过清理。
//   - 写方持续删除（Range 期间进 delPending）并补新节点（驱动对象池复用）。

type qaGLItem struct {
	serial int64
}

func TestQaGL_RangeCleanupNoUseAfterFree(t *testing.T) {
	const nodeCount = 64

	l := NewList()
	var serial atomic.Int64
	for i := 0; i < nodeCount; i++ {
		l.Add(NewNode(&qaGLItem{serial: serial.Add(1)}, nil))
	}

	var anomalies, panics atomic.Int64
	var wg sync.WaitGroup
	dur := 1500 * time.Millisecond
	if testing.Short() {
		dur = 800 * time.Millisecond
	}
	deadline := time.Now().Add(dur)

	check := func(n INode) bool {
		d := n.GetData()
		if d == nil {
			// 归还池前 removeNodeLocked 会把 data 置 nil：
			// 遍历中读到 nil 即命中 use-after-free 窗口
			anomalies.Add(1)
			return true
		}
		if _, ok := d.(*qaGLItem); !ok {
			anomalies.Add(1)
		}
		return true
	}
	guard := func(fn func()) {
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
			}
		}()
		fn()
	}

	// R1 快读方
	wg.Add(1)
	go guard(func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			l.Range(check)
		}
	})

	// R2 慢读方：长遍历放大「快照在手」的时间
	wg.Add(1)
	go guard(func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			l.Range(func(n INode) bool {
				time.Sleep(time.Millisecond)
				return check(n)
			})
			time.Sleep(20 * time.Millisecond)
		}
	})

	// 写方：删除（进 delPending）+ 补新节点（池复用）
	wg.Add(1)
	go guard(func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			if n := l.Head(); n != nil {
				n.Remove()
			}
			l.Add(NewNode(&qaGLItem{serial: serial.Add(1)}, nil))
			runtime.Gosched()
		}
	})

	wg.Wait()

	if n := panics.Load(); n != 0 {
		t.Fatalf("压测期间发生 %d 次 panic", n)
	}
	if n := anomalies.Load(); n != 0 {
		t.Fatalf("Range 快照读到已归还池的节点 %d 次（use-after-free 窗口复现）", n)
	}
}
