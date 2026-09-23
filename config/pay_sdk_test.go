package config

import (
	"strings"
	"testing"
)

// sdkTable 是一份可直接引用的 SDK 表：一段单账户、一段多账户、一段被关闭。
func sdkTable() map[string]*PaySDKConfig {
	return map[string]*PaySDKConfig{
		"one": {
			Enable: "on", Driver: "generic_md5", BaseURL: "https://one.example",
			Endpoints: map[string]string{"recharge": "pay/recharge"},
			Accounts: map[string]*PayMerchantAccount{
				"default": {Enable: "on", MerchantID: "MCH-1"},
			},
		},
		"many": {
			Enable: "on", Driver: "generic_md5", BaseURL: "https://many.example",
			Accounts: map[string]*PayMerchantAccount{
				"default": {Enable: "on", MerchantID: "MCH-2"},
				"reserve": {Enable: "off", MerchantID: "MCH-3"},
				"leaky":   {Enable: "on"},
			},
		},
		"off": {Enable: "off", Driver: "generic_md5", BaseURL: "https://off.example"},
	}
}

// TestRefResolve 通道段只带两个名字，取到哪一段、能不能取都由引用说清楚。
func TestRefResolve(t *testing.T) {
	sdks := sdkTable()
	ok := &PaySDKRef{SDK: "one"}
	cfg, acc, err := ok.Resolve(sdks)
	if err != nil {
		t.Fatalf("唯一账户留空应当可解析: %v", err)
	}
	if cfg.BaseURL != "https://one.example" || acc.MerchantID != "MCH-1" {
		t.Fatalf("解析结果不符: %+v / %+v", cfg, acc)
	}
	// 端点表取值：缺前导斜杠要补上，未登记要报 ok=false。
	if got, has := cfg.Endpoint("recharge"); !has || got != "/pay/recharge" {
		t.Errorf("Endpoint(recharge)=%q,%v，期望 /pay/recharge,true", got, has)
	}
	if _, has := cfg.Endpoint("withdraw"); has {
		t.Error("未登记的端点不得报成功")
	}

	cases := []struct {
		name string
		ref  *PaySDKRef
		want string
	}{
		{"未引用", nil, "不启用"},
		{"引用名为空", &PaySDKRef{SDK: "  "}, "不启用"},
		{"段不存在", &PaySDKRef{SDK: "nope"}, "nope"},
		{"段未开启", &PaySDKRef{SDK: "off"}, "未显式开启"},
		{"多账户未点名", &PaySDKRef{SDK: "many"}, "点名"},
		{"点名了停用账户", &PaySDKRef{SDK: "many", Account: "reserve"}, "未显式开启"},
		{"点名了缺商户号的账户", &PaySDKRef{SDK: "many", Account: "leaky"}, "merchant_id"},
		{"账户别名不存在", &PaySDKRef{SDK: "one", Account: "ghost"}, "ghost"},
	}
	for _, cs := range cases {
		_, _, err := cs.ref.Resolve(sdks)
		if err == nil {
			t.Errorf("%s 应当报错", cs.name)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s 报错文案缺 %q，实得 %v", cs.name, cs.want, err)
		}
	}
}

// TestResolveNeverLeaksSecret 引用解析的报错会进启动日志，密钥不得顺带出去。
func TestResolveNeverLeaksSecret(t *testing.T) {
	sdks := sdkTable()
	sdks["one"].SecretKey = "TOPSECRET"
	_, _, err := (&PaySDKRef{SDK: "one", Account: "ghost"}).Resolve(sdks)
	if err == nil {
		t.Fatal("应当报错")
	}
	if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
}

// TestResolveServiceToleratesNoAccount 只发消息的链路没有商户账户是常态：
// 按资金口径要求它填一个，得到的只会是一个假商户号。
func TestResolveServiceToleratesNoAccount(t *testing.T) {
	sdks := sdkTable()
	sdks["msg"] = &PaySDKConfig{Enable: "on", Driver: "generic_md5", BaseURL: "https://msg.example"}
	_, acc, err := (&PaySDKRef{SDK: "msg"}).ResolveService(sdks)
	if err != nil {
		t.Fatalf("无账户的下发段应当可解析: %v", err)
	}
	if acc != nil {
		t.Fatalf("无账户时却给出了商户账户: %+v", acc)
	}
	// 同一段拿去出款就不放行：出账主体必须明确。
	if _, _, err := (&PaySDKRef{SDK: "msg"}).Resolve(sdks); err == nil ||
		!strings.Contains(err.Error(), "accounts") {
		t.Fatalf("无账户的段用于出款应当报错，实得 %v", err)
	}
	// 一旦配了账户，歧义与停用规则与资金链路完全一致。
	if _, _, err := (&PaySDKRef{SDK: "many"}).ResolveService(sdks); err == nil ||
		!strings.Contains(err.Error(), "点名") {
		t.Fatalf("多账户未点名应当报错，实得 %v", err)
	}
	if _, _, err := (&PaySDKRef{SDK: "off"}).ResolveService(sdks); err == nil {
		t.Fatal("未开启的段不应当被解析成功")
	}
}

// TestValidatePayChannels 配置期就能确定的错（段名写错、缺驱动标记、缺商户号）
// 必须阻止启动，而不是等第一笔出款暴露。
func TestValidatePayChannels(t *testing.T) {
	base := func() *Cfg {
		return &Cfg{
			PaySDks: map[string]*PaySDKConfig{
				"a": {Enable: "on", Driver: "generic_md5", BaseURL: "https://a.example",
					Accounts: map[string]*PayMerchantAccount{"default": {Enable: "on", MerchantID: "M1"}}},
			},
			Usdt:     &UsdtConfig{Provider: &PaySDKRef{SDK: "a"}},
			Whatsapp: &WhatsappConfig{Login: &PaySDKRef{}, Provider: nil},
			Telegram: &TelegramConfig{TonNetwork: "mainnet"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}

	cases := map[string]func(*Cfg){
		"通道引用了不存在的段": func(c *Cfg) { c.Usdt.Provider = &PaySDKRef{SDK: "ghost"} },
		"段缺驱动标记":     func(c *Cfg) { c.PaySDks["a"].Driver = "" },
		"段缺基址":       func(c *Cfg) { c.PaySDks["a"].BaseURL = " " },
		"账户缺商户号":     func(c *Cfg) { c.PaySDks["a"].Accounts["default"].MerchantID = "" },
		"出款引用没有账户":   func(c *Cfg) { c.PaySDks["a"].Accounts = nil },
		"出款引用了停用的账户": func(c *Cfg) { c.PaySDks["a"].Accounts["default"].Enable = "off" },
	}
	for name, mutate := range cases {
		c := base()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s 未被拦下", name)
		} else if strings.Contains(err.Error(), "TOPSECRET") {
			t.Errorf("%s 报错含敏感值: %v", name, err)
		}
	}

	// 「写好但没开」是合法状态：关掉一条通道不该同时要求它每一项配置都仍然完整。
	off := base()
	off.PaySDks["a"].Enable = "off"
	off.PaySDks["a"].Driver = ""
	off.PaySDks["a"].BaseURL = ""
	if err := off.Validate(); err != nil {
		t.Errorf("未开启的段不应参与判定: %v", err)
	}
	// 只有验证码下发的段没有商户账户是常态。
	loginOnly := base()
	loginOnly.PaySDks["wa"] = &PaySDKConfig{Enable: "on", Driver: "generic_md5", BaseURL: "https://wa.example"}
	loginOnly.Whatsapp.Login = &PaySDKRef{SDK: "wa"}
	if err := loginOnly.Validate(); err != nil {
		t.Errorf("下发链路引用无账户的段应当放行: %v", err)
	}
	// 但段名写错，哪怕那条通道还没开，也当场就该报——它是拼写错，与开关无关。
	ghostClosed := base()
	ghostClosed.PaySDks["a"].Enable = "off"
	ghostClosed.Usdt.Provider = &PaySDKRef{SDK: "ghost"}
	if err := ghostClosed.Validate(); err == nil {
		t.Error("引用了不存在的段时，即使该通道未开启也应当拦下")
	}

	// 全空配置（没有任何资金段）是正常缺省，不得拦启动。
	empty := &Cfg{}
	if err := empty.Validate(); err != nil {
		t.Errorf("未配置资金通道时 Validate 应当放行: %v", err)
	}
}
