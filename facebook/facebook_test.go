package facebook

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"touchgocore/config"
	"touchgocore/corectx"
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
	loginSection = "test_fb"
)

// recorder 收集 util.CallFacebookMsg+Kind 上的回执。
//
// 资金门面已不再广播（publish 随链路一起删除），这里留着是给登录侧读操作用例
// 当「不许自广播」的探针：一旦有人把广播接回来，这些用例会立刻变红。
var recorder = struct {
	mu    sync.Mutex
	calls []*pay.PayResult
}{}

func init() {
	for _, kind := range []string{"Withdraw", "Query"} {
		util.DefaultCallFunc.Register(util.CallFacebookMsg+kind, func(res *pay.PayResult) {
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
// provider 引用留着是有意的：config 仍按出款口径校验它，装配侧不再碰它，
// 于是同一份配置既能测「登录链路照常起」，又能测「资金门面回哨兵且零请求」。
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
		Facebook: &config.FacebookConfig{
			Login:     &config.PaySDKRef{SDK: testSection, Account: testAccount},
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

// TestLoginChainNeedsNoMerchantAccount 只做验证码下发的 SDK 本就没有商户账户：
// 按出款口径要求它填一个，只会逼配置里写个假商户号。
func TestLoginChainNeedsNoMerchantAccount(t *testing.T) {
	cfg := payCfg("https://pay.invalid", nil)
	delete(cfg.PaySDks, testSection)
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: "https://pay.invalid",
		AppID: "app1", SecretKey: testSecret,
	}
	cfg.Facebook.Login = &config.PaySDKRef{SDK: loginSection}
	if _, ok := openLogin(cfg, cfg.Facebook.Login); !ok {
		t.Error("无商户账户的下发链路应当可启动")
	}
}

// TestLoginSectionPassesValidateAndStart 没有商户账户的下发段要能通过 config.Validate
// 并真的启动起来：validate 认得 facebook.login 是 service 分支（不要求 accounts），
// 而装配走的是 pay.OpenService 这条同样的口径。任一处漏认，
// 「只接一家消息网关」这个形态就配不出来。
func TestLoginSectionPassesValidateAndStart(t *testing.T) {
	cfg := payCfg("https://pay.invalid", nil)
	cfg.PaySDks[loginSection] = &config.PaySDKConfig{
		Enable: "on", Driver: pay.DriverGeneric, BaseURL: "https://pay.invalid",
		AppID: "app1", SecretKey: testSecret,
		// 刻意不配 accounts：验证码网关只管发消息，没有出账主体。
	}
	cfg.Facebook.Login = &config.PaySDKRef{SDK: loginSection}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("下发段没有 accounts 不该被判成配置错: %v", err)
	}
	FacebookStop(nil)
	defer FacebookStop(nil)
	FacebookStart(corectx.WithCfg(context.Background(), cfg))
	if login.Load() == nil {
		t.Error("引用无账户下发段的登录链路应当启动")
	}
	if err := FacebookSendCode(nil, "86138"); err == nil ||
		!strings.Contains(err.Error(), "endpoints."+pay.EndpointSendCode) {
		t.Errorf("该段没登记任何端点，发码应缺端点被拒，实得: %v", err)
	}
}

// TestOperationsRefuseWhenNotStarted 未启动时门面函数返回错误而不是 panic。
func TestOperationsRefuseWhenNotStarted(t *testing.T) {
	FacebookStop(nil)
	if err := FacebookSendCode(nil, "86138"); err == nil {
		t.Error("未启动时发码应当被拒")
	}
	if _, err := FacebookFriends(nil, "86138"); err == nil {
		t.Error("未启动时查好友列表应当被拒")
	}
	if _, err := FacebookLogin(nil, "86138", "123456"); err == nil {
		t.Error("未启动时登录应当被拒")
	}
}

// TestPayoutInterfaceStaysWired 三个资金门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = FacebookWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = FacebookQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = FacebookAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, Currency: pay.CurrencyUSDT, Address: "TTargetAddr"}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := FacebookWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := FacebookQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := FacebookAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := FacebookWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
	// 空订单号也一样：收口前后都是同一个哨兵，不给「参数校验错」留第二条出口。
	if _, err := FacebookQuery(nil, "  "); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单号的拒绝口径不符: %v", err)
	}
}
