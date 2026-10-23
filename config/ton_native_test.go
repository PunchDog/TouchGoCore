package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// tonNativeBase 是一份可被 Validate 放行的 ton.native 配置。
// 地址只做非空校验，装配期才验派生地址；这里不编译 ton 包，避免 config → ton → config 循环。
func tonNativeBase() *Cfg {
	return &Cfg{Ton: &TonConfig{Native: &TonNativeConfig{
		Enable:  "on",
		Driver:  "ton_tonapi",
		Network: "mainnet",
		Chain: &TonChainConfig{
			WalletAddress: "EQ-CHANGE-ME-HOT-WALLET",
		},
	}}}
}

func TestTonNativeEnabled(t *testing.T) {
	cases := map[string]bool{
		"":        false,
		"off":     false,
		"on":      true,
		" ON ":    true,
		"enabled": false,
	}
	for raw, want := range cases {
		got := (&TonNativeConfig{Enable: raw}).Enabled()
		if got != want {
			t.Errorf("Enabled(%q)=%v，期望 %v", raw, got, want)
		}
	}
}

func TestValidateTonNative(t *testing.T) {
	if err := tonNativeBase().Validate(); err != nil {
		t.Fatalf("合法 ton.native 被拒: %v", err)
	}

	cases := map[string]func(*TonNativeConfig){
		"缺 driver":         func(n *TonNativeConfig) { n.Driver = " " },
		"jetton 非空":        func(n *TonNativeConfig) { n.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs" },
		"network 非法":       func(n *TonNativeConfig) { n.Network = "ton" },
		"chain 为空":         func(n *TonNativeConfig) { n.Chain = nil },
		"wallet_address 空": func(n *TonNativeConfig) { n.Chain.WalletAddress = " " },
	}
	for name, mutate := range cases {
		cfg := tonNativeBase()
		mutate(cfg.Ton.Native)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s 未被拦下", name)
		}
	}

	// 没开闸时半截配置不拦启动：示例与「先写配置、后开开关」都是这个形态。
	off := tonNativeBase()
	off.Ton.Native.Enable = "off"
	off.Ton.Native.Driver = ""
	off.Ton.Native.Chain = nil
	if err := off.Validate(); err != nil {
		t.Fatalf("未开启的 ton.native 不该被校验拦下: %v", err)
	}
}

// TestValidateRejectsLegacyTelegramTon 旧段必须明确报迁移错误，
// 不能被 encoding/json 静默丢弃后表现为「通道没启动」。
func TestValidateRejectsLegacyTelegramTon(t *testing.T) {
	var cfg Cfg
	raw := []byte(`{"telegram":{"bot_token":"x","ton":{"enable":"on"}}}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ton.native") {
		t.Fatalf("旧 telegram.ton 应明确报迁移错误，实得 %v", err)
	}
}

// TestTonNativeSectionParses 钉住新的 JSON 形状：顶层 ton.native，不再有供应商字段。
func TestTonNativeSectionParses(t *testing.T) {
	var cfg Cfg
	raw := []byte(`{"ton":{"native":{
		"enable":"on","driver":"ton_tonapi","network":"testnet","jetton":"",
		"chain":{"http_base":"https://testnet.tonapi.io","wallet_address":"EQ-TEST"}
	}}}`)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	n := cfg.Ton.Native
	if n == nil || !n.Enabled() || n.Driver != "ton_tonapi" || n.Network != "testnet" {
		t.Fatalf("ton.native 字段没有绑定: %+v", n)
	}
	if n.Chain == nil || n.Chain.HTTPBase != "https://testnet.tonapi.io" || n.Chain.WalletAddress != "EQ-TEST" {
		t.Fatalf("ton.native.chain 字段没有绑定: %+v", n.Chain)
	}
}
