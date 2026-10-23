// 本文件是 config 的外部测试包：它要钉的是「示例配置里的链上地址是真地址」，
// 而地址规则住在各资金通道包里（tron / ton / bsc / sol 各自的 address.go）。
// config 不导入这些包（配置层只认数据形状），所以这份核对放在外部测试包，
// 由测试二进制把三方同时链进来。
//
// P5 之后各包不再往 pay 的注册表里登记规则，而是各自持有一份包内实现并导出
// IsValid* 判定函数。核对方式因此从「查注册表」变成「直调各包的导出校验」：
// 少了一个进程级全局，也少了一条「忘了登记 ⇒ 未登记即放行」的静默通道。
package config_test

import (
	"encoding/json"
	"os"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"

	"touchgocore/bsc"
	"touchgocore/sol"
	"touchgocore/ton"
	"touchgocore/tron"
)

// loadExample 读示例配置。示例是给下游抄的起点，抄一份跑不通的地址比不写示例更糟。
func loadExample(t *testing.T) *config.Cfg {
	t.Helper()
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	// telegram 段仍在示例里（Bot 配置）；TON 资金段已独立为 ton.native。
	if cfg.Tron == nil || cfg.Tron.Usdt == nil || cfg.Ton == nil || cfg.Ton.Native == nil {
		t.Fatalf("示例缺 tron.usdt / ton.native 段")
	}
	return &cfg
}

// TestChainValidatorsLinked 四条链的地址校验都必须在本测试二进制里可用。
//
// 这条先于地址断言跑，否则「校验函数没链进来」会伪装成「地址全对」。
// 逐个用一眼假的串探一下：真实现必然拒，恒真函数（或漏链导致的空实现）会放行。
func TestChainValidatorsLinked(t *testing.T) {
	validators := []struct {
		name string
		ok   func(string) bool
		bad  string
	}{
		{"tron", tron.IsValidTRONAddress, "not-a-tron-address"},
		{"ton", ton.IsValidTONAddress, "not-a-ton-address"},
		{"bsc", bsc.IsValidEVMAddress, "0xnothex"},
		{"sol", sol.IsValidSolanaAddress, "0OIl-not-base58"},
	}
	for _, v := range validators {
		if v.ok(v.bad) {
			t.Fatalf("%s 的地址校验放行了明显非法的串 %q，校验函数可能没被链进来", v.name, v.bad)
		}
	}
}

// TestExampleChainAddressesAreReal 示例里的合约地址、网络标识逐字段过各链的校验。
// 地址是给人抄的，长度和版本字节都对、校验和却不过的字符串最容易蒙过肉眼。
func TestExampleChainAddressesAreReal(t *testing.T) {
	cfg := loadExample(t)
	contract := cfg.Tron.Usdt.Contract
	t.Logf("tron.usdt.network=%s contract=%q", cfg.Tron.Usdt.Network, contract)

	nets := map[string]string{
		"tron.usdt.network": cfg.Tron.Usdt.Network,
	}
	for name, v := range nets {
		switch v {
		case pay.NetworkMainnet, pay.NetworkTestnet, pay.NetworkShasta, pay.NetworkNile, pay.NetworkChapel, pay.NetworkDevnet:
		default:
			t.Errorf(`%s=%q 不是已登记的公链网络标识（代币标准名不算网络）`, name, v)
		}
	}

	addrs := []struct {
		name, addr string
		ok         func(string) bool
		optional   bool
	}{
		{"tron.usdt.contract", contract, tron.IsValidTRONAddress, false},
	}
	for _, a := range addrs {
		if a.addr == "" {
			if !a.optional {
				t.Errorf("%s 留空：该字段是示例通道必须带上的合约地址", a.name)
			}
			continue
		}
		if !a.ok(a.addr) {
			t.Errorf("%s=%q 未通过对应链的地址校验", a.name, a.addr)
		}
	}
}

// TestExampleAddressRejectionsStillHold 把本轮换掉的两个历史值钉成反例：
// 它们看着像真地址（长度、字符集、版本字节都对），只有校验和会把它们筛出来。
// 若日后有人为了让本测试通过而放宽校验，这里会先红。
func TestExampleAddressRejectionsStillHold(t *testing.T) {
	cases := []struct {
		name, addr string
		ok         func(string) bool
	}{
		{"旧 usdt.contract", "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR", tron.IsValidTRONAddress},
		{"旧 jetton 夹具", "EQDtFpEwcR-fm552Nv6h3FDFdv3TbHx8w9Wm7FqYqU8dBB1q", ton.IsValidTONAddress},
	}
	for _, c := range cases {
		if c.ok(c.addr) {
			t.Errorf("%s=%q 本应被校验和拒掉，却通过了", c.name, c.addr)
		}
	}
}
