package config

import (
	"testing"
	"time"
)

// TestWhatsappSessionsEffectiveDefaults 表驱动钉住 Effective* 的缺省口径：
// 整机没配 whatsapp.sessions 段（nil 接收者）与配了半截（字段 <=0）都必须落到同一组
// 内置缺省，而不是 panic 或按 0 秒跑——0 秒超时会让消息侧每次连接都当场失败。
func TestWhatsappSessionsEffectiveDefaults(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *WhatsappSessionsConfig
		timeout time.Duration
		min     time.Duration
		max     time.Duration
		maxConc int
	}{
		{"nil 接收者", nil, 15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"零值", &WhatsappSessionsConfig{}, 15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"负值按缺省", &WhatsappSessionsConfig{ConnectTimeoutSec: -1, ReconnectMinSec: -3, ReconnectMaxSec: -9, MaxConcurrent: -2},
			15 * time.Second, 3 * time.Second, 60 * time.Second, 0},
		{"显式取值", &WhatsappSessionsConfig{ConnectTimeoutSec: 5, ReconnectMinSec: 2, ReconnectMaxSec: 20, MaxConcurrent: 8},
			5 * time.Second, 2 * time.Second, 20 * time.Second, 8},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			if got := cs.cfg.EffectiveConnectTimeout(); got != cs.timeout {
				t.Errorf("EffectiveConnectTimeout=%s，期望 %s", got, cs.timeout)
			}
			if got := cs.cfg.EffectiveReconnectMin(); got != cs.min {
				t.Errorf("EffectiveReconnectMin=%s，期望 %s", got, cs.min)
			}
			if got := cs.cfg.EffectiveReconnectMax(); got != cs.max {
				t.Errorf("EffectiveReconnectMax=%s，期望 %s", got, cs.max)
			}
			if got := cs.cfg.EffectiveMaxConcurrent(); got != cs.maxConc {
				t.Errorf("EffectiveMaxConcurrent=%d，期望 %d", got, cs.maxConc)
			}
		})
	}
}

// TestWhatsappSessionsMinOverMaxClamps 下界压过上界时按上界取：
// 配出 min>max 若不夹取，退避序列会从下界起跳、永远越过上界，重连节奏与配置意图相反。
func TestWhatsappSessionsMinOverMaxClamps(t *testing.T) {
	cfg := &WhatsappSessionsConfig{ReconnectMinSec: 30, ReconnectMaxSec: 10}
	if got := cfg.EffectiveReconnectMin(); got != 10*time.Second {
		t.Errorf("min>max 未夹取到上界: %s", got)
	}
	if got := cfg.EffectiveReconnectMax(); got != 10*time.Second {
		t.Errorf("max=%s", got)
	}
}

// TestEffectiveAntiban 防封闸三项的缺省折算：档位空串→moderate 原样透传（兜底在库
// Resolve 侧）、状态目录空→antiban_state、nil 接收者不 panic。
func TestEffectiveAntiban(t *testing.T) {
	var nilCfg *WhatsappSessionsConfig
	if got := nilCfg.EffectiveAntibanPreset(); got != "moderate" {
		t.Errorf("nil 档位=%q", got)
	}
	if got := nilCfg.EffectiveAntibanStateDir(); got != "antiban_state" {
		t.Errorf("nil 目录=%q", got)
	}
	c := &WhatsappSessionsConfig{AntibanPreset: "strict", AntibanStateDir: `D:\data\ab`}
	if got := c.EffectiveAntibanPreset(); got != "strict" {
		t.Errorf("档位=%q", got)
	}
	if got := c.EffectiveAntibanStateDir(); got != `D:\data\ab` {
		t.Errorf("目录=%q", got)
	}
	// 三态开关：段在而键缺省 → 开；显式 false → 关；整段 nil → 关。
	if !c.EffectiveAntibanEnabled() {
		t.Error("段在而键缺省该读作开")
	}
	off := false
	if (&WhatsappSessionsConfig{AntibanEnabled: &off}).EffectiveAntibanEnabled() {
		t.Error("显式 false 该读作关")
	}
	if nilCfg.EffectiveAntibanEnabled() {
		t.Error("整段缺失该读作关")
	}
}

// TestWhatsappConfigEnabled 通道总闸读数：显式 "on"（大小写与首尾空白不敏感）才算开，
// 段缺失/键缺失/off/其它值一律关。口径与 PaySDKConfig.Enabled 一致——whatsmeow 是非官方
// 协议客户端，「没显式开即不连 WhatsApp 服务器」必须是缺省。
func TestWhatsappConfigEnabled(t *testing.T) {
	for _, v := range []string{"on", "ON", "On", " on ", "\ton\n"} {
		if !(&WhatsappConfig{Enable: v}).Enabled() {
			t.Errorf("enable=%q 该读作开", v)
		}
	}
	for _, v := range []string{"", "  ", "off", "true", "1", "yes"} {
		if (&WhatsappConfig{Enable: v}).Enabled() {
			t.Errorf("enable=%q 该读作关", v)
		}
	}
	var nilCfg *WhatsappConfig
	if nilCfg.Enabled() {
		t.Error("整段缺失该读作关")
	}
}
