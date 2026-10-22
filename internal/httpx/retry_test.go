package httpx

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// 本文件是 RetryableCodes 的行为用例，从原 pay/qa_c_retryable_codes_test.go 迁来。
//
// 迁移时多做了一件事：把「默认为空」这条从 pay 的 provider_test.go 也搬了过来。
// 它是全套资金安全判据的地基——业务码一旦默认等于「可原单重发」，
// 所有 5xx、所有说不清的受理结论都会被重发一遍。

// TestRetryableCodesEmptyByDefault 业务码默认可重试性必须是「不可重试」。
// 哪天有人往表里塞了个出款相关码，这条测试会逼他写下理由。
func TestRetryableCodesEmptyByDefault(t *testing.T) {
	if n := LenRetryableCodes(); n != 0 {
		t.Fatalf("RetryableCodes 已登记 %d 项，确认每个码都可原单重发后再提交", n)
	}
}

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
	if n := LenRetryableCodes(); n != 2 {
		t.Errorf("登记后长度=%d，期望 2（空串不该占位）", n)
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
	if n := LenRetryableCodes(); n != 0 {
		t.Errorf("收尾后白名单应回到空表，实得 %d 项", n)
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
				LenRetryableCodes()
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
	if n := LenRetryableCodes(); n != 0 {
		t.Fatalf("并发用例收尾后白名单应回到空表，实得 %d 项", n)
	}
}
