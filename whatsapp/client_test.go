package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
)

// capturedRequest 是一次到达假供应商的请求摘要（不含凭证）。
type capturedRequest struct {
	path string
	sign string
	auth string
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
			path: r.URL.Path, sign: r.Header.Get("X-Sign"), auth: r.Header.Get("Authorization"), body: string(body),
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

// startWith 用给定配置走一遍真实启动路径（含 SDK 引用解析与密钥钩子）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	WhatsappStop(nil)
	recorded()
	WhatsappStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		WhatsappStop(nil)
		recorded()
	})
}

func allEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointSendCode: "/api/code",
		pay.EndpointLogin:    "/api/login",
		pay.EndpointAccount:  "/api/account",
		pay.EndpointRecharge: "/api/recharge",
		pay.EndpointWithdraw: "/api/withdraw",
		pay.EndpointQuery:    "/api/query",
	}
}

// TestLoginThenRechargeCarriesToken 登录后会话凭证只经请求头传给后续请求，
// 不出现在报文里；签名域按登记顺序覆盖下单字段。
func TestLoginThenRechargeCarriesToken(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9","phone":"86138"}}`
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	got, err := WhatsappLogin(nil, "86138", "123456")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if got.UserID != "U9" {
		t.Fatalf("登录结果=%+v", got)
	}
	// 登录请求本身还没有会话凭证，但必须已带签名。
	first := f.seen()
	if len(first) != 1 || first[0].sign == "" || first[0].auth != "" {
		t.Fatalf("登录请求不符: %+v", first)
	}
	if !strings.Contains(first[0].body, "123456") {
		t.Fatalf("登录报文缺验证码: %s", first[0].body)
	}

	res, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000, Currency: pay.CurrencyUSDT})
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if res.Status != pay.StatusSuccess || res.TradeNo != "T1" || res.Amount != 1000 {
		t.Fatalf("充值回执=%+v", res)
	}
	req := f.seen()[1]
	if req.auth != "TK-1" {
		t.Fatalf("后续请求未带会话凭证: %q", req.auth)
	}
	wantSign := pay.SignMD5(testSecret, "app1", merchantID, "O1", "1000", pay.CurrencyUSDT, "", "")
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	if !strings.Contains(req.body, `"order_no":"O1"`) || !strings.Contains(req.body, `"amount":1000`) {
		t.Fatalf("充值报文不符: %s", req.body)
	}
	if strings.Contains(req.body, "TK-1") || strings.Contains(req.body, testSecret) {
		t.Fatalf("凭证进了报文: %s", req.body)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestWithdrawCarriesMerchantAndTarget 商户转出：商户号与目标地址必须既进报文又进签名域。
// 商户号定「钱从谁的账上出」，地址定「钱到哪去」——任一缺失或被篡改都是错账；
// 回执成功后还要广播给下游对账。
func TestWithdrawCarriesMerchantAndTarget(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/withdraw"] = `{"code":"0","data":{"order_no":"O2","trade_no":"T2","status":"success","amount":500}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := WhatsappWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, Currency: pay.CurrencyUSDT, Address: "TTargetAddr"})
	if err != nil {
		t.Fatalf("提现失败: %v", err)
	}
	if res.Status != pay.StatusSuccess || res.TradeNo != "T2" || res.Amount != 500 {
		t.Fatalf("提现回执=%+v", res)
	}
	req := f.seen()[0]
	if req.path != "/api/withdraw" {
		t.Fatalf("请求路径=%s", req.path)
	}
	for _, want := range []string{`"merchant_id":"` + merchantID + `"`, `"address":"TTargetAddr"`, `"amount":500`} {
		if !strings.Contains(req.body, want) {
			t.Fatalf("提现报文缺 %s: %s", want, req.body)
		}
	}
	if strings.Contains(req.body, testSecret) {
		t.Fatalf("凭证进了报文: %s", req.body)
	}
	// 签名域十个定长位：地址在第七位，网络未填以空串占位。
	wantSign := pay.SignMD5(testSecret, "app1", merchantID, "O2", "500", pay.CurrencyUSDT, "", "TTargetAddr")
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O2" || pub[0].Status != pay.StatusSuccess {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestSendCodeUsesConfiguredTemplate 模板名来自配置，供文档到位后按供应商模板改。
func TestSendCodeUsesConfiguredTemplate(t *testing.T) {
	f, url := newFakeSupplier(t)
	cfg := payCfg(url, allEndpoints())
	cfg.Whatsapp.Templates["auth_verify"] = "tpl_x"
	startWith(t, cfg)

	if err := WhatsappSendCode(nil, " 86138 "); err != nil {
		t.Fatalf("发码失败: %v", err)
	}
	req := f.seen()[0]
	var got map[string]any
	if err := json.Unmarshal([]byte(req.body), &got); err != nil {
		t.Fatal(err)
	}
	if got["template"] != "tpl_x" || got["phone"] != "86138" {
		t.Fatalf("发码报文不符: %s", req.body)
	}
}

// TestLogoutClearsToken 退出后请求头不再带旧会话，避免拿已失效凭证继续出款。
func TestLogoutClearsToken(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	startWith(t, payCfg(url, allEndpoints()))

	if _, err := WhatsappLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	WhatsappLogout()
	if _, err := WhatsappQuery(nil, "O1"); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if req := f.seen()[1]; req.auth != "" {
		t.Fatalf("退出后仍带会话凭证: %q", req.auth)
	}
}

// TestSessionNotSharedAcrossHosts 登录链路与资金链路指向不同供应商时，
// 会话凭证不得跟着请求头送到另一家：那等于把用户的登录态交给第三方。
func TestSessionNotSharedAcrossHosts(t *testing.T) {
	loginF, loginURL := newFakeSupplier(t)
	loginF.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	fundF, fundURL := newFakeSupplier(t)
	fundF.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","status":"success","amount":1000}}`

	cfg := payCfg(fundURL, allEndpoints())
	// 登录链路换成另一家：独立 SDK 段、独立基址，只做下发所以没有商户账户。
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: loginURL,
		AppID: "app1", SecretKey: testSecret,
		Endpoints: map[string]string{pay.EndpointLogin: "/api/login", pay.EndpointSendCode: "/api/code"},
	}
	cfg.Whatsapp.Login = &config.PaySDKRef{SDK: loginSection}
	startWith(t, cfg)

	if _, err := WhatsappLogin(nil, "86138", "123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000}); err != nil {
		t.Fatal(err)
	}
	if req := fundF.seen()[0]; req.auth != "" {
		t.Fatalf("会话凭证被送到另一家供应商: %q", req.auth)
	}
}

// TestUnregisteredEndpointSendsNothing 端点没登记的操作必须就地拒绝：
// 发出去只会拿到一个含义不明的 404，还留下「请求可能已被受理」的疑点。
func TestUnregisteredEndpointSendsNothing(t *testing.T) {
	f, url := newFakeSupplier(t)
	cfg := payCfg(url, map[string]string{pay.EndpointLogin: "/api/login"})
	startWith(t, cfg)

	if _, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
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

	_, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000, Currency: pay.CurrencyUSDT})
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

// TestGatewayRetryIsTransparent 网关抖动由共用层重试，门面只报告最终结果。
func TestGatewayRetryIsTransparent(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.status["/api/withdraw"] = http.StatusBadGateway
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[testSection].MaxRetries = 1
	startWith(t, cfg)

	_, err := WhatsappWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 100, Address: "TAddr1"})
	if err == nil {
		t.Fatal("持续 5xx 应当报错")
	}
	if n := len(f.seen()); n != 2 {
		t.Fatalf("请求次数=%d，期望 2（首次 + 1 次重试），且两次都是同一张单", n)
	}
	seen := f.seen()
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
	res, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000})
	if err != nil {
		t.Fatalf("供应商已受理就不该报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("空回执归一成 %s，期望 unknown", res.Status)
	}
	if res.IsFinal() {
		t.Fatal("UNKNOWN 不得是终态，否则会被记账流程当已结")
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].Status != pay.StatusUnknown || pub[0].OrderNo != "O1" {
		t.Fatalf("未广播未知态回执: %+v", pub)
	}
}

// TestSdkSectionKeyHookInjectsSecret 配置文件里不留密钥：下游按 SDK 段名注册一个钩子，
// 两条链路装配时各自注入同一份凭证。与 telegram 包的 BotKey 钩子同源，
// 差别在于键名跟着 SDK 段走——一家供应商的密钥登记一次，而不是每条通道抄一遍。
func TestSdkSectionKeyHookInjectsSecret(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/login"] = `{"code":"0","data":{"token":"TK-1","user_id":"U9"}}`
	cfg := payCfg(url, allEndpoints())
	cfg.PaySDks[testSection].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+testSection, func(sk *string) { *sk = testSecret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+testSection, id)

	startWith(t, cfg)
	if _, err := WhatsappLogin(nil, "86138", "123456"); err != nil {
		t.Fatalf("钩子注入密钥后登录应当可用: %v", err)
	}
	if _, err := WhatsappQuery(nil, "O1"); err != nil {
		t.Fatalf("钩子注入密钥后查询应当可用: %v", err)
	}
	reqs := f.seen()
	if len(reqs) != 2 {
		t.Fatalf("请求次数=%d，期望 2", len(reqs))
	}
	for i, req := range reqs {
		if req.sign == "" {
			t.Fatalf("第 %d 个请求未带上钩子注入密钥算出的签名", i+1)
		}
	}
}

// TestAccountQueryReadsMerchantSnapshot 商户账户查询读回供应商侧的额度快照；
// 这是读操作，不广播回执，也不参与登录会话。
func TestAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	info, err := WhatsappAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Available != 7 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if !info.CanWithdraw(7) || info.CanWithdraw(8) {
		t.Fatalf("可用额判断不符: %+v", info)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("账户查询发出 %d 次请求，期望 1", len(reqs))
	}
	if !strings.Contains(reqs[0].body, `"`+"merchant_id"+`":"`+merchantID) {
		t.Fatalf("账户报文缺商户号: %s", reqs[0].body)
	}
	if strings.Contains(reqs[0].body, testSecret) {
		t.Fatal("密钥进了报文")
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}

// TestStartNilCfgDoesNotPanic 没有任何 whatsapp 配置时启动必须安静通过。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	WhatsappStop(nil)
	WhatsappStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	// 传 nil 回落到包级运行快照：上一条刚把「没有配置」写进快照，这里才测得准「未启动」。
	WhatsappStart(nil)
	if _, err := WhatsappQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	if err := WhatsappSendCode(nil, "86138"); err == nil {
		t.Fatal("未启动时发码应当被拒")
	}
	WhatsappStop(nil)
	WhatsappStop(nil) // 幂等
}
