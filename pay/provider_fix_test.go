package pay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task #18: 回执金额校验——仅当 res.Amount 非零时才校验与订单金额一致
// ---------------------------------------------------------------------------

// TestPlaceOrderAmountValidation 表驱动：回执省略金额通过、金额一致通过、金额不一致拒绝。
func TestPlaceOrderAmountValidation(t *testing.T) {
	order := &PayOrder{OrderNo: "ORD-100", Amount: 5000, Currency: CurrencyUSDT}
	cases := []struct {
		name      string
		resp      string
		wantOK    bool
		wantErrSub string
	}{
		{
			name:   "回执省略金额（amount=0）通过",
			resp:   `{"code":"0","data":{"order_no":"ORD-100","status":"success","amount":0}}`,
			wantOK: true,
		},
		{
			name:   "回执不含 amount 字段通过",
			resp:   `{"code":"0","data":{"order_no":"ORD-100","status":"success"}}`,
			wantOK: true,
		},
		{
			name:   "金额一致通过",
			resp:   `{"code":"0","data":{"order_no":"ORD-100","status":"success","amount":5000}}`,
			wantOK: true,
		},
		{
			name:       "金额不一致拒绝（偏小）",
			resp:       `{"code":"0","data":{"order_no":"ORD-100","status":"success","amount":4999}}`,
			wantErrSub: "金额",
		},
		{
			name:       "金额不一致拒绝（偏大）",
			resp:       `{"code":"0","data":{"order_no":"ORD-100","status":"success","amount":5001}}`,
			wantErrSub: "金额",
		},
		{
			name:   "非终态（pending）省略金额通过",
			resp:   `{"code":"0","data":{"order_no":"ORD-100","status":"pending"}}`,
			wantOK: true,
		},
		{
			name:   "非终态（pending）金额不一致也通过",
			resp:   `{"code":"0","data":{"order_no":"ORD-100","status":"pending","amount":1}}`,
			wantOK: true,
		},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			r := newSDKServer(t)
			r.resp.Store(cs.resp)
			ch, err := Open(DriverGeneric, ProviderOptions{
				Name: "usdt", BaseURL: r.srv.URL, SecretKey: "s",
				Timeout: 2 * time.Second, Endpoint: endpoints(),
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := ch.Recharge(context.Background(), order)
			if cs.wantOK {
				if err != nil {
					t.Fatalf("期望成功，实得错误: %v", err)
				}
				if res == nil {
					t.Fatal("期望成功，实得 nil 回执")
				}
				return
			}
			if err == nil {
				t.Fatalf("期望报错含 %q，实得成功: %+v", cs.wantErrSub, res)
			}
			if !contains(err.Error(), cs.wantErrSub) {
				t.Fatalf("错误文案应含 %q，实得: %v", cs.wantErrSub, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Task #19: Withdraw 重试白名单——仅白名单内状态码可重试
// ---------------------------------------------------------------------------

// TestWithdrawRetryWhitelist Rejects5xx 验证 Withdraw 路径 5xx 不再默认 Retryable。
func TestWithdrawRetryWhitelistRejects5xx(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Name: "usdt", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// parseStrict 模拟 Withdraw 路径的解析行为
	for _, status := range []int{500, 502, 503, 504} {
		_, perr := p.parseStrict(status, nil, json.RawMessage(`{}`))
		var pe *ProviderError
		if !errors.As(perr, &pe) {
			t.Fatalf("status=%d: 期望 ProviderError，实得 %v", status, perr)
		}
		if pe.Retryable {
			t.Errorf("status=%d: Withdraw 路径白名单为空时不应可重试", status)
		}
		if pe.HTTPStatus != status {
			t.Errorf("status=%d: HTTPStatus 未保留", status)
		}
	}
}

// TestWithdrawRetryWhitelistAllowsRegisteredCode 验证白名单含某码时该码可重试。
func TestWithdrawRetryWhitelistAllowsRegisteredCode(t *testing.T) {
	// 临时注册 502 到白名单
	code := strconv.Itoa(http.StatusBadGateway)
	RetryableCodes[code] = struct{}{}
	defer delete(RetryableCodes, code)

	p, err := NewProvider(ProviderOptions{
		Name: "usdt", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// 502 在白名单中 → 可重试
	_, perr := p.parseStrict(http.StatusBadGateway, nil, json.RawMessage(`{}`))
	var pe *ProviderError
	if !errors.As(perr, &pe) {
		t.Fatalf("期望 ProviderError，实得 %v", perr)
	}
	if !pe.Retryable {
		t.Error("502 已登记白名单，Withdraw 路径应可重试")
	}
	// 500 不在白名单 → 不可重试
	_, perr2 := p.parseStrict(http.StatusInternalServerError, nil, json.RawMessage(`{}`))
	var pe2 *ProviderError
	if !errors.As(perr2, &pe2) {
		t.Fatalf("期望 ProviderError，实得 %v", perr2)
	}
	if pe2.Retryable {
		t.Error("500 未登记白名单，Withdraw 路径不应可重试")
	}
}

// TestDepositRetryBehaviorUnchanged 验证 Deposit/查询路径行为不变：5xx 仍可重试。
func TestDepositRetryBehaviorUnchanged(t *testing.T) {
	p, err := NewProvider(ProviderOptions{
		Name: "usdt", BaseURL: "https://x.example", SecretKey: "s",
		Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// parse (非 strict) 保持现状：5xx 和 429 可重试
	for _, status := range []int{429, 500, 502, 503} {
		_, perr := p.parse(status, nil, json.RawMessage(`{}`))
		var pe *ProviderError
		if !errors.As(perr, &pe) {
			t.Fatalf("status=%d: 期望 ProviderError，实得 %v", status, perr)
		}
		if !pe.Retryable {
			t.Errorf("status=%d: Deposit 路径应保持可重试", status)
		}
	}
	// 4xx（非 429）仍不可重试
	_, perr := p.parse(http.StatusBadRequest, nil, json.RawMessage(`{}`))
	var pe *ProviderError
	if !errors.As(perr, &pe) {
		t.Fatalf("期望 ProviderError，实得 %v", perr)
	}
	if pe.Retryable {
		t.Error("400 不应可重试")
	}
}

// TestWithdrawEndToEndStrictRetry 端到端验证：Withdraw 遇 500 且白名单不含 500 时不重试。
func TestWithdrawEndToEndStrictRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "usdt", BaseURL: srv.URL, SecretKey: "s",
		Timeout: 2 * time.Second, MaxRetry: 3, Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	order := &PayOrder{OrderNo: "W-1", Amount: 1000, Currency: CurrencyUSDT, Address: "TAddr"}
	_, err = ch.Withdraw(context.Background(), order)
	if err == nil {
		t.Fatal("期望 Withdraw 报错")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("期望 ProviderError，实得 %T: %v", err, err)
	}
	if pe.Retryable {
		t.Error("Withdraw 500 白名单为空时不应标记可重试")
	}
	// MaxRetry=3 但因不可重试，只应调用一次
	if calls != 1 {
		t.Errorf("期望只请求 1 次（不可重试），实得 %d 次", calls)
	}
}

// TestDepositEndToEndRetries5xx 端到端验证：Deposit 遇 500 仍会重试（行为不变）。
func TestDepositEndToEndRetries5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`unavailable`))
	}))
	defer srv.Close()

	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "usdt", BaseURL: srv.URL, SecretKey: "s",
		Timeout: 2 * time.Second, MaxRetry: 2, Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	order := &PayOrder{OrderNo: "D-1", Amount: 500, Currency: CurrencyUSDT}
	_, err = ch.Recharge(context.Background(), order)
	if err == nil {
		t.Fatal("期望 Recharge 报错")
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("期望 ProviderError，实得 %T: %v", err, err)
	}
	if !pe.Retryable {
		t.Error("Deposit 503 应保持可重试")
	}
	// MaxRetry=2 → 首次 + 2 次重试 = 3 次请求
	if calls != 3 {
		t.Errorf("期望 3 次请求（1+2 重试），实得 %d 次", calls)
	}
}

// contains 是测试内部辅助，避免引入 strings 包的额外依赖。
func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && searchSubstring(s, sub)
}

func searchSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
