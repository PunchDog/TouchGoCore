package ustd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"touchgocore/pay"
	"touchgocore/util"
)

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
		pay.EndpointAccount:  "/api/account",
		pay.EndpointRecharge: "/api/recharge",
		pay.EndpointWithdraw: "/api/withdraw",
		pay.EndpointQuery:    "/api/query",
	}
}

// TestRechargeSignsAndSerializes 下单报文字段与签名域一致，金额以整数最小单位上送。
func TestRechargeSignsAndSerializes(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000000}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := UstdRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000000})
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if res.Status != pay.StatusSuccess || res.TradeNo != "T1" || res.Amount != 1000000 {
		t.Fatalf("回执不符: %+v", res)
	}
	req := f.seen()[0]
	var body map[string]any
	if err := json.Unmarshal([]byte(req.body), &body); err != nil {
		t.Fatal(err)
	}
	if body["merchant_id"] != merchantID {
		t.Fatalf("商户号未进报文: %s", req.body)
	}
	if body["network"] != testNetwork || body["contract"] != testContract {
		t.Fatalf("链上字段未进报文: %s", req.body)
	}
	if amt, ok := body["amount"].(float64); !ok || int64(amt) != 1000000 {
		t.Fatalf("金额不是整数最小单位: %s", req.body)
	}
	if strings.Contains(req.body, testSecret) {
		t.Fatalf("密钥进了报文: %s", req.body)
	}
	// 签名域十个定长标量 + 按登记顺序追加的通道特有字段。此单没填 network 之外的
	// 收款侧字段，所以 address / phone / memo / notify_url 四位是空串占位——
	// 空串也要占位，否则后面每一位的落点都会整体偏移。
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "1000000", pay.CurrencyUSDT, testNetwork, "", "", "", "",
		testContract)
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestWithdrawRequiresAddressOnWire 提现单把收款地址带进报文与签名域。
func TestWithdrawRequiresAddressOnWire(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/withdraw"] = `{"code":"0","data":{"order_no":"O2","status":"pending"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := UstdWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, Address: tronFFAcct})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending || res.IsFinal() {
		t.Fatalf("处理中不应是终态: %+v", res)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"address":"`+tronFFAcct+`"`) {
		t.Fatalf("提现报文缺地址: %s", req.body)
	}
	// 收款地址与网络标识都必须在签名域里：报文改一个字节而签名跟着不变，
	// 就等于谁都能把钱转到别的地址上。
	wantSign := pay.SignMD5(testSecret,
		"app1", merchantID, "O2", "500", pay.CurrencyUSDT, testNetwork, tronFFAcct, "", "", "",
		testContract)
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	if got := recorded(); len(got) != 1 || got[0].Status != pay.StatusPending {
		t.Fatalf("未广播处理中回执: %+v", got)
	}
}

// TestUnregisteredEndpointSendsNothing 端点没登记的操作必须就地拒绝：
// 发出去只会拿到一个含义不明的 404，还留下「请求可能已被受理」的疑点。
func TestUnregisteredEndpointSendsNothing(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, map[string]string{pay.EndpointQuery: "/api/query"}))

	if _, err := UstdRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Fatal("未登记端点应当报错")
	} else if !strings.Contains(err.Error(), "endpoints."+pay.EndpointRecharge) {
		t.Fatalf("错误未指明缺哪个端点: %v", err)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("配置不全却发出了 %d 次请求", n)
	}
}

// TestBusinessFailureIsNotRetried 包络业务失败码只发一次请求，且错误文案不含凭证。
func TestBusinessFailureIsNotRetried(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"250008","msg":"余额不足"}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	_, err := UstdRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000})
	if err == nil {
		t.Fatal("业务失败应当报错")
	}
	var pe *pay.ProviderError
	if !errors.As(err, &pe) || pe.Code != "250008" {
		t.Fatalf("错误类型不符: %v", err)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("业务失败重发了 %d 次", n)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("失败不该广播回执: %+v", got)
	}
}

// TestGatewayRetryIsTransparent 网关抖动由共用层原单重发，门面只报告最终结果。
func TestGatewayRetryIsTransparent(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.status["/api/withdraw"] = http.StatusBadGateway
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[testSection].MaxRetries = 1
	startWith(t, cfg)

	_, err := UstdWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 100, Address: tronRangeAcct})
	if err == nil {
		t.Fatal("持续 5xx 应当报错")
	}
	seen := f.seen()
	if len(seen) != 2 {
		t.Fatalf("请求次数=%d，期望 2（首次 + 1 次重试）", len(seen))
	}
	if seen[0].body != seen[1].body {
		t.Fatalf("重试换了报文，幂等键失效:\n%s\n%s", seen[0].body, seen[1].body)
	}
}

// TestUnrecognizedStatusBecomesUnknown 认不出的状态词归一成 UNKNOWN 并照常广播给对账，
// 而不是被判成失败后诱发上游重发同一张出款单。
func TestUnrecognizedStatusBecomesUnknown(t *testing.T) {
	_, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()
	res, err := UstdRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000})
	if err != nil {
		t.Fatalf("供应商已受理就不该报错: %v", err)
	}
	if res.Status != pay.StatusUnknown || res.IsFinal() {
		t.Fatalf("空回执归一成 %+v，期望 unknown 且非终态", res)
	}
	if got := recorded(); len(got) != 1 || got[0].Status != pay.StatusUnknown {
		t.Fatalf("未广播未知态回执: %+v", got)
	}
}

// TestSdkSectionKeyHookInjectsSecret 配置文件里不留密钥：下游按 SDK 段名注册钩子，
// 装配时注入凭证。与 telegram 包的 BotKey 钩子同源，差别在于键名跟着 SDK 段走。
func TestSdkSectionKeyHookInjectsSecret(t *testing.T) {
	f, url := newFakeSupplier(t)
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[testSection].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+testSection, func(sk *string) { *sk = testSecret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+testSection, id)

	startWith(t, cfg)
	if _, err := UstdQuery(nil, "O1"); err != nil {
		t.Fatalf("钩子注入密钥后应当可用: %v", err)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("请求次数=%d，期望 1", len(reqs))
	}
	if reqs[0].sign == "" {
		t.Fatal("请求未带上钩子注入密钥算出的签名")
	}
}

// TestAccountQueryReadsMerchantSnapshot 商户账户查询把供应商侧的余额、可用额、
// 授信与费率读成整数快照，并带上商户号与本通道的合约；这是读操作，不广播回执。
func TestAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	info, err := UstdAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Balance != 10 || info.Available != 7 ||
		info.Frozen != 3 || info.Credit != 1 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencyUSDT {
		t.Fatalf("币种缺省未补: %+v", info)
	}
	if !info.CanWithdraw(7) || info.CanWithdraw(8) {
		t.Fatalf("可用额判断不符: %+v", info)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("账户查询发出 %d 次请求，期望 1", len(reqs))
	}
	if !strings.Contains(reqs[0].body, `"`+"merchant_id"+`":"`+merchantID) ||
		!strings.Contains(reqs[0].body, `"`+"contract"+`":"`+testContract) {
		t.Fatalf("账户报文缺路由字段: %s", reqs[0].body)
	}
	if strings.Contains(reqs[0].body, testSecret) {
		t.Fatal("密钥进了报文")
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}

// TestAccountQueryFailureNotReportedAsZero 供应商拒了就只能报错：
// 返回一个全零账户会被下游读成「余额不足」，把查询故障伪装成业务拒绝。
func TestAccountQueryFailureNotReportedAsZero(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"401002","msg":"签名失败"}`
	startWith(t, payCfg(url, allEndpoints()))
	if _, err := UstdAccount(nil, &pay.AccountQuery{Currency: pay.CurrencyUSDT}); err == nil {
		t.Fatal("业务失败应当报错")
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("请求次数=%d，期望 1（业务失败不重试）", n)
	}
}
