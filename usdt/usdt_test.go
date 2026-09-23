package usdt

import (
	"context"
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
	// testContract 用官方 USDT-TRC20 合约：它是广播时真正的收端之一，
	// 编一个「看着像」的字符串会让本包的启动校验与全部出款用例都失去意义。
	testContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	testNetwork  = pay.NetworkMainnet
)

// recorder 收集 publish 广播出去的回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面按 CallUsdtMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallUsdtMsg+kind, func(res *pay.PayResult) {
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

// payCfg 组一份「pay_sdks 登记一段 + usdt 段引用它」的完整配置。
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
		Usdt: &config.UsdtConfig{
			Provider: &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Network:  testNetwork,
			Contract: testContract,
		},
	}
}

// startWith 用给定配置走一遍真实启动路径（含密钥钩子与 SDK 引用解析）。
func startWith(t *testing.T, cfg *config.Cfg) {
	t.Helper()
	UsdtStop(nil)
	recorded()
	UsdtStart(corectx.WithCfg(context.Background(), cfg))
	t.Cleanup(func() {
		UsdtStop(nil)
		recorded()
	})
}

// TestNewClientFailClosed 引用指不通的链路一律不启动：SDK 段没开、商户账户没点名、
// 驱动标记写错，都只会在供应商侧留下验签失败，本地却看起来一切正常。
func TestNewClientFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 usdt 段", func(c *config.Cfg) { c.Usdt = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Usdt.Provider = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Usdt.Provider.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[testSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "no_such_driver" }},
		{"多账户没点名", func(c *config.Cfg) {
			c.PaySDks[testSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "M2"}
			c.Usdt.Provider.Account = ""
		}},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[testSection].Accounts[testAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[testSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[testSection].SecretKey = "" }},
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
	// 只有一个账户时允许不点名；多个账户时点名即可启用备用商户号。
	cfg := full()
	cfg.Usdt.Provider.Account = ""
	if _, ok := newClient(cfg); !ok {
		t.Error("唯一账户时 account 留空应当可启动")
	}
	cfg = full()
	cfg.PaySDks[testSection].Accounts["reserve"] = &config.PayMerchantAccount{Enable: "on", MerchantID: "MCH-RESERVE"}
	cfg.Usdt.Provider.Account = "reserve"
	c, ok := newClient(cfg)
	if !ok {
		t.Fatal("点名备用账户后应当可启动")
	}
	_ = c
}

// TestOrderDefaultsFromChainConfig 网络标识与币种缺省由链路配置补齐，
// 调用方显式给的值不得被覆盖，交进来的订单对象也不得被就地改动。
func TestOrderDefaultsFromChainConfig(t *testing.T) {
	c, ok := newClient(payCfg("https://pay.invalid", nil))
	if !ok {
		t.Fatal("装配失败")
	}
	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000, Address: "TAddr1"}
	got := c.order(o)
	if got.Currency != pay.CurrencyUSDT || got.Network != testNetwork {
		t.Fatalf("缺省补齐不符: %+v", got)
	}
	if o.Currency != "" || o.Network != "" {
		t.Fatalf("就地改了调用方的订单: %+v", o)
	}
	other := c.order(&pay.PayOrder{OrderNo: "O2", Amount: 1, Currency: pay.CurrencyTON, Network: "mainnet"})
	if other.Currency != pay.CurrencyTON || other.Network != "mainnet" {
		t.Fatalf("显式字段被配置覆盖: %+v", other)
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	UsdtStop(nil)
	recorded()
	if _, err := UsdtRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Error("未启动时充值应当被拒")
	}
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: "a"}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := UsdtQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := UsdtAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
	UsdtStop(nil) // 幂等
}

// TestOrderPreChecks 下单前置校验在发出请求之前就生效。
func TestOrderPreChecks(t *testing.T) {
	// 用一份引用齐备但没有任何端点的配置：真发出请求就会因缺端点报错，
	// 前置校验应当先于端点判定拦下来。
	startWith(t, payCfg("https://pay.invalid", nil))
	recorded()
	if _, err := UsdtWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil ||
		!strings.Contains(err.Error(), "地址") {
		t.Errorf("提现缺地址未被拦下: %v", err)
	}
	if _, err := UsdtRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 0}); err == nil {
		t.Error("零金额未被拦下")
	}
	if _, err := UsdtRecharge(nil, &pay.PayOrder{Amount: 1}); err == nil {
		t.Error("空幂等键未被拦下")
	}
	if _, err := UsdtQuery(nil, "   "); err == nil {
		t.Error("空订单号未被拦下")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("前置校验失败却广播了回执: %+v", got)
	}
}
