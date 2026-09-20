package list

import (
	"sync"
	"testing"
	"time"
)

// runWithin 在 d 内执行 fn，超时视为挂死
func runWithin(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer func() { close(done) }()
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s 未在 %v 内结束（疑似挂死或锁泄漏）", name, d)
	}
}

// TestList_ClearDuringRangeNoPoolAlias 回归（M10）：Range 期间 Clear 曾把仍在
// 遍历快照里的节点归还节点池，下一个 NewNode 立刻取到同一块内存 —— 遍历方还没
// 走完的节点，业务字段与前后指针已被别人改写。
func TestList_ClearDuringRangeNoPoolAlias(t *testing.T) {
	l := NewList()
	const total = 8
	for i := 0; i < total; i++ {
		if !l.Add(NewNode(i, nil)) {
			t.Fatalf("Add failed at %d", i)
		}
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var held []*Node

	go func() {
		first := true
		l.Range(func(node INode) bool {
			if first {
				// 快照已生成：此刻记录全部节点指针，模拟「遍历方仍在持有」
				for p := node; p != nil; p = p.GetNode().next {
					held = append(held, p.GetNode())
				}
				first = false
				close(entered)
				<-release
			}
			return true
		})
	}()

	runWithin(t, 10*time.Second, "Clear/NewNode", func() {
		<-entered
		l.Clear()
		fresh := NewNode("fresh", nil)
		// 遍历结束后池复用才是合法的；这里只关心「遍历未结束前」不得复用
		if fresh == nil {
			t.Error("NewNode 返回 nil")
			return
		}
		fp := fresh.GetNode()
		for _, p := range held {
			if p == fp {
				t.Errorf("Clear 把仍在遍历中的节点归还了节点池：复用同一 *Node=%p", fp)
				return
			}
		}
		if !l.Add(fresh) {
			t.Error("Clear 后新节点入链失败")
		}
	})
	close(release)
	time.Sleep(50 * time.Millisecond)

	if l.Length() != 1 {
		t.Fatalf("Clear 后应当只剩新节点，实际长度=%d", l.Length())
	}
}

// TestList_ClearRangeConcurrent 回归（M10/M11）：Add/Remove/Range/Clear 四路并发
// 混跑后，链表长度必须与 nodeMap 索引严格一致，且 ID 两两不同。
// 长度与索引的偏差正是「节点被池改写、幽灵条目残留、重复入链」的共同症状。
func TestList_ClearRangeConcurrent(t *testing.T) {
	l := NewList()
	const workers = 8
	const rounds = 200

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				node := NewNode(id*1000000+r, nil)
				if !l.Add(node) {
					t.Errorf("Add 失败: worker=%d round=%d", id, r)
					return
				}
				l.Range(func(n INode) bool {
					_ = n.GetId()
					return true
				})
				node.GetNode().Remove()
				if r%50 == 0 {
					l.Clear()
				}
			}
		}(w)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("并发混跑挂死，链表长度=%d", l.Length())
	}

	l.mu.RLock()
	mapLen := len(l.nodeMap)
	listLen := l.len
	l.mu.RUnlock()

	if mapLen != listLen {
		t.Fatalf("索引与链表长度漂移: nodeMap=%d len=%d", mapLen, listLen)
	}
	if listLen < 0 {
		t.Fatalf("链表长度为负: %d", listLen)
	}

	seen := make(map[int64]struct{}, listLen)
	l.Range(func(n INode) bool {
		id := n.GetId()
		if _, ok := seen[id]; ok {
			t.Errorf("遍历到重复 ID: %d", id)
		}
		seen[id] = struct{}{}
		return true
	})
	if len(seen) != listLen {
		t.Fatalf("遍历节点数与长度不一致: 唯一ID=%d len=%d", len(seen), listLen)
	}
}

// TestList_ReAddKeepsSingleIndex 回归（M14）：遍历期间 Remove 会挂起删除请求，
// 此时节点仍挂在旧 ID 上；随后重新 Add 换了新 ID，旧键必须一并摘掉，
// 否则 nodeMap 永久残留一个指向该节点的幽灵条目。
func TestList_ReAddKeepsSingleIndex(t *testing.T) {
	l := NewList()
	a := NewNode("a", nil)
	b := NewNode("b", nil)
	l.Add(a)
	l.Add(b)

	oldID := a.GetId()
	l.Range(func(n INode) bool {
		// 遍历中标记延迟删除：a 仍留在 nodeMap[oldID]
		a.GetNode().Remove()
		l.Add(a) // 同节点重新入链，取新 ID
		return true
	})

	l.mu.RLock()
	_, stale := l.nodeMap[oldID]
	mapLen := len(l.nodeMap)
	lenVal := l.len
	l.mu.RUnlock()

	if stale {
		t.Fatalf("重新入链后旧 ID %d 仍留在 nodeMap 中（幽灵条目）", oldID)
	}
	if mapLen != lenVal {
		t.Fatalf("索引与链表长度不一致: nodeMap=%d len=%d", mapLen, lenVal)
	}
	if l.Get(a.GetId()) != a {
		t.Fatalf("新 ID %d 查不到重新入链的节点", a.GetId())
	}
}
