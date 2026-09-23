package pay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePay 是一个可配置响应序列的假供应商：按次序吐 resp，耗尽后一直吐最后一个。
type fakePay struct {
	resp     []func(w http.ResponseWriter, r *http.Request)
	calls    atomic.Int32
	lastSign atomic.Value // string
}

func newFakePay(t *testing.T, f *fakePay, maxRetry int, timeout time.Duration) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.calls.Add(1)) - 1
		f.lastSign.Store(r.Header.Get("X-Sign"))
		switch {
		case n < len(f.resp):
			f.resp[n](w, r)
		case len(f.resp) > 0:
			f.resp[len(f.resp)-1](w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{Name: "test", BaseURL: srv.URL, MaxRetry: maxRetry, Timeout: timeout, BackoffBase: time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func okBody() []byte { return []byte(`{"amount":1000}`) }

// TestDoSuccess 正常链路：签名注入点被调用、签名头送达、响应体原样透出。
func TestDoSuccess(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			got, _ := io.ReadAll(r.Body)
			_, _ = w.Write(got)
		},
	}}
	c := newFakePay(t, f, 0, time.Second)
	var signed atomic.Int32
	spec := CallSpec{
		Method: http.MethodPost,
		Path:   "/login",
		Body:   okBody(),
		Sign: func(req *http.Request, body []byte) error {
			signed.Add(1)
			if len(body) == 0 {
				t.Error("Sign 收到空 body")
			}
			req.Header.Set("X-Sign", "deadbeef")
			return nil
		},
	}
	out, err := c.Do(context.Background(), spec)
	if err != nil {
		t.Fatalf("Do 失败: %v", err)
	}
	if signed.Load() != 1 {
		t.Fatalf("Sign 调用次数=%d，期望 1", signed.Load())
	}
	if got := f.lastSign.Load().(string); got != "deadbeef" {
		t.Fatalf("服务端收到签名头=%q，期望 deadbeef", got)
	}
	if string(out) != string(okBody()) {
		t.Fatalf("响应载荷=%s，期望 %s", out, okBody())
	}
}

// TestDoRetryThenSuccess 429 重试到成功：请求次数在 maxRetry+1 以内。
func TestDoRetryThenSuccess(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"success"}`)) },
	}}
	c := newFakePay(t, f, 2, 2*time.Second)
	body := []byte(`{"order_no":"ORD-1","amount":1000}`)
	out, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: body})
	if err != nil {
		t.Fatalf("重试用尽前应当成功: %v", err)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("请求次数=%d，期望 3", got)
	}
	if !strings.Contains(string(out), "success") {
		t.Fatalf("响应=%s", out)
	}
}

// TestDoRetryExhausted 持续 5xx 时按 maxRetry 上限停下，错误可判定为重试用尽的供应商错误。
func TestDoRetryExhausted(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
	}}
	c := newFakePay(t, f, 2, 2*time.Second)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody()})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !IsProviderRetryable(err) {
		t.Fatalf("5xx 应标记为可重试，实得 %v", err)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("请求次数=%d，期望 3（首次 + 2 次重试）", got)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("错误类型/状态码不符: %+v", err)
	}
}

// TestDoNoRetryOn4xx 400 属确定性失败，重发只是重复挨拒，必须只发一次。
func TestDoNoRetryOn4xx(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadRequest) },
	}}
	c := newFakePay(t, f, 3, 2*time.Second)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody()})
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if IsProviderRetryable(err) {
		t.Fatalf("4xx 不得可重试: %v", err)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("请求次数=%d，期望 1", got)
	}
}

// TestDoTimeout 供应商挂住时按超时收口，且超时属可重试（区别于调用方取消）。
func TestDoTimeout(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		},
	}}
	c := newFakePay(t, f, 0, 20*time.Millisecond)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody()})
	if err == nil {
		t.Fatal("超时应当报错")
	}
	if !IsProviderRetryable(err) {
		t.Fatalf("单次请求超时应可重试，实得 %v", err)
	}
	var pe *ProviderError
	if errors.As(err, &pe) && pe.Code != "transport" {
		t.Fatalf("错误码=%q，期望 transport", pe.Code)
	}
}

// TestDoParentCtxCanceled 调用方取消后不得继续重发：上层已经不关心这一单了。
func TestDoParentCtxCanceled(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(150 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		},
	}}
	c := newFakePay(t, f, 3, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.Do(ctx, CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody()})
	if err == nil {
		t.Fatal("取消应当报错")
	}
	if IsProviderRetryable(err) {
		t.Fatalf("调用方取消不得可重试: %v", err)
	}
	if got := f.calls.Load(); got > 2 {
		t.Fatalf("取消后仍在重发，请求次数=%d", got)
	}
}

// TestParseBusinessError 业务包络：HTTP 200 + 业务失败码，由自定义 Parse 决定可重试性。
func TestParseBusinessError(t *testing.T) {
	f := &fakePay{resp: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":"250008","msg":"余额不足"}`))
		},
	}}
	c := newFakePay(t, f, 3, 2*time.Second)
	bizParse := func(status int, hdr http.Header, body []byte) (json.RawMessage, error) {
		var env struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, &ProviderError{Channel: "test", Code: "bad_json", Msg: err.Error()}
		}
		if env.Code != "0" {
			return nil, &ProviderError{Channel: "test", Code: env.Code, Msg: env.Msg}
		}
		return json.RawMessage(body), nil
	}
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody(), Parse: bizParse})
	if err == nil {
		t.Fatal("业务失败码应当报错")
	}
	if IsProviderRetryable(err) {
		t.Fatalf("余额不足不得重试: %v", err)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("请求次数=%d，期望 1", got)
	}
	if strings.Contains(err.Error(), "1000") {
		t.Fatalf("错误文案泄漏了请求体: %v", err)
	}
}

// TestNewClientGuards 基址为空/超时可用的归一化：缺省值与错误路径。
func TestNewClientGuards(t *testing.T) {
	if _, err := NewClient(Options{Name: "x", BaseURL: "  "}); err == nil {
		t.Fatal("空 base_url 应当构造失败")
	}
	c, err := NewClient(Options{Name: "x", BaseURL: "https://pay.example.com///", MaxRetry: -1})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.BaseURL() != "https://pay.example.com" {
		t.Fatalf("基址未归一: %s", c.BaseURL())
	}
	if c.timeout != DefaultTimeout || c.maxRetry != DefaultMaxRetry {
		t.Fatalf("缺省值未归一: timeout=%v maxRetry=%d", c.timeout, c.maxRetry)
	}
}

// TestRetryAfterParsed 服务端给了 Retry-After 就至少等那么久；非法值按 0 处理。
func TestRetryAfterParsed(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"2", 2 * time.Second},
		{" 3 ", 3 * time.Second},
		{"0", 0},
		{"-1", 0},
		{"abc", 0},
		{"", 0},
	}
	for _, cs := range cases {
		hdr := http.Header{}
		hdr.Set("Retry-After", cs.in)
		if got := retryAfterOf(hdr); got != cs.want {
			t.Errorf("Retry-After=%q 解析成 %v，期望 %v", cs.in, got, cs.want)
		}
	}
}

// TestOrderSerializesAsSnakeCase 报文必须是供应商惯用的蛇形字段 + 整数金额：
// 缺 json tag 会直接透出 Go 字段名，而浮点金额会在元/分换算上丢精度。
func TestOrderSerializesAsSnakeCase(t *testing.T) {
	b, err := json.Marshal(&PayOrder{OrderNo: "O1", Amount: 1_000_000, Currency: CurrencyUSDT})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, ".") {
		t.Fatalf("金额出现浮点形态: %s", got)
	}
	for _, want := range []string{`"order_no":"O1"`, `"amount":1000000`, `"currency":"USDT"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("报文缺 %s，实得 %s", want, got)
		}
	}
	// 未填的可选字段不得以空串或 null 出现，否则供应商会当成显式赋值。
	for _, bad := range []string{`"network"`, `"address"`, `"phone"`, `"code"`, `"extra"`} {
		if strings.Contains(got, bad) {
			t.Fatalf("空可选字段仍进报文 %s: %s", bad, got)
		}
	}
}

// TestProviderErrorNoCredential 错误文案只含通道名与业务码，凭证不得顺带出去。
func TestProviderErrorNoCredential(t *testing.T) {
	e := &ProviderError{Channel: "whatsapp", Code: "401002", Msg: "签名校验失败"}
	got := e.Error()
	for _, bad := range []string{"SecretKey", "secret", "token", "AppID"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
			t.Fatalf("错误文案含敏感字样 %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "401002") {
		t.Fatalf("错误文案丢了错误码: %s", got)
	}
}

// TestIsFinal 只有 success/failed 是终态；pending 与 unknown 都必须留给对账。
func TestIsFinal(t *testing.T) {
	for s, want := range map[string]bool{
		StatusSuccess: true, StatusFailed: true,
		StatusPending: false, StatusUnknown: false, "": false,
	} {
		if got := IsFinal(s); got != want {
			t.Errorf("IsFinal(%q)=%v，期望 %v", s, got, want)
		}
	}
	if (*PayResult)(nil).IsFinal() {
		t.Fatal("nil 回执不得判为终态")
	}
}
