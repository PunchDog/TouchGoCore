package pay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// C-F1 传输层用例：严格路径（Withdraw）在 transport/read_body 错误下一律不得自动重发。
//
// 这三类用例覆盖同一个资损窗口：请求已经写出、供应商可能已经受理，但响应没能
// 完整读回（断连 / 超时 / 半个响应体）。修复前 attempt 层对这两类错误标
// Retryable=true，Do 循环会原单重发最多 maxRetry 次——幂等不完备即双付。

// newTransportTestProvider 构造指向假供应商的 Provider；maxRetry/timeout 按用例给。
func newTransportTestProvider(t *testing.T, url string, maxRetry int, timeout time.Duration) *Provider {
	t.Helper()
	p, err := NewProvider(ProviderOptions{
		Name: "usdt", BaseURL: url, SecretKey: "s",
		Timeout: timeout, MaxRetry: maxRetry, Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

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

// assertNoResendStrict 断言严格路径的错误形态：ProviderError、指定 code、Retryable=false、只发一次。
func assertNoResendStrict(t *testing.T, err error, wantCode string, calls int32, op string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望报错，实得成功", op)
	}
	var pe *ProviderError
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

// TestWithdrawHijackCloseNeverResends 收单后断连：Withdraw 只发一次；Query 仍可重试。
func TestWithdrawHijackCloseNeverResends(t *testing.T) {
	var wcalls atomic.Int32
	wsrv := hijackCloseServer(t, &wcalls)
	p := newTransportTestProvider(t, wsrv.URL, 2, 2*time.Second)
	order := &PayOrder{OrderNo: "W-T1", Amount: 1000, Currency: CurrencyUSDT, Address: "TAddr"}
	_, err := p.Withdraw(context.Background(), order)
	assertNoResendStrict(t, err, "transport", wcalls.Load(), "Withdraw/断连")

	// 同形态错误在非严格路径（查单）保持可重试：查询无副作用，重发只是省一次人工对账。
	var qcalls atomic.Int32
	qsrv := hijackCloseServer(t, &qcalls)
	p2 := newTransportTestProvider(t, qsrv.URL, 2, 2*time.Second)
	if _, err := p2.QueryOrder(context.Background(), "W-T1"); err == nil {
		t.Fatal("Query: 期望报错")
	} else if !IsProviderRetryable(err) {
		t.Errorf("Query: 非严格路径 transport 错误应保持可重试，实得 %v", err)
	}
	if n := qcalls.Load(); n != 3 {
		t.Errorf("Query: 期望 3 次请求（首次+2 重试），实得 %d 次", n)
	}
}

// TestWithdrawTimeoutNeverResends 供应商收单后不回包、客户端超时：Withdraw 只发一次；
// Recharge（非严格）仍按 maxRetry 重发。
func TestWithdrawTimeoutNeverResends(t *testing.T) {
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
	p := newTransportTestProvider(t, wsrv.URL, 2, 40*time.Millisecond)
	order := &PayOrder{OrderNo: "W-T2", Amount: 1000, Currency: CurrencyUSDT, Address: "TAddr"}
	_, err := p.Withdraw(context.Background(), order)
	assertNoResendStrict(t, err, "transport", wcalls.Load(), "Withdraw/超时")

	var rcalls atomic.Int32
	rsrv := hang(t, &rcalls)
	p2 := newTransportTestProvider(t, rsrv.URL, 1, 40*time.Millisecond)
	_, err = p2.Recharge(context.Background(), &PayOrder{OrderNo: "D-T2", Amount: 500, Currency: CurrencyUSDT})
	if err == nil {
		t.Fatal("Recharge: 期望报错")
	} else if !IsProviderRetryable(err) {
		t.Errorf("Recharge: 非严格路径超时应保持可重试，实得 %v", err)
	}
	if n := rcalls.Load(); n != 2 {
		t.Errorf("Recharge: 期望 2 次请求（首次+1 重试），实得 %d 次", n)
	}
}

// TestWithdrawReadBodyErrorNeverResends 半个响应体（Content-Length 不符即断）：
// Withdraw 只发一次且错误码为 read_body；Query 仍可重试。
func TestWithdrawReadBodyErrorNeverResends(t *testing.T) {
	var wcalls atomic.Int32
	wsrv := partialBodyServer(t, &wcalls)
	p := newTransportTestProvider(t, wsrv.URL, 2, 2*time.Second)
	order := &PayOrder{OrderNo: "W-T3", Amount: 1000, Currency: CurrencyUSDT, Address: "TAddr"}
	_, err := p.Withdraw(context.Background(), order)
	assertNoResendStrict(t, err, "read_body", wcalls.Load(), "Withdraw/断体")

	var qcalls atomic.Int32
	qsrv := partialBodyServer(t, &qcalls)
	p2 := newTransportTestProvider(t, qsrv.URL, 1, 2*time.Second)
	if _, err := p2.QueryOrder(context.Background(), "W-T3"); err == nil {
		t.Fatal("Query: 期望报错")
	} else if !IsProviderRetryable(err) {
		t.Errorf("Query: 非严格路径 read_body 错误应保持可重试，实得 %v", err)
	}
	if n := qcalls.Load(); n != 2 {
		t.Errorf("Query: 期望 2 次请求（首次+1 重试），实得 %d 次", n)
	}
}
