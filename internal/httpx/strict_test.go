package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"touchgocore/pay"
)

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
