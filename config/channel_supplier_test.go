package config

import (
	"strings"
	"testing"
)

// supplier 是一份可直接用于判定的内联供应商字段：一段单账户、一段多账户、一段被关闭。
func supplierSection() *ChannelSupplierConfig {
	return &ChannelSupplierConfig{
		Enable: "on", Driver: "tron_grid", BaseURL: "https://one.example",
		Endpoints: map[string]string{"query": "pay/query", "account": " pay/account "},
		Accounts: map[string]*ChannelAccountConfig{
			"default": {Enable: "on", MerchantID: "MCH-1"},
		},
	}
}

// TestSupplierEndpoint 端点表取值：缺前导斜杠要补上、两端空白要去掉、未登记要报 ok=false。
//
// 未登记必须报 false 而不是给个默认路径：配置不全的操作要在发请求之前就拒掉，
// 拼一个猜出来的路径上去只会让供应商回一个语义不明的 404。
func TestSupplierEndpoint(t *testing.T) {
	s := supplierSection()
	if got, has := s.Endpoint("query"); !has || got != "/pay/query" {
		t.Errorf("Endpoint(query)=%q,%v，期望 /pay/query,true", got, has)
	}
	if got, has := s.Endpoint("account"); !has || got != "/pay/account" {
		t.Errorf("Endpoint(account)=%q,%v，期望 /pay/account,true（两端空白要 trim 后再补斜杠）", got, has)
	}
	if _, has := s.Endpoint("withdraw"); has {
		t.Error("未登记的端点不得报成功")
	}
	if _, has := (*ChannelSupplierConfig)(nil).Endpoint("query"); has {
		t.Error("nil 接收者不得报成功")
	}
}

// TestSupplierAccount 商户账户解析：唯一账户可留空别名，多账户必须点名，
// 停用或缺商户号一律不放行——出账主体说不清就是不能出钱。
func TestSupplierAccount(t *testing.T) {
	s := supplierSection()
	acc, err := s.Account("")
	if err != nil {
		t.Fatalf("唯一账户留空应当可解析: %v", err)
	}
	if acc.MerchantID != "MCH-1" {
		t.Fatalf("解析出的商户号不符: %+v", acc)
	}
	// 点名与留空在单账户表上等价。
	if acc, err := s.Account("default"); err != nil || acc.MerchantID != "MCH-1" {
		t.Fatalf("点名唯一账户应当可解析: %+v / %v", acc, err)
	}

	multi := supplierSection()
	multi.Accounts["reserve"] = &ChannelAccountConfig{Enable: "off", MerchantID: "MCH-2"}
	multi.Accounts["leaky"] = &ChannelAccountConfig{Enable: "on"}

	cases := []struct {
		name  string
		sec   *ChannelSupplierConfig
		alias string
		want  string
	}{
		{"nil 段", nil, "", "accounts"},
		{"没有账户表", &ChannelSupplierConfig{Enable: "on"}, "", "accounts"},
		{"多账户未点名", multi, "", "点名"},
		{"点名了停用账户", multi, "reserve", "未显式开启"},
		{"点名了缺商户号的账户", multi, "leaky", "merchant_id"},
		{"账户别名不存在", multi, "ghost", "ghost"},
	}
	for _, cs := range cases {
		_, err := cs.sec.Account(cs.alias)
		if err == nil {
			t.Errorf("%s 应当报错", cs.name)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s 报错文案缺 %q，实得 %v", cs.name, cs.want, err)
		}
	}
	// 点名之后多账户表同样可解析：歧义靠点名消除，不是靠只允许一个账户。
	if acc, err := multi.Account("default"); err != nil || acc.MerchantID != "MCH-1" {
		t.Fatalf("多账户点名后应当可解析: %+v / %v", acc, err)
	}
}

// TestSupplierAccountNeverLeaksSecret 解析报错会进启动日志，密钥不得顺带出去。
func TestSupplierAccountNeverLeaksSecret(t *testing.T) {
	s := supplierSection()
	s.SecretKey = "TOPSECRET"
	if _, err := s.Account("ghost"); err == nil {
		t.Fatal("应当报错")
	} else if strings.Contains(err.Error(), "TOPSECRET") {
		t.Fatalf("错误文案泄漏密钥: %v", err)
	}
}

// TestValidateInlineChannels 配置期就能确定的错（缺驱动、缺基址、缺商户号、
// 缺接入点、缺出款地址）必须阻止启动，而不是等第一笔出款暴露。
//
// 口径对四个通道段一致：判定范围只有「已经开启的部分」，enable 不为 on 的段整体跳过——
// 关掉一条通道不该同时要求它的每一项配置都仍然完整。
func TestValidateInlineChannels(t *testing.T) {
	base := func() *Cfg {
		return &Cfg{
			Tron: &TronConfig{Usdt: &TronUsdtConfig{
				ChannelSupplierConfig: *supplierSection(),
				Network:               "mainnet",
				Contract:              "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
				Decimals:              6,
				Chain: &TronChainConfig{
					Node:          "grpc.trongrid.io:50051",
					WalletAddress: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
				},
			}},
			Bsc: &BscConfig{Usdt: &BscUsdtConfig{
				Enable: "on", Driver: "bsc_evm", Network: "mainnet",
				Contract: "0x55d398326f99059fF775485246999027B3197955", Decimals: 18,
				Chain: &BscChainConfig{
					RPC: "https://bsc-dataseed.binance.org", ChainID: 56,
					WalletAddress: "0x1111111111111111111111111111111111111111",
				},
			}},
			Sol: &SolConfig{Usdt: &SolUsdtConfig{
				Enable: "on", Driver: "sol_spl", Network: "mainnet-beta",
				Contract: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6,
				Chain: &SolChainConfig{
					RPC:           "https://api.mainnet-beta.solana.com",
					WalletAddress: "11111111111111111111111111111111",
				},
			}},
			Whatsapp: &WhatsappConfig{},
			Telegram: &TelegramConfig{BotToken: "123456:ABC"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("合法配置被拒: %v", err)
	}

	cases := map[string]func(*Cfg){
		"段缺驱动标记":         func(c *Cfg) { c.Tron.Usdt.Driver = "" },
		"段缺基址":           func(c *Cfg) { c.Tron.Usdt.BaseURL = " " },
		"账户缺商户号":         func(c *Cfg) { c.Tron.Usdt.Accounts["default"].MerchantID = "" },
		"出款引用没有账户":       func(c *Cfg) { c.Tron.Usdt.Accounts = nil },
		"出款引用了停用的账户":     func(c *Cfg) { c.Tron.Usdt.Accounts["default"].Enable = "off" },
		"链上段缺失":          func(c *Cfg) { c.Tron.Usdt.Chain = nil },
		"链上段缺接入点":        func(c *Cfg) { c.Tron.Usdt.Chain.Node = "" },
		"链上段缺出款地址":       func(c *Cfg) { c.Tron.Usdt.Chain.WalletAddress = "" },
		"network 填了代币标准": func(c *Cfg) { c.Tron.Usdt.Network = "trc20" },
		"缺合约地址":          func(c *Cfg) { c.Tron.Usdt.Contract = "" },
		"小数位与合约不符":       func(c *Cfg) { c.Tron.Usdt.Decimals = 18 },
		"BSC 缺链 ID":      func(c *Cfg) { c.Bsc.Usdt.Chain.ChainID = 0 },
		"BSC 小数位不符":      func(c *Cfg) { c.Bsc.Usdt.Decimals = 6 },
		"SOL 缺 RPC":      func(c *Cfg) { c.Sol.Usdt.Chain.RPC = " " },
		"SOL 小数位不符":      func(c *Cfg) { c.Sol.Usdt.Decimals = 18 },
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

	// 「写好但没开」是合法状态：四个通道段都只剩半截配置也不得拦启动。
	off := base()
	for _, s := range []*ChannelSupplierConfig{&off.Tron.Usdt.ChannelSupplierConfig} {
		s.Enable = "off"
		s.Driver = ""
		s.BaseURL = ""
		s.Accounts = nil
	}
	off.Tron.Usdt.Chain = nil
	off.Bsc.Usdt.Enable = "off"
	off.Bsc.Usdt.Chain = nil
	off.Sol.Usdt.Enable = "off"
	off.Sol.Usdt.Chain = nil
	if err := off.Validate(); err != nil {
		t.Errorf("未开启的段不应参与判定: %v", err)
	}

	// 全空配置（没有任何资金段）是正常缺省，不得拦启动。
	empty := &Cfg{}
	if err := empty.Validate(); err != nil {
		t.Errorf("未配置资金通道时 Validate 应当放行: %v", err)
	}
}
