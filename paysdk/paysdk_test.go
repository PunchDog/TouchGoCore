package paysdk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

const (
	section    = "supplier_a"
	account    = "default"
	secret     = "TEST_SDK_SECRET"
	merchantID = "MCH-0001"
)

// spyChannel 是注册表里的替身驱动：只负责把「装配出来的选项」原样交回来，
// 一次 HTTP 都不发。真实供应商链路（签名、报文、包络）在 pay 包与三条通道包里测。
type spyChannel struct {
	opt pay.ProviderOptions
}

func (s *spyChannel) Driver() string     { return s.opt.Driver }
func (s *spyChannel) MerchantID() string { return s.opt.MerchantID }

func (s *spyChannel) Recharge(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, errors.New("替身驱动不发起请求")
}
func (s *spyChannel) Withdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, errors.New("替身驱动不发起请求")
}
func (s *spyChannel) QueryOrder(context.Context, string) (*pay.PayResult, error) {
	return nil, errors.New("替身驱动不发起请求")
}
func (s *spyChannel) QueryAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, errors.New("替身驱动不发起请求")
}

// registerSpy 登记一个唯一命名的替身驱动，返回驱动名与最后一次装配看到的选项。
func registerSpy(t *testing.T) (string, *spyChannel) {
	t.Helper()
	name := "spy_" + t.Name()
	spy := &spyChannel{}
	if err := pay.Register(name, func(opt pay.ProviderOptions) (pay.Channel, error) {
		*spy = spyChannel{opt: opt}
		return spy, nil
	}); err != nil {
		t.Fatal(err)
	}
	return name, spy
}

// sdkCfg 组一份「pay_sdks 登记一段 + 通道段引用它」的最小配置。
func sdkCfg(driver string, accounts map[string]*config.PayMerchantAccount) *config.Cfg {
	return &config.Cfg{
		PaySDks: map[string]*config.PaySDKConfig{
			section: {
				Enable: "on", Driver: driver, BaseURL: "https://supplier.example",
				AppID: "app1", SecretKey: secret, TimeoutSec: 30, MaxRetries: 2,
				NotifyURL: "https://example.com/cb", Accounts: accounts,
			},
		},
	}
}

func oneAccount() map[string]*config.PayMerchantAccount {
	return map[string]*config.PayMerchantAccount{
		account: {Enable: "on", MerchantID: merchantID},
	}
}

func ref() *config.PaySDKRef { return &config.PaySDKRef{SDK: section, Account: account} }

// TestOpenRoutesByDriver 装配结果必须逐字段来自被引用的 SDK 段：
// 通道包只给引用，配置表里的任何一项漏转，到了 pay 侧都是「看起来配了、实际用默认值」。
func TestOpenRoutesByDriver(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	res, err := Open(cfg, "usdt", ref(), []pay.ExtraField{{Name: "contract", Value: "TR7"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Section != section || res.Driver != driver || res.MerchantID != merchantID {
		t.Fatalf("装配结果不符: %+v", res)
	}
	opt := spy.opt
	if opt.Name != "usdt" || opt.BaseURL != "https://supplier.example" || opt.AppID != "app1" ||
		opt.SecretKey != secret || opt.NotifyURL != "https://example.com/cb" ||
		opt.MaxRetry != 2 || opt.Timeout != 30*time.Second {
		// 只列缺项，不整体打印选项：里面就有密钥。
		t.Fatalf("SDK 段字段未完整交给驱动: name=%s base_url=%s app_id=%s max_retry=%d timeout=%v",
			opt.Name, opt.BaseURL, opt.AppID, opt.MaxRetry, opt.Timeout)
	}
	if len(opt.Extras) != 1 || opt.Extras[0].Name != "contract" {
		t.Fatalf("通道特有字段未透传: %+v", opt.Extras)
	}
	if path, ok := opt.Endpoint(pay.EndpointQuery); ok {
		t.Fatalf("端点表为空时不应取到路径，实得 %q", path)
	}
}

// TestOpenInjectsSecretBeforeConstruct 密钥钩子按 SDK 段名触发，且发生在构造之前：
// pay 侧见到空密钥会直接拒绝启动，晚一步注入这条链路就永远起不来。
func TestOpenInjectsSecretBeforeConstruct(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	cfg.PaySDks[section].SecretKey = ""
	id := util.DefaultCallFunc.Register(util.CallPaySDKMsg+section, func(sk *string) { *sk = secret })
	defer util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+section, id)

	if _, err := Open(cfg, "usdt", ref(), nil); err != nil {
		t.Fatalf("钩子注入密钥后应当装配成功: %v", err)
	}
	if spy.opt.SecretKey != secret {
		t.Fatal("驱动拿到的密钥不是钩子注入的那一份")
	}
}

// TestOpenNeedsAccountButServiceDoesNot 出款链路必须点到一个可用商户账户；
// 只做验证码下发/登录的链路没有账户也要能用，否则配置里就会被塞进一个假商户号。
func TestOpenNeedsAccountButServiceDoesNot(t *testing.T) {
	driver, spy := registerSpy(t)
	noAccounts := sdkCfg(driver, nil)
	if _, err := Open(noAccounts, "usdt", ref(), nil); err == nil {
		t.Fatal("没有商户账户的出款链路应当拒绝装配")
	}
	res, err := OpenService(noAccounts, "whatsapp.login", &config.PaySDKRef{SDK: section})
	if err != nil {
		t.Fatalf("无账户的下发链路应当可装配: %v", err)
	}
	if res.MerchantID != "" {
		t.Fatalf("无账户时下发了商户号: %+v", res)
	}
	// 一旦配了账户，歧义规则与资金链路一致：多个账户必须点名。
	multi := sdkCfg(driver, map[string]*config.PayMerchantAccount{
		"default": {Enable: "on", MerchantID: "M1"},
		"reserve": {Enable: "on", MerchantID: "M2"},
	})
	if _, err := OpenService(multi, "whatsapp.login", &config.PaySDKRef{SDK: section}); err == nil {
		t.Fatal("多账户未点名应当拒绝装配")
	}
	if _, err := OpenService(multi, "whatsapp.login", &config.PaySDKRef{SDK: section, Account: "reserve"}); err != nil {
		t.Fatalf("点名后应当可装配: %v", err)
	}
	if spy.opt.MerchantID != "M2" {
		t.Fatalf("点名的账户没生效: %+v", spy.opt.MerchantID)
	}
}

// TestOpenFailClosedAndQuiet 每一类引用错误都要报错，且报错文案里不出现密钥：
// 错误会被上层记进日志，配置错不该顺带把凭证广播出去。
func TestOpenFailClosedAndQuiet(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Cfg
		ref  *config.PaySDKRef
	}{
		{"没有配置", nil, ref()},
		{"引用为空", sdkCfg(pay.DriverGeneric, oneAccount()), &config.PaySDKRef{}},
		{"段不存在", sdkCfg(pay.DriverGeneric, oneAccount()), &config.PaySDKRef{SDK: "ghost"}},
		{"段没开", func() *config.Cfg {
			c := sdkCfg(pay.DriverGeneric, oneAccount())
			c.PaySDks[section].Enable = "off"
			return c
		}(), ref()},
		{"缺驱动标记", func() *config.Cfg {
			c := sdkCfg(pay.DriverGeneric, oneAccount())
			c.PaySDks[section].Driver = ""
			return c
		}(), ref()},
		{"驱动未登记", sdkCfg("no_such_driver", oneAccount()), ref()},
		{"空密钥且无人注入", func() *config.Cfg {
			c := sdkCfg(pay.DriverGeneric, oneAccount())
			c.PaySDks[section].SecretKey = ""
			return c
		}(), ref()},
		{"账户停用", func() *config.Cfg {
			c := sdkCfg(pay.DriverGeneric, oneAccount())
			c.PaySDks[section].Accounts[account].Enable = "off"
			return c
		}(), ref()},
	}
	for _, cs := range cases {
		res, err := Open(cs.cfg, "usdt", cs.ref, nil)
		if err == nil || res != nil {
			t.Errorf("%s 应当拒绝装配，实得 res=%v err=%v", cs.name, res, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s 的报错泄漏了密钥: %v", cs.name, err)
		}
		if !strings.Contains(err.Error(), "usdt") {
			t.Errorf("%s 的报错没指出是哪条通道: %v", cs.name, err)
		}
	}
}

// TestSecondsLeavesDefaultsToPay 超时留空时交出 0，由 pay 用它的默认值：
// 这里再立一套默认秒数，两处默认迟早会对不上。
func TestSecondsLeavesDefaultsToPay(t *testing.T) {
	driver, spy := registerSpy(t)
	cfg := sdkCfg(driver, oneAccount())
	cfg.PaySDks[section].TimeoutSec = 0
	if _, err := Open(cfg, "usdt", ref(), nil); err != nil {
		t.Fatal(err)
	}
	if spy.opt.Timeout != 0 {
		t.Fatalf("超时应按配置留空交给 pay，实得 %v", spy.opt.Timeout)
	}
}
