package dictionary

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/swd/types/category"
)

// QA-GL: Loader.Clear 不再整体重新赋值 l.words（sync.Map 值拷贝写）。
//
// 旧实现 `l.words = sync.Map{}` 与并发 GetWords 的 Range、AddWord/RemoveWord 的
// Store/Delete 竞争，且旧 map 上的在途操作全部落在被丢弃的实例上。
// 新实现保持同一个 sync.Map 实例，Range 出 key 逐个 Delete。
// 竞态窗口无 -race 不可确定性复现，这里用并发压测 + 清空后契约断言佐证。

func TestQaGL_ClearKeepsSameMapAndConcurrentSafe(t *testing.T) {
	l := NewLoader()
	if err := l.LoadDefaultWords(context.Background()); err != nil {
		t.Fatalf("加载默认词库失败: %v", err)
	}
	if len(l.GetWords()) == 0 {
		t.Fatal("默认词库为空")
	}

	var panics atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	dur := 500 * time.Millisecond
	if testing.Short() {
		dur = 200 * time.Millisecond
	}

	// 并发读
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panics.Add(1)
				}
			}()
			for {
				select {
				case <-stop:
					return
				default:
					_ = l.GetWords()
				}
			}
		}()
	}
	// 并发写
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
			}
		}()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				_ = l.AddWord("压测词", category.None)
				_ = l.RemoveWord("压测词")
			}
		}
	}()
	// 并发清空
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				panics.Add(1)
			}
		}()
		for {
			select {
			case <-stop:
				return
			default:
				_ = l.Clear()
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	time.Sleep(dur)
	close(stop)
	wg.Wait()

	if n := panics.Load(); n != 0 {
		t.Fatalf("并发 Clear/GetWords/AddWord 期间发生 %d 次 panic", n)
	}

	// 契约：清空后词库为空，且同一个 loader 仍可继续使用（实例未被换掉）
	if err := l.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := len(l.GetWords()); got != 0 {
		t.Fatalf("Clear 后仍有 %d 个词", got)
	}
	if err := l.AddWord("清空后新增", category.None); err != nil {
		t.Fatalf("Clear 后 AddWord: %v", err)
	}
	if _, ok := l.GetWords()["清空后新增"]; !ok {
		t.Fatal("Clear 后 AddWord 未生效")
	}
}
