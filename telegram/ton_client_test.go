package telegram

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

const (
	// tonSecret 是测试内用的签名密钥，与真实凭证无关。
	tonSecret = "TEST_TON_SECRET"
	// tonSection / tonAccount 是测试里 SDK 段与商户账户的名字。
	tonSection  = "test_ton"
	tonAccount  = "default"
	tonMerchant = "MCH-TON"
	tonJetton   = "EQDtFpEwcR-fm552Nv6h3FDFdv3TbHx8w9Wm7FqYqU8dBB1q"
)

// tonRecorder 收集 tonPublish 广播出去的回执。
var tonRecorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallTonMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallTonMsg+kind, func(res *pay.PayResult) {
			tonRecorder.mu.Lock()
			tonRecorder.calls = append(tonRecorder.calls, res)
			tonRecorder.mu.Unlock()
		})
	}
}

func tonRecorded() []*pay.PayResult {
	tonRecorder.mu.Lock()
	defer tonRecorder.mu.Unlock()
	got := tonRecorder.calls
	tonRecorder.calls = nil
	return got
}

// tonFakeSupplier 是假供应商：按路径返回预置响应，并记录收到的请求。
type tonFakeSupplier struct {
	mu       sync.Mutex
	requests []struct {
		path string
		sign string
		body string
	}
	response map[string]string
	status   map[string]int
}

func newTonFakeSupplier(t *testing.T) (*tonFakeSupplier, string) {
	t.Helper()
	f := &tonFakeSupplier{response: map[string]string{}, status: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, struct {
			path string
			sign string
			body string
		}{r.URL.Path, r.Header.Get("X-Sign"), string(body)})
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

func (f *tonFakeSupplier) seen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *tonFakeSupplier) request(i int) (path, sign, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.requests[i]
	return r.path, r.sign, r.body
}

// payTonCfg 组一份「pay_sdks 登记一段 + telegram.ton 引用它」的完整配置。
//
// 通道段只留 sdk/account 两个名字，其余全在 SDK 段里：这正是本仓的装配形态，
// 测试若绕过 paysdk 直接喂 ProviderOptions，就测不到「引用指不通」这一整类错误。
func payTonCfg(baseURL string, endpoints map[string]string) *config.Cfg {
	return &config.Cfg{
		PaySDks: map[string]*config.PaySDKConfig{
			tonSection: {
				Enable: "on", Driver: pay.DriverGeneric, BaseURL: baseURL,
				AppID: "app1", SecretKey: tonSecret, Endpoints: endpoints,
				Accounts: map[string]*config.PayMerchantAccount{
					tonAccount: {Enable: "on", MerchantID: tonMerchant},
				},
			},
		},
		Telegram: &config.TelegramConfig{
			Ton:        &config.PaySDKRef{SDK: tonSection, Account: tonAccount},
			TonNetwork: "mainnet",
			Jetton:     tonJetton,
		},
	}
}

// startTon 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
// 只装配 TON 链路，不碰 Bot：两边的开关本来就该互不影响。
func startTon(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	TonStop(nil)
	tonRecorded()
	TonStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		TonStop(nil)
		tonRecorded()
	})
}

func tonEndpoints() map[string]string {
	return map[string]string{
		pay.EndpointAccount:  "/api/account",
		pay.EndpointRecharge: "/api/recharge",
		pay.EndpointWithdraw: "/api/withdraw",
		pay.EndpointQuery:    "/api/query",
	}
}

// TestTonNewClientFailClosed 引用指不通的链路一律不启动：SDK 段没开、商户账户没点名、
// 驱动标记写错，都只会在供应商侧留下验签失败，本地却看起来一切正常。
func TestTonNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payTonCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 telegram 段", func(c *config.Cfg) { c.Telegram = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Telegram.Ton = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Telegram.Ton.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[tonSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[tonSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[tonSection].Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.PaySDks[tonSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "M2"}
			c.Telegram.Ton.Account = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[tonSection].Accounts[tonAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[tonSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[tonSection].SecretKey = "" }},
	}
	for _, cs := range cases {
		cfg := full()
		cs.mutate(cfg)
		if c, ok := newTonClient(cfg); ok || c != nil {
			t.Errorf("%s 不应启动，实得 ok=%v", cs.name, ok)
		}
	}
	// 端点表为空但引用齐备：装配成功，但任何操作都应在发请求之前被拒。
	if _, ok := newTonClient(full()); !ok {
		t.Error("引用齐备时应当启动")
	}
	// 只有一个账户时允许不点名。
	cfg := full()
	cfg.Telegram.Ton.Account = ""
	if _, ok := newTonClient(cfg); !ok {
		t.Error("唯一账户时 account 留空应当可启动")
	}
}

// TestTonOrderDefaultsFromChainConfig 网络标识与 jetton 由链路配置补齐，币种缺省补 TON；
// 调用方显式给的值不得被覆盖，交进来的订单对象也不得被就地改动。
func TestTonOrderDefaultsFromChainConfig(t *testing.T) {
	c, ok := newTonClient(payTonCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000000, Address: "EQAddress"}
	got := c.order(o)
	if got.Currency != pay.CurrencyTON || got.Network != "mainnet" {
		t.Fatalf("缺省补齐不符: %+v", got)
	}
	if o.Currency != "" || o.Network != "" {
		t.Fatalf("就地改了调用方的订单: %+v", o)
	}
	// 原生 TON（配置里 jetton 留空）与调用方显式指定的网络都不应被配置覆盖。
	native := payTonCfg("https://pay.invalid", nil)
	native.Telegram.Jetton = ""
	nativeC, ok := newTonClient(native)
	if !ok {
		t.Fatal("装配失败")
	}
	other := nativeC.order(&pay.PayOrder{OrderNo: "O2", Amount: 1, Network: "testnet"})
	if other.Network != "testnet" {
		t.Fatalf("显式网络被配置覆盖: %+v", other)
	}
}

// TestTonRechargeSignsAndSerializes 下单报文与签名一致，金额以整数最小单位（nanoton）上送。
func TestTonRechargeSignsAndSerializes(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000000000}}`
	cfg := payTonCfg(url, tonEndpoints())
	startTon(t, cfg)
	tonRecorded()

	res, err := TonRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000000000})
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if res.Status != pay.StatusSuccess || res.TradeNo != "T1" || res.Amount != 1000000000 {
		t.Fatalf("回执不符: %+v", res)
	}
	_, sign, body := f.request(0)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if amt, ok := parsed["amount"].(float64); !ok || int64(amt) != 1000000000 {
		t.Fatalf("金额不是整数最小单位: %s", body)
	}
	if parsed["jetton"] != tonJetton || parsed["network"] != "mainnet" {
		t.Fatalf("链上字段未进报文: %s", body)
	}
	if parsed["merchant_id"] != tonMerchant {
		t.Fatalf("商户号未进报文: %s", body)
	}
	if strings.Contains(body, tonSecret) {
		t.Fatalf("密钥进了报文: %s", body)
	}
	want := pay.SignMD5(tonSecret, "app1", tonMerchant, "O1", "1000000000", pay.CurrencyTON, "", "", tonJetton)
	if sign != want {
		t.Fatalf("签名=%s，期望 %s", sign, want)
	}
	if pub := tonRecorded(); len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestTonUnregisteredEndpointSendsNothing 端点没登记的操作必须就地拒绝。
func TestTonUnregisteredEndpointSendsNothing(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	startTon(t, payTonCfg(url, map[string]string{pay.EndpointQuery: "/api/query"}))
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: "EQ1"}); err == nil {
		t.Fatal("未登记端点应当报错")
	} else if !strings.Contains(err.Error(), "endpoints."+pay.EndpointWithdraw) {
		t.Fatalf("错误未指明缺哪个端点: %v", err)
	}
	if n := f.seen(); n != 0 {
		t.Fatalf("配置不全却发出了 %d 次请求", n)
	}
}

// TestTonGatewayRetryIsTransparent 网关抖动由共用层原单重发，门面只报告最终结果。
func TestTonGatewayRetryIsTransparent(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	f.status["/api/withdraw"] = http.StatusBadGateway
	cfg := payTonCfg(url, tonEndpoints())
	cfg.PaySDks[tonSection].MaxRetries = 1
	cfg.PaySDks[tonSection].TimeoutSec = 5
	startTon(t, cfg)

	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 100, Address: "EQ1"}); err == nil {
		t.Fatal("持续 5xx 应当报错")
	}
	if n := f.seen(); n != 2 {
		t.Fatalf("请求次数=%d，期望 2（首次 + 1 次重试）", n)
	}
	_, _, first := f.request(0)
	_, _, second := f.request(1)
	if first != second {
		t.Fatalf("重试换了报文，幂等键失效:\n%s\n%s", first, second)
	}
}

// TestTonBusinessFailureIsNotRetried 包络业务失败码只发一次请求，且错误文案不含凭证。
func TestTonBusinessFailureIsNotRetried(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"250008","msg":"余额不足"}`
	startTon(t, payTonCfg(url, tonEndpoints()))
	tonRecorded()

	_, err := TonRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000})
	var pe *pay.ProviderError
	if !errors.As(err, &pe) || pe.Code != "250008" {
		t.Fatalf("错误类型不符: %v", err)
	}
	if n := f.seen(); n != 1 {
		t.Fatalf("业务失败重发了 %d 次", n)
	}
	if strings.Contains(err.Error(), tonSecret) {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
	if got := tonRecorded(); len(got) != 0 {
		t.Fatalf("失败不该广播回执: %+v", got)
	}
}

// TestTonUnrecognizedStatusBecomesUnknown 认不出的状态词归一成 UNKNOWN 并照常广播给对账。
func TestTonUnrecognizedStatusBecomesUnknown(t *testing.T) {
	_, url := newTonFakeSupplier(t)
	startTon(t, payTonCfg(url, tonEndpoints()))
	tonRecorded()
	res, err := TonRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000})
	if err != nil {
		t.Fatalf("供应商已受理就不该报错: %v", err)
	}
	if res.Status != pay.StatusUnknown || res.IsFinal() {
		t.Fatalf("空回执归一成 %+v，期望 unknown 且非终态", res)
	}
	if got := tonRecorded(); len(got) != 1 || got[0].Status != pay.StatusUnknown {
		t.Fatalf("未广播未知态回执: %+v", got)
	}
}

// TestTonAccountQueryReadsMerchantSnapshot 商户账户查询把供应商侧的额度读成整数快照，
// 币种缺省补 TON；这是读操作，不广播回执。
func TestTonAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"frozen"}}`
	startTon(t, payTonCfg(url, tonEndpoints()))
	tonRecorded()

	info, err := TonAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctFrozen || info.Balance != 10 || info.Frozen != 3 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencyTON {
		t.Fatalf("币种缺省未补: %+v", info)
	}
	// 冻结账户即便有可用额也不能出款。
	if info.CanWithdraw(1) {
		t.Fatalf("冻结账户被判成可出款: %+v", info)
	}
	body := func() string {
		_, _, b := f.request(0)
		return b
	}
	if !strings.Contains(body(), `"`+"merchant_id"+`":"`+tonMerchant) ||
		!strings.Contains(body(), `"`+"jetton"+`":"`+tonJetton) {
		t.Fatalf("账户报文缺路由字段: %s", body())
	}
	if strings.Contains(body(), tonSecret) {
		t.Fatal("密钥进了报文")
	}
	if got := tonRecorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}

// TestTonSdkSectionKeyHookInjectsSecret 配置文件里不留密钥：下游按 SDK 段名注册钩子，
// 在启动前注入。
func TestTonProviderKeyHookInjectsSecret(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	cfg := payTonCfg(url, tonEndpoints())
	cfg.PaySDks[tonSection].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+tonSection, func(sk *string) { *sk = tonSecret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+tonSection, id)

	startTon(t, cfg)
	if _, err := TonQuery(nil, "O1"); err != nil {
		t.Fatalf("钩子注入密钥后应当可用: %v", err)
	}
	if _, sign, _ := f.request(0); sign == "" {
		t.Fatal("请求未带上钩子注入密钥算出的签名")
	}
}

// TestTonOrderPreChecks 下单前置校验在发出请求之前就生效。
func TestTonOrderPreChecks(t *testing.T) {
	startTon(t, payTonCfg("https://pay.invalid", tonEndpoints()))
	tonRecorded()
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil ||
		!strings.Contains(err.Error(), "地址") {
		t.Errorf("提现缺地址未被拦下: %v", err)
	}
	if _, err := TonRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 0}); err == nil {
		t.Error("零金额未被拦下")
	}
	if _, err := TonQuery(nil, "  "); err == nil {
		t.Error("空订单号未被拦下")
	}
	if got := tonRecorded(); len(got) != 0 {
		t.Errorf("前置校验失败却广播了回执: %+v", got)
	}
}
