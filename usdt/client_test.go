package usdt

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"touchgocore/pay"
	"touchgocore/tron"
)

// 本包没有装配代码了，这里钉的是「转发有没有转错」：
// 门面必须落到 USDT 那条 route 上（币种缺省、contract 进签名域、回执键前缀），
// 而不是顺手接到了 TRX 那条 route 上——两者现在共用同一套机器（tron/route.go），
// 接错的时候报错文案与回执前缀都会跟着换，下游按 CallUsdtMsg 注册的回调就收不到东西。
// 报文形态与签名逐位的完整用例在 tron/usdt_test.go。

// capturedRequest 是一次到达假供应商的请求摘要（不含凭证）。
type capturedRequest struct {
	path string
	sign string
	body string
}

// fakeSupplier 是假供应商：按路径返回预置响应，并记录收到的请求。
type fakeSupplier struct {
	mu       sync.Mutex
	requests []capturedRequest
	response map[string]string // 路径 -> 响应包络
	status   map[string]int    // 路径 -> HTTP 状态码（0 表示 200）
}

func newFakeSupplier(t *testing.T) (*fakeSupplier, string) {
	t.Helper()
	f := &fakeSupplier{response: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, capturedRequest{
			path: r.URL.Path, sign: r.Header.Get("X-Sign"), body: string(body),
		})
		payload, code := f.response[r.URL.Path], f.status[r.URL.Path]
		f.mu.Unlock()
		if payload == "" {
			payload = `{"code":"0","data":{}}`
		}
		w.Header().Set("Content-Type", "application/json")
		if code != 0 {
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeSupplier) seen() []capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func allEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointAccount: "/api/account",
		pay.EndpointQuery:   "/api/query",
	}
}

// TestShimForwardsToUsdtRouteOnWire 转发的门面确实落在 USDT 那条 route 上：
// 币种缺省补 USDT、合约既进报文又追加在签名域末尾。账户查询是充值删除后仅存的
// 供应商侧调用，路由身份由它钉（快照字段的归一条例在 tron 包用例里）。
func TestShimForwardsToUsdtRouteOnWire(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"currency":"USDT","status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := UsdtAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if res.Currency != pay.CurrencyUSDT {
		t.Fatalf("币种缺省未按 USDT 口径补: %+v", res)
	}
	reqs := f.seen()
	if len(reqs) != 1 || reqs[0].path != "/api/account" {
		t.Fatalf("账户查询请求不符: %+v", reqs)
	}
	req := reqs[0]
	if !strings.Contains(req.body, `"currency":"`+pay.CurrencyUSDT+`"`) ||
		!strings.Contains(req.body, `"contract":"`+testContract+`"`) {
		t.Fatalf("报文未按 USDT 通道装配: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, pay.CurrencyUSDT,
		testContract)
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s（签名域少一位合约就是把通道接到了另一条 route 上）", req.sign, want)
	}
	// 账户查询是读操作，不广播回执。
	if pub := recorded(); len(pub) != 0 {
		t.Fatalf("账户查询广播了回执: %+v", pub)
	}
}

// TestShimKeepsRoutesIndependent 启动 USDT 不该让 TRX 门面变成「能出钱」：
// 两条链路各有各的状态，共用一份快照时换配置会出现用 TRX 网关发 USDT 的单。
//
// TRX 侧现在恒返回未接入哨兵（供应商提现实现已删，链上原生币出款没有实现），
// 所以这里的判据从「文案是 TRON 通道未启动」换成「错误就是那个哨兵」：
// 保住的是同一条不变量——USDT 起了，TRX 也不会跟着动起来。
func TestShimKeepsRoutesIndependent(t *testing.T) {
	_, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := tron.TronWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); !errors.Is(err, tron.ErrPayoutNotWired) {
		t.Fatalf("USDT 启动后 TRX 门面却可用了: %v", err)
	}
	if _, err := UsdtQuery(nil, "O1"); err != nil {
		t.Fatalf("USDT 门面应当可用: %v", err)
	}
}

// TestShimErrorWordingUnchanged 错误文案逐字保持合并前的写法：
// 下游按文案做失败分类与告警归因，改一个字就是下游的归类漂移。
func TestShimErrorWordingUnchanged(t *testing.T) {
	UsdtStop(nil)
	recorded()
	_, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: "a"})
	if err == nil || err.Error() != "USDT 通道未启动（检查 usdt.provider 是否引用了已开启的 pay_sdks 段与商户账户）" {
		t.Fatalf("未启动文案变了: %v", err)
	}
	if _, err := UsdtQuery(nil, "  "); err == nil || err.Error() != "USDT Query 失败: 订单号（幂等键）为空" {
		t.Fatalf("空订单号文案变了: %v", err)
	}

	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"250008","msg":"余额不足"}`
	startWith(t, payCfg(url, allEndpoints()))
	if _, err := UsdtAccount(nil, nil); err == nil ||
		!strings.HasPrefix(err.Error(), "USDT 账户查询失败: ") {
		t.Fatalf("失败前缀不是 USDT 口径: %v", err)
	}
	// 失败语义（错误码原样、只发一次、不广播）由 tron 包的 TestUsdtBusinessFailureIsNotRetried 钉。
}
