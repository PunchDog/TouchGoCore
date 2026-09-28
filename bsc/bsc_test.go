package bsc

import (
	"context"
	"encoding/json"
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

// testSecret 是测试内用的签名密钥，与真实凭证无关。
const testSecret = "TEST_SECRET_KEY"

// testSection / testAccount 是测试里 SDK 段与商户账户的名字。
const (
	testSection = "test_a"
	testAccount = "default"
	merchantID  = "MCH-TEST"
	// testEVMContract 是 BEP20 合约地址的夹具（EIP-55 混合大小写形态，校验和已核）。
	// 合约留空代表原生 BNB，也是本通道的默认形态，两种都要测到。
	testEVMContract = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	testNetwork     = pay.NetworkMainnet
)

// EVM 地址夹具：两个 EIP-55 校验和形态由 Keccak-256 现算（不是抄来的），
// 单一大小写的历史形态不带校验和语义，格式过关即放行。
const (
	evmChecksumMixed = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmChecksumMixed2 = "0xDEaDbEeFdEADbEeFdEaDbEEFdEaDbeEFdEaDbeeF"
	evmAllLower      = "0xfb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"
	evmAllUpper      = "0xFB6916095CA1D6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmAllDigits     = "0x1234567890123456789012345678901234567890"
	// evmBogusChecksum 长度、前缀、字符集全对，只有大小写与校验和不符。
	evmBogusChecksum = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3B"
)

// recorder 收集 publish 广播出去的回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallBscMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallBscMsg+kind, func(res *pay.PayResult) {
			recorder.mu.Lock()
			recorder.calls = append(recorder.calls, res)
			recorder.mu.Unlock()
		})
	}
}

func recorded() []*pay.PayResult {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	got := recorder.calls
	recorder.calls = nil
	return got
}

// payCfg 组一份「pay_sdks 登记一段 + bsc 段引用它」的完整配置。
func payCfg(baseURL string, endpoints map[string]string) *config.Cfg {
	return &config.Cfg{
		PaySDks: map[string]*config.PaySDKConfig{
			testSection: {
				Enable: "on", Driver: pay.DriverGeneric, BaseURL: baseURL,
				AppID: "app1", SecretKey: testSecret, Endpoints: endpoints,
				Accounts: map[string]*config.PayMerchantAccount{
					testAccount: {Enable: "on", MerchantID: merchantID},
				},
			},
		},
		Bsc: &config.BscConfig{
			Provider: &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Network:  testNetwork,
		},
	}
}

// startWith 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	BscStop(nil)
	recorded()
	BscStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		BscStop(nil)
		recorded()
	})
}

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

// TestIsValidEVMAddress EIP-55 的正负用例：混合大小写必须等于校验和形态；
// 全小写、全大写、纯数字是历史形态，不携带校验和信息，格式过关即放行。
func TestIsValidEVMAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{evmChecksumMixed, true, "EIP-55 校验和形态"},
		{evmChecksumMixed2, true, "另一枚现算的校验和形态"},
		{evmAllLower, true, "全小写（无校验和语义的历史形态）"},
		{evmAllUpper, true, "全大写（同上）"},
		{evmAllDigits, true, "纯数字（无字母，大小写规则不适用）"},
		{evmBogusChecksum, false, "混合大小写但校验和不符"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{"fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a", false, "缺 0x 前缀"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3", false, "少一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3AA", false, "多一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3Z", false, "含非十六进制字符"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2b4D0F3a", false, "校验和形态上翻转两个字母的大小写"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 EVM 地址"},
	}
	// 逐位替换成 '0' / 'f' 的三向断言：结果若是混合大小写，除非恰好落在
	// EIP-55 形态上，否则必须被校验和拦下；单一大小写与纯数字则必须放行。
	body := []byte(evmChecksumMixed)
	for i := 2; i < len(body); i++ {
		orig := body[i]
		for _, cand := range []byte{'0', 'f'} {
			if cand == orig {
				continue
			}
			body[i] = cand
			addr := string(body)
			hex40 := addr[2:]
			// 替换后恰好落在校验和形态上的属巧合，不在被拒之列。
			if hex40 == toEIP55(hex40) {
				continue
			}
			mixed := hex40 != strings.ToLower(hex40) && hex40 != strings.ToUpper(hex40)
			if got := IsValidEVMAddress(addr); got == mixed {
				t.Errorf("替换后的地址 %q 判定不符：混合大小写=%v，实得 ok=%v", addr, mixed, got)
			}
		}
		body[i] = orig
	}
	// 逐位替换跑完后原夹具必须完好：循环里的写回如果漏了一位，这里会先红。
	if string(body) != evmChecksumMixed || !IsValidEVMAddress(evmChecksumMixed) {
		t.Fatalf("夹具 %s 在替换循环里被改坏了", evmChecksumMixed)
	}
	for _, cs := range cases {
		if got := IsValidEVMAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidEVMAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestToEIP55Derivation oracle 单独钉一次：对已知摘要手工推导前几位的大小写，
// 防止 toEIP55 的 nibble 取位（高低半字节 / 奇偶下标）被改反而无人察觉。
func TestToEIP55Derivation(t *testing.T) {
	// 夹具已核：evmChecksumHash("fb69...") 前两字节 0xF4 0x6C——
	// 第 0 位看高 nibble F >= 8，'f' 大写；第 1 位看低 nibble 4 < 8，'b' 保持小写；
	// 第 2 位看下一字节高 nibble 6 < 8，'6' 无字母；第 3 位看低 nibble C >= 8，'9' 无字母。
	want := "fB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	if got := toEIP55("fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"); got != want {
		t.Fatalf("toEIP55 推导不符:\n got=%s\nwant=%s", got, want)
	}
	// 反向钉 nibble 取位：大小写颠倒的串不能等于自身推导结果。
	if got := toEIP55("Fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"); got == "Fb6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A" {
		t.Fatal("nibble 取位方向不符：颠倒大小写的串应被推导回校验和形态")
	}
}

// TestBNBRuleRegistered 本包必须把 BNB 规则登记进 pay 的注册表。
func TestBNBRuleRegistered(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyBNB)
	if !ok {
		t.Fatalf("BNB 规则未登记，已登记的是 %v", pay.CurrenciesWithRules())
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: evmAllLower}); err != nil {
		t.Errorf("全小写合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: evmBogusChecksum}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestBNBRuleRejectsMemo EVM 转账没有 memo 概念：填了就拒而不是忽略。
func TestBNBRuleRejectsMemo(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyBNB)
	if !ok {
		t.Fatal("BNB 规则未登记")
	}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	if err := rule.CheckMemo(&pay.PayOrder{Memo: "   "}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestNewClientFailClosed 引用指不通、合约地址不合 EVM 格式的链路一律不启动：
// 合约是供应商广播时的收端之一，写错要等到第一笔出款才暴露。
func TestNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 bsc 段", func(c *config.Cfg) { c.Bsc = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Bsc.Provider = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Bsc.Provider.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[testSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.PaySDks[testSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "M2"}
			c.Bsc.Provider.Account = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[testSection].Accounts[testAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[testSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[testSection].SecretKey = "" }},
		{"合约填了 TRON 地址", func(c *config.Cfg) { c.Bsc.Token = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" }},
		{"合约填了校验和不过的混合大小写", func(c *config.Cfg) { c.Bsc.Token = evmBogusChecksum }},
	}
	for _, cs := range cases {
		cfg := full()
		cs.mutate(cfg)
		if c, ok := newClient(cfg); ok || c != nil {
			t.Errorf("%s 不应启动，实得 ok=%v", cs.name, ok)
		}
	}
	// 端点表为空但引用齐备：装配成功，但任何操作都应在发请求之前被拒。
	if _, ok := newClient(full()); !ok {
		t.Error("引用齐备时应当启动")
	}
	// 原生 BNB 不配合约是默认形态；配了合法合约也要能起。
	cfg := full()
	cfg.Bsc.Token = evmAllLower
	if _, ok := newClient(cfg); !ok {
		t.Error("全小写合约地址应当可启动")
	}
}

// TestOrderDefaultsFromChainConfig 网络标识与币种缺省由链路配置补齐，
// 调用方显式给的值不得被覆盖，交进来的订单对象也不得被就地改动。
func TestOrderDefaultsFromChainConfig(t *testing.T) {
	c, ok := newClient(payCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000000000000000, Address: evmAllLower}
	got := c.order(o)
	if got.Currency != pay.CurrencyBNB || got.Network != testNetwork {
		t.Fatalf("缺省补齐不符: %+v", got)
	}
	if o.Currency != "" || o.Network != "" {
		t.Fatalf("就地改了调用方的订单: %+v", o)
	}
	other := c.order(&pay.PayOrder{OrderNo: "O2", Amount: 1, Currency: pay.CurrencyUSDT, Network: pay.NetworkChapel})
	if other.Currency != pay.CurrencyUSDT || other.Network != pay.NetworkChapel {
		t.Fatalf("显式字段被配置覆盖: %+v", other)
	}
}

// TestStartNilCfgDoesNotPanic 没有任何 bsc 配置时启动必须安静通过。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	BscStop(nil)
	BscStart(corectx.WithCfg(context.Background(), nil))
	BscStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	BscStart(nil)
	BscStart(corectx.WithCfg(context.Background(), &config.Cfg{Bsc: &config.BscConfig{}}))
	if _, err := BscQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	BscStop(nil)
	BscStop(nil) // 幂等
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	BscStop(nil)
	recorded()
	if _, err := BscRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Error("未启动时充值应当被拒")
	}
	if _, err := BscWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: evmAllLower}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := BscQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := BscAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
	BscStop(nil)
}

// TestRechargeSignsAndSerializes 原生 BNB 下单（未配合约）：报文字段与签名域一致，
// 金额以整数最小单位（18 位）原样上送，十个定长签名位后面没有追加。
func TestRechargeSignsAndSerializes(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000000}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := BscRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000000})
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
	if body["currency"] != pay.CurrencyBNB || body["network"] != testNetwork {
		t.Fatalf("链上字段未进报文: %s", req.body)
	}
	if _, has := body["contract"]; has {
		t.Fatalf("未配合约却发出了 contract 字段: %s", req.body)
	}
	if strings.Contains(req.body, testSecret) {
		t.Fatalf("密钥进了报文: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "1000000", pay.CurrencyBNB, testNetwork, "", "", "", "")
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestBEP20ContractAppendsToBodyAndSign 配置了 BEP20 合约时，
// contract 必须既进报文又追加在签名域末尾——字段顺序与 usdt 侧口径一致。
func TestBEP20ContractAppendsToBodyAndSign(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","status":"success","amount":10}}`
	cfg := payCfg(url, allEndpoints())
	cfg.Bsc.Token = testEVMContract
	startWith(t, cfg)
	recorded()

	if _, err := BscRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 10}); err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"contract":"`+testEVMContract+`"`) {
		t.Fatalf("合约未进报文: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "10", pay.CurrencyBNB, testNetwork, "", "", "", "",
		testEVMContract)
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
}

// TestWithdrawRequiresAddressOnWire 提现单把收款地址带进报文与签名域。
func TestWithdrawRequiresAddressOnWire(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/withdraw"] = `{"code":"0","data":{"order_no":"O2","status":"pending"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := BscWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, Address: evmChecksumMixed})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending || res.IsFinal() {
		t.Fatalf("处理中不应是终态: %+v", res)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"address":"`+evmChecksumMixed+`"`) {
		t.Fatalf("提现报文缺地址: %s", req.body)
	}
	wantSign := pay.SignMD5(testSecret,
		"app1", merchantID, "O2", "500", pay.CurrencyBNB, testNetwork, evmChecksumMixed, "", "", "")
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	if got := recorded(); len(got) != 1 || got[0].Status != pay.StatusPending {
		t.Fatalf("未广播处理中回执: %+v", got)
	}
}

// TestBscRejectsBadChainFieldsWithoutSending 地址或备注不合链上规则的单，
// 必须一次请求都不发出、一条回执都不广播。
func TestBscRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := BscWithdraw(nil, &pay.PayOrder{OrderNo: "O9", Amount: 10, Address: evmBogusChecksum}); err == nil {
		t.Error("校验和不过的提现地址应当被拒")
	}
	if _, err := BscWithdraw(nil, &pay.PayOrder{OrderNo: "O8", Amount: 10, Address: evmAllLower, Memo: "8890"}); err == nil {
		t.Error("EVM 转账带备注的提现应当被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("链上字段不合法却发出了 %d 次请求", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("前置校验失败却广播了回执: %+v", got)
	}
}

// TestAccountQueryReadsMerchantSnapshot 账户查询读回整数快照，币种缺省补 BNB；
// 读操作不广播回执。
func TestAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	info, err := BscAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Balance != 10 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencyBNB {
		t.Fatalf("币种缺省未补: %+v", info)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("账户查询发出 %d 次请求，期望 1", len(reqs))
	}
	if !strings.Contains(reqs[0].body, `"`+"merchant_id"+`":"`+merchantID) {
		t.Fatalf("账户报文缺路由字段: %s", reqs[0].body)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}
