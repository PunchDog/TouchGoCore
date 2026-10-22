package tron

import (
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/util"
)

// 本文件钉住内联装配（tron.usdt 一段写全）的两道闸：密钥注入钩子的键名迁移，
// 以及驱动名必须自己把关。这两条在旧装配层（pay.OpenChain 按注册表取驱动）里
// 是白拿的，内联之后没人替本包挡，所以必须各有一条用例。

// mustOnchain 走一遍真实装配入口并取回链上通道内部结构。
func mustOnchain(t *testing.T, cfg *config.Cfg) *onchainChannel {
	t.Helper()
	c, ok := newUSDTClient(cfg)
	if !ok || c == nil {
		t.Fatal("装配失败")
	}
	ch, ok := c.ch.(*onchainChannel)
	if !ok {
		t.Fatalf("装配出的通道类型不是 *onchainChannel: %T", c.ch)
	}
	return ch
}

// TestUsdtInlineKeyHooks 注入钩子的键名：只认段名 tron.usdt，旧段名 supplier_a 不再被读取。
//
// 双读期（P2→P5）已经结束：两个键都能把密钥塞进来时，「密钥从哪来」是猜的。
// 这条闸把两边都腕死：新键必须生效，旧键必须不再生效——只断言前者的话，
// 回落分支就算被删错（比如把新键写成旧键）也看不出来。
func TestUsdtInlineKeyHooks(t *testing.T) {
	// 新键生效：密钥与私钥各走各的前缀。
	// 钩子注册与注销成对贴着这一小段（不用 defer）：同一个键名留在全局钩子表里，
	// 下一段用例就会把它的值当成自己那段注入进来的结果。
	cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.SecretKey = ""
	cfg.Tron.Usdt.Chain.PrivateKey = ""
	idNew := util.DefaultCallFunc.Register(util.CallPaySDKMsg+inlineUSDTKey, func(sk *string) { *sk = usdtTestSecret })
	idKey := util.DefaultCallFunc.Register(util.CallPayChainMsg+inlineUSDTKey, func(pk *string) { *pk = usdtTestPrivKey })
	ch := mustOnchain(t, cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+inlineUSDTKey, idNew)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+inlineUSDTKey, idKey)
	if ch.opt.SecretKey != usdtTestSecret {
		t.Fatalf("新键未生效: SecretKey=%q", ch.opt.SecretKey)
	}
	if ch.chain.PrivateKey != usdtTestPrivKey {
		t.Fatalf("私钥钩子未按新键注入: PrivateKey=%q", ch.chain.PrivateKey)
	}

	// 旧键不再被读取：只注册旧键时，密钥不得落进段里，也不得进到通道快照。
	// 注意不能拿「通道不启动」当判据：供应商密钥为空只让账户查询不可用，
	// 链上出款照样能装配——那种失败形态跟密钥从哪来无关。
	cfg = usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.SecretKey = ""
	idOld := util.DefaultCallFunc.Register(util.CallPaySDKMsg+"supplier_a", func(sk *string) { *sk = "OLD_SECRET" })
	c, _ := newUSDTClient(cfg)
	util.DefaultCallFunc.Unregister(util.CallPaySDKMsg+"supplier_a", idOld)
	if got := strings.TrimSpace(cfg.Tron.Usdt.SecretKey); got != "" {
		t.Fatalf("旧键 supplier_a 仍能注入密钥：段里的 secret_key=%q", got)
	}
	if c != nil {
		if ch, ok := c.ch.(*onchainChannel); ok && ch.opt.SecretKey != "" {
			t.Fatalf("旧键 supplier_a 仍能注入密钥：通道快照 SecretKey=%q", ch.opt.SecretKey)
		}
	}
}

// TestUsdtInlineRejectsWrongDriver 内联装配不经驱动注册表，驱动名必须自己把关。
//
// 写错驱动名在这里放行，等于让「配置写着换实现、实际还是这一套」静默通过。
// 只测真的不同的名字：前后空白由 TrimSpace 归一（那是同一份意图，不是另一个驱动）。
func TestUsdtInlineRejectsWrongDriver(t *testing.T) {
	for _, driver := range []string{"", "generic_md5", "tron_trongrid_v2", "tron"} {
		cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
		cfg.Tron.Usdt.Driver = driver
		if c, ok := newUSDTClient(cfg); ok || c != nil {
			t.Errorf("driver=%q 不应让内联通道启动", driver)
		}
	}
	// 前后空白是同一份意图：TrimSpace 之后等价，不该被拒。
	cfg := usdtPayCfg("https://pay.invalid", usdtAllEndpoints())
	cfg.Tron.Usdt.Driver = " " + DriverTronGrid + " "
	if _, ok := newUSDTClient(cfg); !ok {
		t.Error("driver 前后空白应当被归一而不是当成另一个驱动")
	}
}
