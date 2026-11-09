package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/pay"
)

// 本文件是传输层的行为用例，从原 pay/client_test.go 迁来。
//
// 迁移时只改了错误类型的包名（ProviderError/IsProviderRetryable 现在是 pay 的），
// 断言强度不变：重试次数、可重试性、超时与调用方取消的区分、Retry-After 的钳制，
// 每一条都直接对应一次「钱有没有可能被重复送出去」。

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
	if !pay.IsProviderRetryable(err) {
		t.Fatalf("5xx 应标记为可重试，实得 %v", err)
	}
	if got := f.calls.Load(); got != 3 {
		t.Fatalf("请求次数=%d，期望 3（首次 + 2 次重试）", got)
	}
	var pe *pay.ProviderError
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
	if pay.IsProviderRetryable(err) {
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
	if !pay.IsProviderRetryable(err) {
		t.Fatalf("单次请求超时应可重试，实得 %v", err)
	}
	var pe *pay.ProviderError
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
	if pay.IsProviderRetryable(err) {
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
			return nil, &pay.ProviderError{Channel: "test", Code: "bad_json", Msg: err.Error()}
		}
		if env.Code != "0" {
			return nil, &pay.ProviderError{Channel: "test", Code: env.Code, Msg: env.Msg}
		}
		return json.RawMessage(body), nil
	}
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/x", Body: okBody(), Parse: bizParse})
	if err == nil {
		t.Fatal("业务失败码应当报错")
	}
	if pay.IsProviderRetryable(err) {
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

// 本文件是「写路径不自动重发」的用例，从原 pay/qa_c_strict_transport_test.go
// 与 pay/provider_fix_test.go 的 Withdraw 严格口径迁来。
//
// 迁移后的落点变了，判据一条没减：原实现用 Provider.Withdraw 触发严格路径，
// 现在那两个入口都没了，改用 CallSpec 上成对的两个键复现同一件事——
// NoTransportRetry（transport/read_body 不重发）与 StrictParse（非 2xx 只认白名单）。
//
// 覆盖的资损窗口：请求已经写出、供应商可能已经受理，但响应没能完整读回
// （断连 / 超时 / 半个响应体）。修复前 attempt 层对这两类错误标 Retryable=true，
// Do 循环会原单重发最多 maxRetry 次——幂等不完备即双付。

// hijackCloseServer 收完请求体后劫持连接直接关闭、不回任何字节：
// 客户端在 http.Do 处拿到 transport 错误（EOF/连接重置）。
func hijackCloseServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("测试服务器不支持 hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// partialBodyServer 回一个 Content-Length 与实际写出量不符的 200 响应后断开：
// 状态行与头都到了，读体在 io.ReadAll 处失败 → read_body 错误。
func partialBodyServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("测试服务器不支持 hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"code\":\"0\""))
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTransportTestClient 构造指向假供应商的客户端；maxRetry/timeout 按用例给。
func newTransportTestClient(t *testing.T, url string, maxRetry int, timeout time.Duration) *Client {
	t.Helper()
	c, err := NewClient(Options{
		Name: "usdt", BaseURL: url,
		Timeout: timeout, MaxRetry: maxRetry, BackoffBase: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// assertNoResendStrict 断言严格路径的错误形态：ProviderError、指定 code、Retryable=false、只发一次。
func assertNoResendStrict(t *testing.T, err error, wantCode string, calls int32, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望报错，实得成功", op)
	}
	var pe *pay.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: 期望 ProviderError，实得 %T: %v", op, err, err)
	}
	if pe.Code != wantCode {
		t.Errorf("%s: 错误码=%q，期望 %q", op, pe.Code, wantCode)
	}
	if pe.Retryable {
		t.Errorf("%s: 严格路径 %s 错误不得标记可重试", op, wantCode)
	}
	if calls != 1 {
		t.Errorf("%s: 期望只发 1 次请求，实得 %d 次（自动重发=双付窗口）", op, calls)
	}
}

// TestWritePathHijackCloseNeverResends 收单后断连：写路径只发一次；读路径仍可重试。
func TestWritePathHijackCloseNeverResends(t *testing.T) {
	var wcalls atomic.Int32
	wsrv := hijackCloseServer(t, &wcalls)
	c := newTransportTestClient(t, wsrv.URL, 2, 2*time.Second)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/pay", Body: []byte(`{}`), NoTransportRetry: true})
	assertNoResendStrict(t, err, "transport", wcalls.Load(), "写路径/断连")

	// 同形态错误在非严格路径（只读查询）保持可重试：查询无副作用，重发只是省一次人工对账。
	var rcalls atomic.Int32
	rsrv := hijackCloseServer(t, &rcalls)
	c2 := newTransportTestClient(t, rsrv.URL, 2, 2*time.Second)
	if _, err := c2.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/query", Body: []byte(`{}`)}); err == nil {
		t.Fatal("读路径: 期望报错")
	} else if !pay.IsProviderRetryable(err) {
		t.Errorf("读路径 transport 错误应保持可重试，实得 %v", err)
	}
	if n := rcalls.Load(); n != 3 {
		t.Errorf("读路径: 期望 3 次请求（首次+2 重试），实得 %d 次", n)
	}
}

// TestWritePathTimeoutNeverResends 供应商收单后不回包、客户端超时：写路径只发一次；
// 读路径仍按 maxRetry 重发。
func TestWritePathTimeoutNeverResends(t *testing.T) {
	hang := func(t *testing.T, calls *atomic.Int32) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"code":"0","msg":"ok"}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	var wcalls atomic.Int32
	wsrv := hang(t, &wcalls)
	c := newTransportTestClient(t, wsrv.URL, 2, 40*time.Millisecond)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/pay", Body: []byte(`{}`), NoTransportRetry: true})
	assertNoResendStrict(t, err, "transport", wcalls.Load(), "写路径/超时")

	var rcalls atomic.Int32
	rsrv := hang(t, &rcalls)
	c2 := newTransportTestClient(t, rsrv.URL, 1, 40*time.Millisecond)
	if _, err := c2.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/query", Body: []byte(`{}`)}); err == nil {
		t.Fatal("读路径: 期望报错")
	} else if !pay.IsProviderRetryable(err) {
		t.Errorf("读路径超时应保持可重试，实得 %v", err)
	}
	if n := rcalls.Load(); n != 2 {
		t.Errorf("读路径: 期望 2 次请求（首次+1 重试），实得 %d 次", n)
	}
}

// TestWritePathReadBodyErrorNeverResends 半个响应体（Content-Length 不符即断）：
// 写路径只发一次且错误码为 read_body；读路径仍可重试。
func TestWritePathReadBodyErrorNeverResends(t *testing.T) {
	var wcalls atomic.Int32
	wsrv := partialBodyServer(t, &wcalls)
	c := newTransportTestClient(t, wsrv.URL, 2, 2*time.Second)
	_, err := c.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/pay", Body: []byte(`{}`), NoTransportRetry: true})
	assertNoResendStrict(t, err, "read_body", wcalls.Load(), "写路径/断体")

	var rcalls atomic.Int32
	rsrv := partialBodyServer(t, &rcalls)
	c2 := newTransportTestClient(t, rsrv.URL, 1, 2*time.Second)
	if _, err := c2.Do(context.Background(), CallSpec{Method: http.MethodPost, Path: "/query", Body: []byte(`{}`)}); err == nil {
		t.Fatal("读路径: 期望报错")
	} else if !pay.IsProviderRetryable(err) {
		t.Errorf("读路径 read_body 错误应保持可重试，实得 %v", err)
	}
	if n := rcalls.Load(); n != 2 {
		t.Errorf("读路径: 期望 2 次请求（首次+1 重试），实得 %d 次", n)
	}
}

// TestStrictParseRejects5xxWithoutWhitelist 写路径的 5xx 一律不可重试，白名单为空时尤其如此。
//
// 这一条是双付与抖动的分界：出款请求发出之后，供应商回 5xx 既可能是没受理、
// 也可能是受理了而回执没穿回来——客户端无从分辨。前者重发省钱，后者重发就是第二笔钱。
func TestStrictParseRejects5xxWithoutWhitelist(t *testing.T) {
	parse := StrictParse("usdt")
	for _, status := range []int{500, 502, 503, 504} {
		_, err := parse(status, nil, []byte(`{}`))
		var pe *pay.ProviderError
		if !errors.As(err, &pe) {
			t.Fatalf("status=%d: 期望 ProviderError，实得 %v", status, err)
		}
		if pe.Retryable {
			t.Errorf("status=%d: 白名单为空时 5xx 不得可重试", status)
		}
		if pe.HTTPStatus != status {
			t.Errorf("status=%d: HTTPStatus 未保留", status)
		}
	}
	// 对照：宽容口径（DefaultParse）对同一批状态码仍标可重试。
	loose := DefaultParse("usdt")
	for _, status := range []int{429, 500, 502, 503} {
		if _, err := loose(status, nil, []byte(`{}`)); !pay.IsProviderRetryable(err) {
			t.Errorf("status=%d: DefaultParse 应保持可重试", status)
		}
	}
	if _, err := loose(http.StatusBadRequest, nil, []byte(`{}`)); pay.IsProviderRetryable(err) {
		t.Error("400 在宽容口径下也不得可重试")
	}
}

// TestStrictParseHonorsWhitelistedCode 白名单内的码可在写路径重试。
//
// 警示：502=受理状态未知（请求可能已被供应商受理、只是回执没穿回来），
// 提现白名单严禁纳入 502/504，原单重发有双付风险。本用例为验证白名单机制，
// 特意使用虚构状态码，不构成对任何真实 HTTP 状态码（尤其 502）的登记背书。
func TestStrictParseHonorsWhitelistedCode(t *testing.T) {
	const fakeCode = 59901 // 非真实 HTTP 语义，仅为机制验证
	code := strconv.Itoa(fakeCode)
	SetRetryableCodes(code)
	defer DeleteRetryableCode(code)

	parse := StrictParse("usdt")
	_, err := parse(fakeCode, nil, []byte(`{}`))
	if !pay.IsProviderRetryable(err) {
		t.Errorf("虚构码已登记白名单，写路径应可重试: %v", err)
	}
	if _, err := parse(http.StatusBadGateway, nil, []byte(`{}`)); pay.IsProviderRetryable(err) {
		t.Error("502 不得在写路径可重试（受理状态未知，双付风险）")
	}
	if _, err := parse(http.StatusInternalServerError, nil, []byte(`{}`)); pay.IsProviderRetryable(err) {
		t.Error("500 未登记白名单，写路径不应可重试")
	}
}

// TestStrictParsePassesThrough2xx 2xx 原样透出：包络判定归通道包，传输层不越位解释正文。
func TestStrictParsePassesThrough2xx(t *testing.T) {
	parse := StrictParse("usdt")
	out, err := parse(http.StatusOK, nil, []byte(`{"code":"0","data":{"balance":1}}`))
	if err != nil {
		t.Fatalf("2xx 不该报错: %v", err)
	}
	if string(out) != `{"code":"0","data":{"balance":1}}` {
		t.Fatalf("2xx 正文应原样透出，实得 %s", out)
	}
}

// TestStrictParsePlusNoTransportRetryStopsEndToEnd 端到端：写路径遇 500 且白名单不含 500 时，
// 即使 maxRetry=3 也只发一次。
func TestStrictParsePlusNoTransportRetryStopsEndToEnd(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	t.Cleanup(srv.Close)

	c := newTransportTestClient(t, srv.URL, 3, 2*time.Second)
	_, err := c.Do(context.Background(), CallSpec{
		Method: http.MethodPost, Path: "/pay", Body: []byte(`{}`),
		Parse:            StrictParse("usdt"),
		NoTransportRetry: true,
	})
	if err == nil {
		t.Fatal("期望报错")
	}
	if pay.IsProviderRetryable(err) {
		t.Error("写路径 500 白名单为空时不应标记可重试")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("期望只请求 1 次（不可重试），实得 %d 次", n)
	}
}
