package nft

// 装配面（wire.go newClient / fillOrder）的行为闸。
//
// 为什么单独一件：newClient 是「配置 → 真资产链路」的唯一入口，它判错的代价是
// 整条链路带着错误的签发主体/密钥上线，而此前全部用例都直接构造 Provider 绕过了它
// ——变异检验时把密钥注入钩子那一行删掉，包级套件照绿，即证明这条接线没有活闸。
// 本文件按 §8「宁可不起」口径逐条钉住每个拒启分支，并钉住 CallNftSDKMsg 密钥钩子真被调用。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"touchgocore/config"
	"touchgocore/util"
)

// qaWireCfg 造一份「只差被点名的那一处」的可启动配置，返回 Cfg 与 SDK 段指针便于就地改坏。
func qaWireCfg(t *testing.T, baseURL string) (*config.Cfg, *config.NftSDKConfig) {
	t.Helper()
	sdk := &config.NftSDKConfig{
		Enable: "on", Driver: DriverGeneric, BaseURL: baseURL,
		AppID: "wire-app", SecretKey: "wire-secret",
		Endpoints: map[string]string{"mint": "/mint", "query": "/query"},
		Accounts: map[string]*config.NftMerchantAccount{
			"main": {Enable: "on", MerchantID: "m-wire"},
		},
	}
	cfg := &config.Cfg{
		Nft: &config.NftConfig{
			Provider: &config.NftSDKRef{SDK: "mynft", Account: "main"},
			Chain:    ChainETH, Network: NetworkMainnet,
		},
		NftSDks: map[string]*config.NftSDKConfig{"mynft": sdk},
	}
	return cfg, sdk
}

func TestQaWireAssembly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	// 每个拒启分支都必须「不起链路」而不是带着半套配置上线。
	cases := []struct {
		name   string
		mutate func(cfg *config.Cfg, sdk *config.NftSDKConfig)
	}{
		{"sdk 段未开", func(_ *config.Cfg, sdk *config.NftSDKConfig) { sdk.Enable = "off" }},
		{"enable 大小写外的值", func(_ *config.Cfg, sdk *config.NftSDKConfig) { sdk.Enable = "yes" }},
		{"未标 driver", func(_ *config.Cfg, sdk *config.NftSDKConfig) { sdk.Driver = "" }},
		{"账户别名没点名且表里多主体", func(_ *config.Cfg, sdk *config.NftSDKConfig) {
			sdk.Accounts["second"] = &config.NftMerchantAccount{Enable: "on", MerchantID: "m2"}
		}},
		{"账户未开", func(_ *config.Cfg, sdk *config.NftSDKConfig) {
			sdk.Accounts["main"].Enable = "off"
		}},
		{"账户缺 merchant_id", func(_ *config.Cfg, sdk *config.NftSDKConfig) {
			sdk.Accounts["main"].MerchantID = " "
		}},
		{"没有 accounts", func(_ *config.Cfg, sdk *config.NftSDKConfig) { sdk.Accounts = nil }},
		{"密钥为空（钩子没注入）", func(_ *config.Cfg, sdk *config.NftSDKConfig) { sdk.SecretKey = "" }},
		{"合约地址含空白", func(cfg *config.Cfg, _ *config.NftSDKConfig) { cfg.Nft.Contract = "0x AA" }},
		{"base_url 明文非本机", func(_ *config.Cfg, sdk *config.NftSDKConfig) {
			sdk.BaseURL = "http://nft.example.com"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, sdk := qaWireCfg(t, srv.URL)
			// 「账户别名没点名」需要两个主体才成立，其余用例单主体即可。
			if tc.name == "账户别名没点名且表里多主体" {
				cfg.Nft.Provider.Account = ""
			}
			tc.mutate(cfg, sdk)
			if c, ok := newClient(cfg); ok || c != nil {
				t.Fatalf("半套配置不得起链路，实得 ok=%v c=%+v", ok, c)
			}
		})
	}

	t.Run("provider引用缺失", func(t *testing.T) {
		cfg, _ := qaWireCfg(t, srv.URL)
		cfg.Nft.Provider = nil
		if c, ok := newClient(cfg); ok || c != nil {
			t.Fatalf("未绑定 SDK 段应不启动，实得 ok=%v", ok)
		}
		cfg2, _ := qaWireCfg(t, srv.URL)
		cfg2.Nft.Provider.SDK = "not-there"
		if c, ok := newClient(cfg2); ok || c != nil {
			t.Fatalf("引用了不存在的段名应不启动，实得 ok=%v", ok)
		}
		if c, ok := newClient(&config.Cfg{}); ok || c != nil {
			t.Fatalf("nft 段缺失应不启动，实得 ok=%v", ok)
		}
		if c, ok := newClient(nil); ok || c != nil {
			t.Fatalf("cfg 为 nil 应不启动而不是 panic，实得 ok=%v", ok)
		}
	})

	t.Run("密钥注入钩子必须先于构造", func(t *testing.T) {
		// 配置里留空、启动前由下游钩子注入——这正是「密钥不进版本库」的口径。
		// 删掉 wire.go 里那一行 Do，钩子就永不触发：secret 仍为空 ⇒ NewProvider 拒 ⇒ ok=false。
		cfg, sdk := qaWireCfg(t, srv.URL)
		sdk.SecretKey = ""
		var gotSection string
		id := util.DefaultCallFunc.Register(util.CallNftSDKMsg+"mynft", func(key *string) {
			gotSection = "hit"
			*key = "hook-injected-secret"
		})
		defer util.DefaultCallFunc.Unregister(util.CallNftSDKMsg+"mynft", id)

		c, ok := newClient(cfg)
		if gotSection != "hit" {
			t.Fatal("CallNftSDKMsg 密钥钩子没被调用（wire.go 的注入行是死代码？）")
		}
		if !ok || c == nil {
			t.Fatal("钩子注入密钥后应能装配链路")
		}
	})

	t.Run("启动态快照与只读不广播口径", func(t *testing.T) {
		cfg, _ := qaWireCfg(t, srv.URL)
		cfg.Nft.Contract = "0xContract"
		c, ok := newClient(cfg)
		if !ok || c == nil {
			t.Fatal("完整配置应装配成功")
		}
		if c.chain != ChainETH || c.network != NetworkMainnet || c.contract != "0xContract" {
			t.Fatalf("快照字段没按配置落定: %+v", c)
		}
		if c.ch == nil {
			t.Fatal("通道没装上")
		}
	})
}

// TestQaWireFillOrderDefaults 钉住 fillOrder 只补默认、不改幂等键、不就地改调用方的单。
func TestQaWireFillOrderDefaults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	cfg, _ := qaWireCfg(t, srv.URL)
	cfg.Nft.Contract = "0xDefault"
	c, ok := newClient(cfg)
	if !ok {
		t.Fatal("前置装配失败")
	}

	o := &NftOrder{OrderNo: "qa-wire-fill-1", Quantity: 1, ToAddress: "0xAAAA"}
	cp := c.fillOrder(o)
	if cp == o {
		t.Fatal("补了默认值就必须给副本，就地改上游的单=替上游决定发出去的是什么")
	}
	if cp.Chain != ChainETH || cp.Network != NetworkMainnet || cp.Contract != "0xDefault" {
		t.Fatalf("默认值没补齐: %+v", cp)
	}
	if o.Chain != "" || o.Contract != "" {
		t.Fatalf("原单被改写: %+v", o)
	}
	if cp.OrderNo != "qa-wire-fill-1" {
		t.Fatalf("OrderNo 是幂等键，任何路径不得改写，实得 %q", cp.OrderNo)
	}

	// 已填齐的单不再复制（避免无谓分配），且 nil 输入不 panic。
	full := &NftOrder{OrderNo: "qa-wire-fill-2", Chain: ChainTRON, Network: NetworkMainnet,
		Contract: "0xOther", Quantity: 1, ToAddress: "0xBBBB"}
	if got := c.fillOrder(full); got != full {
		t.Fatal("全填齐时应原样返回，不必复制")
	}
	if c.fillOrder(nil) != nil {
		t.Fatal("nil 单应原样返回 nil")
	}

	// 快照缺省（chain/network 未配）时也不得凭空造值。
	c2, ok2 := newClient(func() *config.Cfg {
		cc, _ := qaWireCfg(t, srv.URL)
		cc.Nft.Chain, cc.Nft.Network = "", ""
		return cc
	}())
	if !ok2 {
		t.Fatal("chain/network 留空不该阻断装配")
	}
	if got := c2.fillOrder(&NftOrder{OrderNo: "qa-wire-fill-3", Quantity: 1,
		ToAddress: "0xCCCC"}); got.Chain != "" || got.Network != "" {
		t.Fatalf("配置留空时不得造默认值，实得 %q/%q", got.Chain, got.Network)
	}
}
