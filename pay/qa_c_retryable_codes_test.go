package pay

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// C-F4 用例：RetryableCodes 的带锁注册/查询/撤销三入口，以及并发读写不再撞
// 「map 并发读写 runtime fatal」的雷。裸 map 导出仅为禁改通道包测试的编译兼容保留。

// TestRetryableCodesLockedAccessors 三入口的基本语义：登记生效、空串跳过、撤销生效。
func TestRetryableCodesLockedAccessors(t *testing.T) {
	if IsRetryableCode("429") {
		t.Fatal("白名单默认必须为空")
	}
	SetRetryableCodes("429", "", "504")
	if !IsRetryableCode("429") || !IsRetryableCode("504") {
		t.Error("登记后应可查到")
	}
	if IsRetryableCode("") {
		t.Error("空串不得登记进白名单")
	}
	DeleteRetryableCode("429")
	DeleteRetryableCode("504")
	if IsRetryableCode("429") || IsRetryableCode("504") {
		t.Error("撤销后不应再查到")
	}
	// 重复登记/撤销不存在的码都不该 panic。
	SetRetryableCodes("504")
	SetRetryableCodes("504")
	DeleteRetryableCode("504")
	DeleteRetryableCode("never-registered")
	if IsRetryableCode("504") {
		t.Error("收尾撤销失败")
	}
}

// TestRetryableCodesConcurrentAccess 运行期登记与解析期查询并发跑：
// 修复前解析读的是裸 map，与写并发即 fatal concurrent map read/write（不可 recover）。
func TestRetryableCodesConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := strconv.Itoa(1000 + i)
			for {
				select {
				case <-stop:
					return
				default:
				}
				SetRetryableCodes(code)
				IsRetryableCode(code)
				DeleteRetryableCode(code)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			IsRetryableCode("1000")
			IsRetryableCode("250008")
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	// 收尾清白名单，别影响「默认为空」的既有钉桩。
	for i := 0; i < 4; i++ {
		DeleteRetryableCode(strconv.Itoa(1000 + i))
	}
	retryableCodesMu.RLock()
	n := len(RetryableCodes)
	retryableCodesMu.RUnlock()
	if n != 0 {
		t.Fatalf("并发用例收尾后白名单应回到空表，实得 %d 项", n)
	}
}
