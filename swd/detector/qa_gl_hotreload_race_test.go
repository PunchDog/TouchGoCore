package detector

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/swd/core"
	"touchgocore/swd/types/category"
)

// QA-GL: detector 热重载并发竞态（P1）与除零（P2）回归。
//
// 旧实现：OnWordsChanged 持 d.mu 就地 d.primary.Build(words)，而 Detect/Match
// 读取路径不取 d.mu —— ac.Build 直接换根并重建 children map，与并发遍历构成
// concurrent map read/write（Go runtime fatal，recover 拦不住）或读到半建状态。
// 新实现：构建全新实例后 atomic.Pointer 整体发布，读写无共享可变状态。

func newQaDetector(t *testing.T) *detector {
	t.Helper()
	opt := &core.SWDOptions{IgnoreCase: true}
	di, err := NewDetector(opt)
	if err != nil {
		t.Fatalf("创建检测器失败: %v", err)
	}
	d, ok := di.(*detector)
	if !ok {
		t.Fatalf("检测器类型异常: %T", di)
	}
	return d
}

func qaWords(seed string) map[string]category.Category {
	w := map[string]category.Category{
		"嫖娼":    category.Pornography,
		"赌博":    category.Gambling,
		"冰毒":    category.Drugs,
		seed:    category.None,
		"敏感词检测": category.None,
	}
	for i := 0; i < 64; i++ {
		w[fmt.Sprintf("%s填充词%d", seed, i)] = category.None
	}
	return w
}

func TestQaGL_ConcurrentDetectDuringHotRebuild(t *testing.T) {
	d := newQaDetector(t)

	var panics atomic.Int64
	var readers, writers sync.WaitGroup
	stop := make(chan struct{})
	deadline := time.Now().Add(800 * time.Millisecond)
	if testing.Short() {
		deadline = time.Now().Add(300 * time.Millisecond)
	}

	// 读方：持续 Detect/MatchAll（不持 d.mu）
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panics.Add(1)
					t.Errorf("读协程 panic: %v", rec)
				}
			}()
			texts := []string{
				"这是一段包含嫖娼的文本，用来持续触发 AC 遍历",
				"完全正常的文本，不含任何词库内容，只是拉长遍历路径",
				"赌博和冰毒混在一段较长文本里，覆盖 MatchAll 的多命中路径",
			}
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				d.Detect(texts[i%len(texts)])
				d.MatchAll(texts[i%len(texts)])
			}
		}(r)
	}

	// 写方：反复 OnWordsChanged 触发整表重建
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			defer func() {
				if rec := recover(); rec != nil {
					panics.Add(1)
					t.Errorf("重建协程 panic: %v", rec)
				}
			}()
			for i := 0; time.Now().Before(deadline); i++ {
				d.OnWordsChanged(qaWords(fmt.Sprintf("重建词表%d-%d", w, i%7)))
			}
		}(w)
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	if n := panics.Load(); n != 0 {
		t.Fatalf("并发期间发生 %d 次 panic", n)
	}

	// 重建后必须能检到「新词表」的词：证明发布的是新建实例
	d.OnWordsChanged(map[string]category.Category{"热重载新词": category.None})
	if !d.Detect("这句话里含有热重载新词啊") {
		t.Errorf("OnWordsChanged 后未检出新词表中的词")
	}
	if d.Detect("这句话只有赌博两个字") {
		t.Errorf("旧词表未随重建失效（赌博已不在新词表中）")
	}
}

func TestQaGL_MatchAllNoDivideByZeroAfterWordsCleared(t *testing.T) {
	d := newQaDetector(t)

	// 词库清空：avgWordLength 归 0，旧实现 MatchAll 里
	// len(processedText)/int(d.avgWordLength) 直接 integer divide by zero panic
	d.OnWordsChanged(map[string]category.Category{})

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("词库清空后 MatchAll panic: %v", rec)
		}
	}()
	if got := d.MatchAll("任意一段文本，清空后不应命中也不应崩溃"); len(got) != 0 {
		t.Errorf("清空词库后仍命中: %v", got)
	}
	if d.Detect("任意文本") {
		t.Errorf("清空词库后 Detect 应为 false")
	}
}
