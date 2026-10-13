package whatsapp

import (
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

const (
	// testSecret 是测试内用的签名密钥，与真实凭证无关。
	testSecret = "TEST_SECRET_KEY"
	// testSection / testAccount 是测试里 SDK 段与商户账户的名字。
	testSection = "test_a"
	testAccount = "default"
	merchantID  = "MCH-TEST"
)

// recorder 收集 publish 广播出去的资金回执。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	// 资金门面会按 CallWhatsappMsg+Kind 广播回执，测试在此收口，
	// 下游业务的落库回调就是同一个形态。
	for _, kind := range []string{"Recharge", "Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallWhatsappMsg+kind, func(res *pay.PayResult) {
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

// payCfg 组一份「pay_sdks 登记一段 + provider 引用它」的完整配置。
//
// 消息侧改走 whatsmeow 之后本包只剩 provider 一段引用：登录/发码链路已随
// HTTP 供应商一起撤掉（扫码配对不经过任何供应商），资金链路不变。
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
		Whatsapp: &config.WhatsappConfig{
			Provider: &config.PaySDKRef{SDK: testSection, Account: testAccount},
		},
	}
}

// TestOpenFundFailClosed 引用指不通的资金链路不得启动：带着空密钥、没点名的商户账户
// 出去，只会在供应商侧留下验签失败或错账，本地却看起来一切正常。
func TestOpenFundFailClosed(t *testing.T) {
	full := func() *config.Cfg { return payCfg("https://pay.invalid", nil) }
	cases := []struct {
		name   string
		mutate func(*config.Cfg)
	}{
		{"没有 whatsapp 段", func(c *config.Cfg) { c.Whatsapp = nil }},
		{"通道段没引用 SDK", func(c *config.Cfg) { c.Whatsapp.Provider = nil }},
		{"引用的段不存在", func(c *config.Cfg) { c.Whatsapp.Provider.SDK = "ghost" }},
		{"SDK 段没开", func(c *config.Cfg) { c.PaySDks[testSection].Enable = "off" }},
		{"缺驱动标记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "" }},
		{"驱动名没登记", func(c *config.Cfg) { c.PaySDks[testSection].Driver = "no_such_driver" }},
		{"没有商户账户", func(c *config.Cfg) { c.PaySDks[testSection].Accounts = nil }},
		{"商户账户停用", func(c *config.Cfg) { c.PaySDks[testSection].Accounts[testAccount].Enable = "off" }},
		{"缺基址", func(c *config.Cfg) { c.PaySDks[testSection].BaseURL = "" }},
		{"缺密钥", func(c *config.Cfg) { c.PaySDks[testSection].SecretKey = "" }},
	}
	for _, cs := range cases {
		cfg := full()
		cs.mutate(cfg)
		var ref *config.PaySDKRef
		if cfg.Whatsapp != nil {
			ref = cfg.Whatsapp.Provider
		}
		if p, ok := openFund(cfg, ref); ok || p != nil {
			t.Errorf("%s 不应启动，实得 ok=%v", cs.name, ok)
		}
	}
	// 端点表为空但引用齐备：装配成功，但任何操作都应在发请求之前被拒（见 client_test）
	ready := full()
	if _, ok := openFund(ready, ready.Whatsapp.Provider); !ok {
		t.Error("引用齐备时应当启动")
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时资金门面返回错误而不是 panic，
// 且不得广播任何回执。消息侧门面的同型断言在 wa_client_test.go。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	WhatsappStop(nil)
	recorded()
	if _, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil {
		t.Error("未启动时充值应当被拒")
	}
	if _, err := WhatsappWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1, Address: "a"}); err == nil {
		t.Error("未启动时提现应当被拒")
	}
	if _, err := WhatsappQuery(nil, "O1"); err == nil {
		t.Error("未启动时查询应当被拒")
	}
	if _, err := WhatsappAccount(nil, nil); err == nil {
		t.Error("未启动时账户查询应当被拒")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("未启动却广播了回执: %+v", got)
	}
}

// TestOrderPreChecks 下单前置校验在发出请求之前就生效。
func TestOrderPreChecks(t *testing.T) {
	// 用一份引用齐备、但没有任何端点的配置：真发出请求就会因缺端点报错，
	// 前置校验应当先于端点判定拦下来。
	startWith(t, payCfg("https://pay.invalid", nil))
	recorded()
	if _, err := WhatsappWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 1}); err == nil ||
		!strings.Contains(err.Error(), "地址") {
		t.Errorf("提现缺地址未被拦下: %v", err)
	}
	if _, err := WhatsappRecharge(nil, &pay.PayOrder{OrderNo: "O1", Amount: 0}); err == nil {
		t.Error("零金额未被拦下")
	}
	if _, err := WhatsappQuery(nil, "   "); err == nil {
		t.Error("空订单号未被拦下")
	}
	if got := recorded(); len(got) != 0 {
		t.Errorf("前置校验失败却广播了回执: %+v", got)
	}
}
