package ton

import (
	"encoding/hex"
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

// 本文件与 tron/usdt_assembly_test.go 同构：钉住内联装配（ton.native 一段写全）
// 的几道闸。内联之后没人替本包挡驱动名与密钥来源，所以必须各有一条用例。

const (
	tonEquivHTTPBase = "https://tonapi.invalid"
	tonEquivAPIKey   = "TON-API-KEY"
)

// tonInlineCfg 造一份字段齐备的内联形态 TON 配置。
func tonInlineCfg() *config.Cfg {
	privHex := hex.EncodeToString(tonTestSeed())
	return &config.Cfg{
		Ton: &config.TonConfig{Native: &config.TonNativeConfig{
			Enable:  "on",
			Driver:  DriverTonAPI,
			Network: pay.NetworkMainnet,
			Chain: &config.TonChainConfig{
				HTTPBase: tonEquivHTTPBase, APIKey: tonEquivAPIKey,
				WalletAddress: tonV5R1Mainnet, WalletVersion: "v5R1",
				PrivateKey: privHex, FeeLimit: 20_000_000,
				TimeoutSec: 45, MaxRetries: 2,
			},
		}},
	}
}

// mustTONChannel 走一遍真实装配入口并取回链上通道内部结构。
func mustTONChannel(t *testing.T, cfg *config.Cfg) *tonChannel {
	t.Helper()
	c, ok := newTONNativeClient(cfg)
	if !ok || c == nil {
		t.Fatal("装配失败")
	}
	ch, ok := c.Ch.(*tonChannel)
	if !ok {
		t.Fatalf("装配出的通道类型不是 *tonChannel: %T", c.Ch)
	}
	return ch
}

// TestTonInlineKeyHooks 注入钩子的键名：只认 segment ton.native，旧段名不再被读取。
//
// TON 没有供应商签名密钥了，只剩热钱包私钥一个钩子；旧键必须不再生效。
func TestTonInlineKeyHooks(t *testing.T) {
	privHex := hex.EncodeToString(tonTestSeed())

	cfg := tonInlineCfg()
	cfg.Ton.Native.Chain.PrivateKey = ""
	idKey := util.DefaultCallFunc.Register(util.CallPayChainMsg+inlineTONKey, func(pk *string) { *pk = privHex })
	ch := mustTONChannel(t, cfg)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+inlineTONKey, idKey)
	if ch.chain.PrivateKey != privHex {
		t.Fatalf("私钥钩子未按新键注入: PrivateKey=%q", ch.chain.PrivateKey)
	}

	// 旧键不再被读取：只注册旧键时，配置里仍为空，装配必须失败（没有私钥）。
	cfg = tonInlineCfg()
	cfg.Ton.Native.Chain.PrivateKey = ""
	idOld := util.DefaultCallFunc.Register(util.CallPayChainMsg+"telegram.ton", func(pk *string) { *pk = privHex })
	c, ok := newTONNativeClient(cfg)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+"telegram.ton", idOld)
	if got := strings.TrimSpace(cfg.Ton.Native.Chain.PrivateKey); got != "" {
		t.Fatalf("旧键 telegram.ton 仍能注入私钥：配置里的 private_key=%q", got)
	}
	if ok || c != nil {
		t.Fatal("只有旧键时 TON 通道不该装配成功")
	}
}

// TestTonInlineRejectsWrongDriver 内联装配不经驱动注册表，驱动名必须自己把关。
func TestTonInlineRejectsWrongDriver(t *testing.T) {
	for _, driver := range []string{"", "generic_md5", "ton_tonapi_v2", "ton"} {
		cfg := tonInlineCfg()
		cfg.Ton.Native.Driver = driver
		if _, ok := newTONNativeClient(cfg); ok {
			t.Errorf("driver=%q 不应让内联通道启动", driver)
		}
	}
}

// TestTonInlineStillRejectsJetton jetton 与网络两道闸跑在链装配之前。
func TestTonInlineStillRejectsJetton(t *testing.T) {
	cfg := tonInlineCfg()
	cfg.Ton.Native.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTONNativeClient(cfg); ok {
		t.Error("配了 jetton 的 TON 通道不应启动")
	}
	// 通道没开也不该因为「反正不启动」就把 jetton 放过去：写错的配置要当场报出来。
	cfg = tonInlineCfg()
	cfg.Ton.Native.Enable = "off"
	cfg.Ton.Native.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTONNativeClient(cfg); ok {
		t.Error("未开启的通道同样必须拒 jetton")
	}
}
