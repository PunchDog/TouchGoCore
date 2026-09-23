// 本文件是 config 的外部测试包：它要钉的是「示例配置里的链上地址是真地址」，
// 而地址规则住在 pay 的注册表里、由各资金通道包在自己的 init 中登记。
// config 不能导入 pay（pay 侧已经引用 config 的通道结构，两头引就成环），
// 所以这份核对只能放在外部测试包，靠测试二进制把三方同时链进来。
package config_test

import (
	"encoding/json"
	"os"
	"testing"

	"touchgocore/config"
	"touchgocore/pay"

	// 只为把两个通道包的 init 链进来：不导入则注册表是空的，
	// 下面的地址断言会全部走「未登记即放行」那条路，测不出任何东西。
	_ "touchgocore/telegram"
	_ "touchgocore/ustd"
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
	if cfg.Ustd == nil || cfg.Telegram == nil {
		t.Fatalf("示例缺 ustd / telegram 段: %p %p", cfg.Ustd, cfg.Telegram)
	}
	return &cfg
}

// TestChainRulesLinked 三条链的规则都得在注册表里。
// 这条先于地址断言跑，否则「规则没登记」会伪装成「地址全对」。
func TestChainRulesLinked(t *testing.T) {
	for _, c := range []string{pay.CurrencyUSDT, pay.CurrencyTRX, pay.CurrencyTON} {
		if _, ok := pay.ChainRuleFor(c); !ok {
			t.Fatalf("币种 %s 无链上规则：通道包的 init 未被链进来，已登记的是 %v", c, pay.CurrenciesWithRules())
		}
	}
}

// TestExampleChainAddressesAreReal 示例里的合约地址、网络标识逐字段过各链的校验。
// 地址是给人抄的，长度和版本字节都对、校验和却不过的字符串最容易蒙过肉眼。
func TestExampleChainAddressesAreReal(t *testing.T) {
	cfg := loadExample(t)
	contract, jetton := cfg.Ustd.Contract, cfg.Telegram.Jetton
	t.Logf("ustd.network=%s contract=%q | telegram.ton_network=%s jetton=%q",
		cfg.Ustd.Network, contract, cfg.Telegram.TonNetwork, jetton)

	nets := map[string]string{
		"ustd.network":         cfg.Ustd.Network,
		"telegram.ton_network": cfg.Telegram.TonNetwork,
	}
	for name, v := range nets {
		switch v {
		case pay.NetworkMainnet, pay.NetworkTestnet, pay.NetworkShasta, pay.NetworkNile:
		default:
			t.Errorf(`%s=%q 不是已登记的公链网络标识（代币标准名不算网络）`, name, v)
		}
	}

	// jetton 留空代表原生 TON，是示例有意演示的形态；填了就必须是真地址。
	addrs := []struct {
		name, currency, addr, network string
		optional                      bool
	}{
		{"ustd.contract", pay.CurrencyUSDT, contract, cfg.Ustd.Network, false},
		{"telegram.jetton", pay.CurrencyTON, jetton, cfg.Telegram.TonNetwork, true},
	}
	for _, a := range addrs {
		rule, ok := pay.ChainRuleFor(a.currency)
		if !ok {
			t.Fatalf("币种 %s 无规则", a.currency)
		}
		if a.addr == "" {
			if !a.optional {
				t.Errorf("%s 留空：该字段是示例通道必须带上的合约地址", a.name)
			}
			continue
		}
		o := &pay.PayOrder{Currency: a.currency, Network: a.network, Address: a.addr}
		if err := rule.CheckAddress(o); err != nil {
			t.Errorf("%s=%q 未通过 %s 链校验: %v", a.name, a.addr, a.currency, err)
		}
	}
}

// TestExampleAddressRejectionsStillHold 把本轮换掉的两个历史值钉成反例：
// 它们看着像真地址（长度、字符集、版本字节都对），只有校验和会把它们筛出来。
// 若日后有人为了让本测试通过而放宽校验，这里会先红。
func TestExampleAddressRejectionsStillHold(t *testing.T) {
	usdt, ok := pay.ChainRuleFor(pay.CurrencyUSDT)
	if !ok {
		t.Fatal("USDT 规则未登记")
	}
	ton, ok := pay.ChainRuleFor(pay.CurrencyTON)
	if !ok {
		t.Fatal("TON 规则未登记")
	}
	cases := []struct {
		name, currency, addr, network string
		rule                          pay.ChainRule
	}{
		{"旧 ustd.contract", pay.CurrencyUSDT, "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR", pay.NetworkMainnet, usdt},
		{"旧 jetton 夹具", pay.CurrencyTON, "EQDtFpEwcR-fm552Nv6h3FDFdv3TbHx8w9Wm7FqYqU8dBB1q", pay.NetworkMainnet, ton},
	}
	for _, c := range cases {
		o := &pay.PayOrder{Currency: c.currency, Network: c.network, Address: c.addr}
		if err := c.rule.CheckAddress(o); err == nil {
			t.Errorf("%s=%q 本应被校验和拒掉，却通过了", c.name, c.addr)
		}
	}
}
