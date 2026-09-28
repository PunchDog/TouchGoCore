package tron

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
	// testTRC10 是 TRC10 代币 id 的夹具形态：纯数字串。
	// 合约字段留空代表原生 TRX，也是本通道的默认形态，两种都要测到。
	testTRC10   = "1000101"
	testNetwork = pay.NetworkMainnet
)

// TRON 地址夹具：官方 USDT-TRC20 合约（版本字节 0x41、校验和已核），
// 以及用 20 字节账户哈希现算校验和造出的几个。造向量而不是抄网上的：
// 抄来的地址一旦本身是编的，测试就会把「校验和不过」永远藏住。
const (
	tronOfficialUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	tronAllZeroAcct  = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
	tronRangeAcct    = "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"
	tronFFAcct       = "TZJozAg1ruapycCicgz31GxvYJ1FraLjZa"
	// tronBogusChecksum 长度与版本字节全对，只有末尾 4 字节校验和不过。
	tronBogusChecksum = "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR"
)

// recorder 收集 publish 广播出去的回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallTronMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallTronMsg+kind, func(res *pay.PayResult) {
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

// payCfg 组一份「pay_sdks 登记一段 + tron 段引用它」的完整配置。
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
		Tron: &config.TronConfig{
			Provider: &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Network:  testNetwork,
		},
	}
}

// startWith 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	TronStop(nil)
	recorded()
	TronStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		TronStop(nil)
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

// TestIsValidTRONAddress 地址校验的正负用例：合法向量全过，
// 长度、版本字节、字母表、校验和任一不合都当场拒。
func TestIsValidTRONAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{tronOfficialUSDT, true, "官方 USDT-TRC20 合约"},
		{tronAllZeroAcct, true, "全零账户哈希"},
		{tronRangeAcct, true, "递增账户哈希"},
		{tronFFAcct, true, "全 F 账户哈希"},
		{tronBogusChecksum, false, "校验和不符的编造地址"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{tronRangeAcct[:33], false, "少一位"},
		{tronRangeAcct + "1", false, "多一位"},
		{strings.ReplaceAll(tronRangeAcct, "T", "1"), false, "首字符换成 1：前导零让解码长度变成 26 字节"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6O", false, "含非法字符 O"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6l", false, "含非法字符 l"},
		{"0x76d5820a24a2089091721d40a4d20680804b6229", false, "EVM 地址不是 TRON 地址"},
	}
	// 逐位篡改：每一位换成另一个合法字符都得被校验和判出来。
	tampered := []rune(tronRangeAcct)
	for i := range tampered {
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + "X" + string(tampered[i+1:]), false, "逐位篡改"})
	}
	for _, cs := range cases {
		if got := IsValidTRONAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidTRONAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestTRXRuleRegistered 本包必须把 TRX 规则登记进 pay 的注册表：
// 地址规则归属链包，usdt 侧只剩薄包装，这里的接线断了两边会同时失去校验。
func TestTRXRuleRegistered(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyTRX)
	if !ok {
		t.Fatalf("TRX 规则未登记，已登记的是 %v", pay.CurrenciesWithRules())
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: tronRangeAcct}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: tronBogusChecksum}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestTRXRuleRejectsMemo TRON 原生转账没有 memo 概念：填了就拒而不是忽略，
// 上游把 TON 的认款机制套到这条链上是错配，静默吞掉等于放它去别处发作。
func TestTRXRuleRejectsMemo(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyTRX)
	if !ok {
		t.Fatal("TRX 规则未登记")
	}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	// 纯空白按「没有备注」处理：真正不发出去的那一步在 NewOrderRequest 的 trim。
	if err := rule.CheckMemo(&pay.PayOrder{Memo: "   "}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestNewClientFailClosed 引用指不通、代币 id 形态不对的链路一律不启动：
// TRX 出款宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
func TestNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 tron 段", func(c *config.Cfg) { c.Tron = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Tron.Provider = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Tron.Provider.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[testSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.PaySDks[testSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "M2"}
			c.Tron.Provider.Account = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[testSection].Accounts[testAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[testSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[testSection].SecretKey = "" }},
		{"TRC10 位填了 TRC20 地址", func(c *config.Cfg) { c.Tron.Token = tronOfficialUSDT }},
		{"TRC10 位填了带字母的串", func(c *config.Cfg) { c.Tron.Token = "1000a01" }},
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
	// 纯数字 TRC10 id 合法：原生 TRX 之外也要能挂得上代币。
	cfg := full()
	cfg.Tron.Token = testTRC10
	if _, ok := newClient(cfg); !ok {
		t.Error("TRC10 纯数字 id 应当可启动")
	}
}

// TestOrderDefaultsFromChainConfig 网络标识与币种缺省由链路配置补齐，
// 调用方显式给的值不得被覆盖，交进来的订单对象也不得被就地改动。
func TestOrderDefaultsFromChainConfig(t *testing.T) {
	c, ok := newClient(payCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000, Address: tronRangeAcct}
	got := c.order(o)
	if got.Currency != pay.CurrencyTRX || got.Network != testNetwork {
		t.Fatalf("缺省补齐不符: %+v", got)
	}
	if o.Currency != "" || o.Network != "" {
		t.Fatalf("就地改了调用方的订单: %+v", o)
	}
	other := c.order(&pay.PayOrder{OrderNo: "O2", Amount: 1, Currency: pay.CurrencyUSDT, Network: pay.NetworkNile})
	if other.Currency != pay.CurrencyUSDT || other.Network != pay.NetworkNile {
		t.Fatalf("显式字段被配置覆盖: %+v", other)
	}
}

// TestStartNilCfgDoesNotPanic 没有任何 tron 配置时启动必须安静通过，
// 且后续操作被拒而不是拿着空链路发请求。
func TestStartNilCfgDoesNotPanic(t *testing.T) {
	TronStop(nil)
	TronStart(corectx.WithCfg(context.Background(), nil))
	TronStart(corectx.WithCfg(context.Background(), &config.Cfg{}))
	TronStart(nil)
	TronStart(corectx.WithCfg(context.Background(), &config.Cfg{Tron: &config.TronConfig{}}))
	if _, err := TronQuery(nil, "O1"); err == nil {
		t.Fatal("未启动时查询应当被拒")
	}
	TronStop(nil)
	TronStop(nil) // 幂等
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	TronStop(nil)
	recorded()
	if _, err := TronRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Error("未启动时充值应当被拒")
	}
	if _, err := TronWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: tronRangeAcct}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := TronQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := TronAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
	TronStop(nil)
}

// TestRechargeSignsAndSerializes 原生 TRX 下单（未配代币）：报文字段与签名域一致，
// 金额以整数最小单位（6 位）上送，签名域十个定长位后面没有追加。
func TestRechargeSignsAndSerializes(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","trade_no":"T1","status":"success","amount":1000000}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := TronRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1000000})
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
	if body["currency"] != pay.CurrencyTRX || body["network"] != testNetwork {
		t.Fatalf("链上字段未进报文: %s", req.body)
	}
	if _, has := body["contract"]; has {
		t.Fatalf("未配代币却发出了 contract 字段: %s", req.body)
	}
	if amt, ok := body["amount"].(float64); !ok || int64(amt) != 1000000 {
		t.Fatalf("金额不是整数最小单位: %s", req.body)
	}
	if strings.Contains(req.body, testSecret) {
		t.Fatalf("密钥进了报文: %s", req.body)
	}
	// 签名域十个定长标量：此单收款侧字段全是空串占位——
	// 空串也要占位，否则后面每一位的落点都会整体偏移。
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "1000000", pay.CurrencyTRX, testNetwork, "", "", "", "")
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
	pub := recorded()
	if len(pub) != 1 || pub[0].OrderNo != "O1" {
		t.Fatalf("回执广播=%+v", pub)
	}
}

// TestTRC10TokenAppendsContractToBodyAndSign 配置了 TRC10 代币 id 时，
// contract 必须既进报文又追加在签名域末尾：字段发出去了却没签进去，
// 供应商会整批拒签，而且本地永远查不出来。
func TestTRC10TokenAppendsContractToBodyAndSign(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/recharge"] = `{"code":"0","data":{"order_no":"O1","status":"success","amount":10}}`
	cfg := payCfg(url, allEndpoints())
	cfg.Tron.Token = testTRC10
	startWith(t, cfg)
	recorded()

	if _, err := TronRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 10}); err != nil {
		t.Fatalf("充值失败: %v", err)
	}
	req := f.seen()[0]
	if !strings.Contains(req.body, `"contract":"`+testTRC10+`"`) {
		t.Fatalf("代币 id 未进报文: %s", req.body)
	}
	want := pay.SignMD5(testSecret,
		"app1", merchantID, "O1", "10", pay.CurrencyTRX, testNetwork, "", "", "", "",
		testTRC10)
	if req.sign != want {
		t.Fatalf("签名=%s，期望 %s", req.sign, want)
	}
}

// TestWithdrawRequiresAddressOnWire 提现单把收款地址带进报文与签名域。
// 地址定「钱到哪去」，不在签名域里就等于谁都能把钱转到别的地址上。
func TestWithdrawRequiresAddressOnWire(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/withdraw"] = `{"code":"0","data":{"order_no":"O2","status":"pending"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	res, err := TronWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 500, Address: tronFFAcct})
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
	wantSign := pay.SignMD5(testSecret,
		"app1", merchantID, "O2", "500", pay.CurrencyTRX, testNetwork, tronFFAcct, "", "", "")
	if req.sign != wantSign {
		t.Fatalf("签名=%s，期望 %s", req.sign, wantSign)
	}
	if got := recorded(); len(got) != 1 || got[0].Status != pay.StatusPending {
		t.Fatalf("未广播处理中回执: %+v", got)
	}
}

// TestTronRejectsBadChainFieldsWithoutSending 地址或备注不合链上规则的单，
// 必须一次请求都不发出、一条回执都不广播：发出去的最坏结果是钱动了但落错了地方。
func TestTronRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := TronWithdraw(nil, &pay.PayOrder{OrderNo: "O9", Amount: 10, Address: tronBogusChecksum}); err == nil {
		t.Error("校验和不过的提现地址应当被拒")
	}
	if _, err := TronWithdraw(nil, &pay.PayOrder{OrderNo: "O8", Amount: 10, Address: tronRangeAcct, Memo: "8890"}); err == nil {
		t.Error("TRON 原生转账带备注的提现应当被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("链上字段不合法却发出了 %d 次请求", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("前置校验失败却广播了回执: %+v", got)
	}
}

// TestAccountQueryReadsMerchantSnapshot 账户查询把供应商侧读成整数快照，
// 币种缺省补 TRX；这是读操作，不广播回执。
func TestAccountQueryReadsMerchantSnapshot(t *testing.T) {
	f, url := newFakeSupplier(t)
	f.response["/api/account"] = `{"code":"0","data":{"balance":10,"available":7,"frozen":3,"credit":1,"fee_rate_bps":30,"status":"active"}}`
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	info, err := TronAccount(nil, nil)
	if err != nil {
		t.Fatalf("账户查询失败: %v", err)
	}
	if info.Status != pay.AcctActive || info.Balance != 10 || info.Available != 7 || info.FeeRateBps != 30 {
		t.Fatalf("账户快照不符: %+v", info)
	}
	if info.Currency != pay.CurrencyTRX {
		t.Fatalf("币种缺省未补: %+v", info)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("账户查询发出 %d 次请求，期望 1", len(reqs))
	}
	if !strings.Contains(reqs[0].body, `"`+"merchant_id"+`":"`+merchantID) {
		t.Fatalf("账户报文缺路由字段: %s", reqs[0].body)
	}
	if strings.Contains(reqs[0].body, testSecret) {
		t.Fatal("密钥进了报文")
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("账户查询不该广播资金回执: %+v", got)
	}
}
