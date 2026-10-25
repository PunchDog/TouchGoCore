package bsc

import (
	"strings"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"
)

// 本文件只钉 BSC 侧的「接线正确性」：装配期「配置不全就静默不启动」，以及注入键名
// 与分派种类同源。门面机器本身（分派登记、重启关旧、订单补全、未启动拒绝）的行为
// 基线在 chainroute/route_test.go 只写一份，四链共用。

// TestNewBscUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
//
// 最后一例是资金闸而不是缺项：配置写齐了但私钥没注入，同样拒绝。
// 签名者的存在与否不该由「配置看起来完整」来推断。
func TestNewBscUSDTClientRejectsIncomplete(t *testing.T) {
	good := func() *config.BscUsdtConfig {
		return &config.BscUsdtConfig{
			Enable:   "on",
			Driver:   driverBSCRPC,
			Network:  "mainnet",
			Contract: evmChecksumMixed,
			Decimals: 18,
			Chain: &config.BscChainConfig{
				RPC:           "https://example.invalid",
				WalletAddress: evmAllLower,
			},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 bsc 段", &config.Cfg{}},
		{"缺 bsc.usdt 段", &config.Cfg{Bsc: &config.BscConfig{}}},
		{"enable 不是 on", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"合约为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"合约非法", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = evmBogusChecksum
			return u
		}()}}},
		{"网络为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
		{"配置齐备但私钥未注入", &config.Cfg{Bsc: &config.BscConfig{Usdt: good()}}},
	}
	for _, cs := range cases {
		c, ok := newBscUSDTClient(cs.cfg)
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

// TestBscNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestBscNamespaceKeys(t *testing.T) {
	if bscUSDTKey != string(pay.KindBscUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", bscUSDTKey, pay.KindBscUSDT)
	}
	if !strings.Contains(string(pay.KindBscUSDT), "bsc") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindBscUSDT)
	}
}
