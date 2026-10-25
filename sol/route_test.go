package sol

import (
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
)

// 本文件只钉 SOL 侧的「接线正确性」：装配期「配置不全就静默不启动」，以及注入键名
// 与分派种类同源。门面机器本身（分派登记、重启关旧、订单补全、未启动拒绝）的行为
// 基线在 chainroute/route_test.go 只写一份，四链共用。

// TestNewSolUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
func TestNewSolUSDTClientRejectsIncomplete(t *testing.T) {
	wallet := solTestKey(t).PublicKey().String()
	good := func() *config.SolUsdtConfig {
		return &config.SolUsdtConfig{
			Enable:   "on",
			Driver:   driverSolRPC,
			Network:  "mainnet-beta",
			Contract: solBytes1To32,
			Decimals: 6,
			Chain:    &config.SolChainConfig{RPC: "https://example.invalid", WalletAddress: wallet},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 sol 段", &config.Cfg{}},
		{"缺 sol.usdt 段", &config.Cfg{Sol: &config.SolConfig{}}},
		{"enable 不是 on", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"mint 为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"mint 非法", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "not-an-address"
			return u
		}()}}},
		{"网络为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
	}
	for _, cs := range cases {
		c, ok := newSolUSDTClient(cs.cfg)
		if ok {
			t.Errorf("%s: 装配成功了（应当拒绝）", cs.name)
			if c != nil {
				_ = c.Ch.Close()
			}
		}
		if c != nil {
			t.Errorf("%s: 拒绝时仍返回了非 nil 客户端", cs.name)
		}
	}
}

// TestNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestNamespaceKeys(t *testing.T) {
	if solUSDTKey != string(pay.KindSolUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", solUSDTKey, pay.KindSolUSDT)
	}
	if !strings.Contains(string(pay.KindSolUSDT), "sol") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindSolUSDT)
	}
}
