package sol

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
	// testSPLToken 是 SPL 代币合约地址的夹具。合约留空代表原生 SOL，
	// 也是本通道的默认形态，两种都要测到。
	testSPLToken = "4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"
	testNetwork  = pay.NetworkMainnet
)

// Solana 地址夹具：base58 解码后恰为 32 字节的 ed25519 公钥形态。
// 三个向量都是先定字节（1..32 递增、全 FF、按公式生成）再现编码并 roundtrip 核过的，
// 不是抄来的——抄来的串一旦本身解不出 32 字节，测试就会把长度判断的错误永远藏住。
const (
	solBytes1To32  = "4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"
	solAllFFAcct   = "JEKNVnkbo3jma5nREBBJCDoXFVeKkD56V3xKrvRmWxFG"
	solPatternAcct = "5PzNBPqQ1c4QcMg9zC2G5eF1P3nZRbZNWf471X2mYtBh"
)

// recorder 收集 publish 广播出去的回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallSolMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallSolMsg+kind, func(res *pay.PayResult) {
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

// payCfg 组一份「pay_sdks 登记一段 + sol 段引用它」的完整配置。
//
// 通道段只留 sdk/account 两个名字，其余全在 SDK 段里：这正是本仓的装配形态，
// 测试若绕过 paysdk 直接喂 ProviderOptions，就测不到「引用指不通」这一整类错误。
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
		Sol: &config.SolConfig{
			Provider: &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Network:  testNetwork,
		},
	}
}

// startWith 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	SolStop(nil)
	recorded()
	SolStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		SolStop(nil)
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

// TestBase58DecodeRoundtrip 夹具的来历单独钉一次：解码后必须恰好 32 字节，
// 且内容与造向量时的原始字节一致——Solana 没有内嵌校验和，长度是本地唯一能核的性质。
func TestBase58DecodeRoundtrip(t *testing.T) {
	raw, err := base58Decode(solBytes1To32)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solBytes1To32, len(raw), err)
	}
	for i, b := range raw {
		if b != byte(i+1) {
			t.Fatalf("第 %d 字节=%02x，期望递增序列", i, b)
		}
	}
	raw, err = base58Decode(solAllFFAcct)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solAllFFAcct, len(raw), err)
	}
	for i, b := range raw {
		if b != 0xFF {
			t.Fatalf("第 %d 字节=%02x，期望 FF", i, b)
		}
	}
}

// TestIsValidSolanaAddress 32 字节 base58 的正负用例。
func TestIsValidSolanaAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{solBytes1To32, true, "1..32 递增字节"},
		{solAllFFAcct, true, "全 FF 字节"},
		{solPatternAcct, true, "公式生成字节"},
		{"  " + solBytes1To32 + "  ", true, "两侧空白应被 trim 后过关"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{solBytes1To32[:42], false, "少一位（解码不足 32 字节）"},
		{solAllFFAcct + "1", false, "尾部多一个前导零字符（解码 33 字节）"},
		{"1" + solBytes1To32, false, "首部多一个前导零字符"},
		// 0OIl 不在 Base58 字母表里，混进来必须当场拒。
		{"0wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 0"},
		{"OwBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 O"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4VigI", false, "含非法字符 I"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigl", false, "含非法字符 l"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A", false, "EVM 地址不是 Solana 地址"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 Solana 地址"},
	}
	// 逐位篡改：Solana 没有校验和，改一位仍然可能是「另一个格式合法的地址」——
	// 这里钉的是格式判定不会被篡改绕过，字母表外的替换必须拒。
	tampered := []rune(solPatternAcct)
	for i := range tampered {
		c := []rune{'0', 'O'}[i%2]
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + string(c) + string(tampered[i+1:]), false, "逐位替换为字母表外字符"})
	}
	for _, cs := range cases {
		if got := IsValidSolanaAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidSolanaAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestSOLRuleRegistered 本包必须把 SOL 规则登记进 pay 的注册表。
func TestSOLRuleRegistered(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencySOL)
	if !ok {
		t.Fatalf("SOL 规则未登记，已登记的是 %v", pay.CurrenciesWithRules())
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: solBytes1To32}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: solBytes1To32[:42]}); err == nil {
		t.Error("长度不足的地址未被拦下")
	}
}

// TestSOLRuleRejectsMemo Solana 原生与 SPL 转账都没有 TON comment 那种认款机制：
// 填了就拒而不是忽略。
func TestSOLRuleRejectsMemo(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencySOL)
	if !ok {
		t.Fatal("SOL 规则未登记")
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

// TestNewClientFailClosed 引用指不通、SPL 合约不合形态的链路一律不启动。
func TestNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 sol 段", func(c *config.Cfg) { c.Sol = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Sol.Provider = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Sol.Provider.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[testSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.PaySDks[testSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "M2"}
			c.Sol.Provider.Account = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[testSection].Accounts[testAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[testSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[testSection].SecretKey = "" }},
		{"SPL 位填了 EVM 地址", func(c *config.Cfg) { c.Sol.Token = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A" }},
		{"SPL 位填了短串", func(c *config.Cfg) { c.Sol.Token = solBytes1To32[:20] }},
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
	cfg := full()
	cfg.Sol.Token = testSPLToken
	if _, ok := newClient(cfg); !ok {
		t.Error("合法 SPL 合约应当可启动")
	}
}

// TestOrderDefaultsFromChainConfig 网络标识与币种缺省由链路配置补齐，
// 调用方显式给的值不得被覆盖，交进来的订单对象也不得被就地改动。
func TestOrderDefaultsFromChainConfig(t *testing.T) {
	c, ok := newClient(payCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000000, Address: solBytes1To32}
	got := c.order(o)
	if got.Currency != pay.CurrencySOL || got.Network != testNetwork {
		t.Fatalf("缺省补齐不符: %+v", got)
	}
	if o.Currency != "" || o.Network != "" {
		t.Fatalf("就地改了调用方的订单: %+v", o)
	}
	other := c.order(&pay.PayOrder{OrderNo: "O2", Amount: 1, Currency: pay.CurrencyUSDT, Network: pay.NetworkDevnet})
	if other.Currency != pay.CurrencyUSDT || other.Network != pay.NetworkDevnet {
		t.Fatalf("显式字段被配置覆盖: %+v", other)
	}
}

// TestStartNilCfgDoesNotPanic 没有任何 sol 配置时启动必须安静通过。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	SolStop(nil)
	SolStart(corectx.WithCfg(context.Background(), nil))
	SolStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	SolStart(nil)
	SolStart(corectx.WithCfg(context.Background(), &config.Cfg{Sol: &config.SolConfig{}}))
	if _, err := SolQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	SolStop(nil)
	SolStop(nil) // 幂等
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	SolStop(nil)
	recorded()
	if _, err := SolRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Error("未启动时充值应当被拒")
	}
	if _, err := SolWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: solBytes1To32}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := SolQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := SolAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
	SolStop(nil)
}

// TestRechargeSignsAndSerializes 原生 SOL 下单（未配合约）：报文字段与签名域一致，
// 金额以整数最小单位（9 位 lamports）原样上送，十个定长签名位后面没有追加。
func TestRechargeSignsAndSerializes(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000000000}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := SolRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000000000})
	if err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	if res.Status != pay.StatusSuccess || res.TradeNo != "T1" || res.Amount != 1000000000 {
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
	if body["currency"] != pay.CurrencySOL || body["network"] != testNetwork {
		t.Fatalf("链上字段未进报文: %s", req.body)
	}
	if _, has := body["contract"]; has {
		t.Fatalf("未配合约却发出了 contract 字段: %s", req.body)
	}
	if strings.Contains(req.body, testSecret) {
		t.Fatalf("密钥进了报文: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "1000000000", pay.CurrencySOL, testNetwork, "", "", "", "")
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestSPLTokenAppendsContractToBodyAndSign 配置了 SPL 合约时，extras 键名沿用
// contract（与 usdt 侧口径一致），且必须既进报文又追加在签名域末尾。
func TestSPLTokenAppendsContractToBodyAndSign(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","status":"success","amount":10}}`
	cfg := payCfg(url, allEndpoints())
	cfg.Sol.Token = testSPLToken
	startWith(t, cfg)
	recorded()

	if _, err := SolRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 10}); err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"contract":"`+testSPLToken+`"`) {
		t.Fatalf("合约未进报文: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "10", pay.CurrencySOL, testNetwork, "", "", "", "",
		testSPLToken)
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

	res, err := SolWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, Address: solAllFFAcct})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending || res.IsFinal() {
		t.Fatalf("处理中不应是终态: %+v", res)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"address":"`+solAllFFAcct+`"`) {
		t.Fatalf("提现报文缺地址: %s", req.body)
	}
	wantSign := pay.SignMD5(testSecret,
		"app1", merchantID, "O2", "500", pay.CurrencySOL, testNetwork, solAllFFAcct, "", "", "")
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	if got := recorded(); len(got) != 1 || got[0].Status != pay.StatusPending {
		t.Fatalf("未广播处理中回执: %+v", got)
	}
}

// TestSolRejectsBadChainFieldsWithoutSending 地址或备注不合链上规则的单，
// 必须一次请求都不发出、一条回执都不广播。
func TestSolRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := SolWithdraw(nil, &pay.PayOrder{OrderNo: "O9", Amount: 10, Address: solBytes1To32[:42]}); err == nil {
		t.Error("长度不足提现地址应当被拒")
	}
	if _, err := SolWithdraw(nil, &pay.PayOrder{OrderNo: "O8", Amount: 10, Address: solBytes1To32, Memo: "8890"}); err == nil {
		t.Error("SOL 转账带备注的提现应当被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("链上字段不合法却发出了 %d 次请求", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("前置校验失败却广播了回执: %+v", got)
	}
}

// TestAccountQueryReadsMerchantSnapshot 账户查询读回整数快照，币种缺省补 SOL；
// 读操作不广播回执。
func TestAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	info, err := SolAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Balance != 10 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencySOL {
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
