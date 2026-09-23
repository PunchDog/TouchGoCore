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
	// loginSection 是只做验证码下发、没有商户账户的 SDK 段名。
	loginSection = "test_wa"
)

// recorder 收集 publish 广播出去的回执。
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

// payCfg 组一份「pay_sdks 登记一段 + login/provider 都引用它」的完整配置。
//
// 两段引用同一段是刻意的：同一家的登录网关与支付网关指向同一供应商是最常见形态，
// 装配出的是两个 Provider 对象，会话凭证能不能带过去由 syncSession 的基址规则决定。
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
			Login:     &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Provider:  &config.PaySDKRef{SDK: testSection, Account: testAccount},
			Templates: map[string]string{"auth_verify": "tpl_verify"},
		},
	}
}

// TestSignValuesMatchPayloadOrder 签名域顺序必须与报文里登记的字段的先后一致。
// 报文加了字段而签名域漏掉，供应商会全量拒签，且本地永远查不出来。
func TestSignValuesMatchPayloadOrder(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"下发验证码",
			(&sendCodeRequest{AppID: "app1", Phone: "86138", Template: "tpl"}).signValues(),
			[]string{"app1", "86138", "tpl"}},
		{"登录",
			(&loginRequest{AppID: "app1", Phone: "86138", Code: "123456"}).signValues(),
			[]string{"app1", "86138", "123456"}},
	}
	for _, cs := range cases {
		if strings.Join(cs.got, "|") != strings.Join(cs.want, "|") {
			t.Errorf("%s 签名域=%v，期望 %v", cs.name, cs.got, cs.want)
		}
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

// TestLoginChainNeedsNoMerchantAccount 只做验证码下发的 SDK 本就没有商户账户：
// 按资金链路口径要求它填一个，只会逼配置里写个假商户号。
func TestLoginChainNeedsNoMerchantAccount(t *testing.T) {
	cfg := payCfg("https://pay.invalid", nil)
	delete(cfg.PaySDks, testSection)
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: "https://pay.invalid",
		AppID: "app1", SecretKey: testSecret,
	}
	cfg.Whatsapp.Login = &config.PaySDKRef{SDK: loginSection}
	if _, ok := openLogin(cfg, cfg.Whatsapp.Login); !ok {
		t.Error("无商户账户的下发链路应当可启动")
	}
	// 同一段拿去做出款链路就不行了：出账主体必须明确。
	cfg.Whatsapp.Provider = &config.PaySDKRef{SDK: loginSection}
	if _, ok := openFund(cfg, cfg.Whatsapp.Provider); ok {
		t.Error("没有商户账户的链路不得用于出款")
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
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
	if err := WhatsappSendCode(nil, "86138"); err == nil {
		t.Error("未启动时发码应当被拒")
	}
	if _, err := WhatsappLogin(nil, "86138", "123456"); err == nil {
		t.Error("未启动时登录应当被拒")
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
