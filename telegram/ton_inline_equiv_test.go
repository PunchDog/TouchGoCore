package telegram

import (
	"encoding/hex"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件与 tron/usdt_assembly_test.go 同构：钉住内联装配（telegram.ton 一段写全）
// 的几道闸。这些判定在旧装配层（pay.OpenChain 按注册表取驱动）里是白拿的，
// 内联之后没人替本包挡，所以必须各有一条用例。

const (
	tonEquivAccount  = "default"
	tonEquivMerchant = "MCH-TON-EQ"
	tonEquivSecret   = "TON_TEST_SECRET"
	tonEquivBaseURL  = "https://supplier.invalid/api"
	tonEquivHTTPBase = "https://tonapi.invalid"
	tonEquivAPIKey   = "TON-API-KEY"
)

// tonInlineCfg 造一份字段齐备的内联形态 TON 配置。
func tonInlineCfg() *config.Cfg {
	privHex := hex.EncodeToString(tonTestSeed())
	return &config.Cfg{
		Telegram: &config.TelegramConfig{
			BotToken: "123456:ABC",
			Ton: &config.TonChannelConfig{
				ChannelSupplierConfig: config.ChannelSupplierConfig{
					Enable: "on", Driver: DriverTonAPI, BaseURL: tonEquivBaseURL,
					AppID: "app-ton", SecretKey: tonEquivSecret, AuthHeader: "X-Sign",
					TimeoutSec: 30, MaxRetries: 2,
					Endpoints: map[string]string{pay.EndpointAccount: "/pay/account"},
					Accounts: map[string]*config.ChannelAccountConfig{
						tonEquivAccount: {Enable: "on", MerchantID: tonEquivMerchant},
					},
					AccountID: tonEquivAccount,
				},
				Network: pay.NetworkMainnet,
				Chain: &config.TonChainConfig{
					HTTPBase: tonEquivHTTPBase, APIKey: tonEquivAPIKey,
					WalletAddress: tonV5R1Mainnet, WalletVersion: "v5R1",
					PrivateKey: privHex, FeeLimit: 20_000_000,
					TimeoutSec: 45, MaxRetries: 2,
				},
			},
		},
	}
}

// mustTonChannel 走一遍真实装配入口并取回链上通道内部结构。
func mustTonChannel(t *testing.T, cfg *config.Cfg) *tonChannel {
	t.Helper()
	c, ok := newTonClient(cfg)
	if !ok || c == nil {
		t.Fatal("装配失败")
	}
	ch, ok := c.ch.(*tonChannel)
	if !ok {
		t.Fatalf("装配出的通道类型不是 *tonChannel: %T", c.ch)
	}
	return ch
}

// TestTonInlineKeyHooks 注入钩子的键名：只认段名 telegram.ton，旧段名 supplier_ton 不再被读取。
//
// 双读期（P2→P5）已经结束：两个键都能把密钥塞进来时，「密钥从哪来」是猜的。
// 这条闸把两边都腕死：新键必须生效，旧键必须不再生效。
func TestTonInlineKeyHooks(t *testing.T) {
	privHex := hex.EncodeToString(tonTestSeed())

	cfg := tonInlineCfg()
	cfg.Telegram.Ton.SecretKey = ""
	cfg.Telegram.Ton.Chain.PrivateKey = ""
	// 钩子注册与注销成对贴着这一小段（不用 defer）：同一个键名留在全局钩子表里，
	// 下一段用例就会把它的值当成自己那段注入进来的结果。
	idNew := util.DefaultCallFunc.Register(util.CallPaySDKMsg+inlineTONKey, func(sk *string) { *sk = tonEquivSecret })
	idKey := util.DefaultCallFunc.Register(util.CallPayChainMsg+inlineTONKey, func(pk *string) { *pk = privHex })
	ch := mustTonChannel(t, cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+inlineTONKey, idNew)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+inlineTONKey, idKey)
	if ch.opt.SecretKey != tonEquivSecret {
		t.Fatalf("新键未生效: SecretKey=%q", ch.opt.SecretKey)
	}
	if ch.chain.PrivateKey != privHex {
		t.Fatalf("私钥钩子未按新键注入: PrivateKey=%q", ch.chain.PrivateKey)
	}

	// 旧键不再被读取：只注册旧键时，密钥不得落进段里，也不得进到通道快照。
	// 不拿「通道不启动」当判据：供应商密钥为空只让账户查询不可用，
	// 链上出款照样能装配——那种失败形态跟密钥从哪来无关。
	cfg = tonInlineCfg()
	cfg.Telegram.Ton.SecretKey = ""
	idOld := util.DefaultCallFunc.Register(util.CallPaySDKMsg+"supplier_ton", func(sk *string) { *sk = "OLD_SECRET" })
	c, _ := newTonClient(cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+"supplier_ton", idOld)
	if got := strings.TrimSpace(cfg.Telegram.Ton.SecretKey); got != "" {
		t.Fatalf("旧键 supplier_ton 仍能注入密钥：段里的 secret_key=%q", got)
	}
	if c != nil {
		if ch, ok := c.ch.(*tonChannel); ok && ch.opt.SecretKey != "" {
			t.Fatalf("旧键 supplier_ton 仍能注入密钥：通道快照 SecretKey=%q", ch.opt.SecretKey)
		}
	}
}

// TestTonInlineRejectsWrongDriver 内联装配不经驱动注册表，驱动名必须自己把关。
func TestTonInlineRejectsWrongDriver(t *testing.T) {
	for _, driver := range []string{"", "generic_md5", "ton_tonapi_v2", "ton"} {
		cfg := tonInlineCfg()
		cfg.Telegram.Ton.Driver = driver
		if _, ok := newTonClient(cfg); ok {
			t.Errorf("driver=%q 不应让内联通道启动", driver)
		}
	}
}

// TestTonInlineStillRejectsJetton jetton 与网络两道闸跑在供应商字段之前：
// 它们的判据来自通道段而不是供应商字段，不该被「字段齐不齐」影响。
func TestTonInlineStillRejectsJetton(t *testing.T) {
	cfg := tonInlineCfg()
	cfg.Telegram.Ton.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTonClient(cfg); ok {
		t.Error("配了 jetton 的 TON 通道不应启动")
	}
	// 通道没开也不该因为「反正不启动」就把 jetton 放过去：写错的配置要当场报出来，
	// 否则某天有人只改 enable=on 就会把代币当原生币发出去。
	cfg = tonInlineCfg()
	cfg.Telegram.Ton.Enable = "off"
	cfg.Telegram.Ton.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTonClient(cfg); ok {
		t.Error("未开启的通道同样必须拒 jetton")
	}
}
