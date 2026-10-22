package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestZaloEnabled 总闸口径与 whatsapp/ChannelSupplierConfig 一致：只有显式 on（大小写不敏感）才算开，
// nil 接收者安全，缺省不开。
func TestZaloEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *ZaloConfig
		want bool
	}{
		{"nil", nil, false},
		{"空段", &ZaloConfig{}, false},
		{"off", &ZaloConfig{Enable: "off"}, false},
		{"ON 大写", &ZaloConfig{Enable: "ON"}, true},
		{"带空格", &ZaloConfig{Enable: " on "}, true},
	}
	for _, cs := range cases {
		if got := cs.cfg.Enabled(); got != cs.want {
			t.Errorf("%s: Enabled()=%v 期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestZaloEffectiveDefaults nil 与空段都必须给出截图口径的保守缺省。
func TestZaloEffectiveDefaults(t *testing.T) {
	for _, c := range []*ZaloSessionsConfig{nil, {}} {
		if got := c.EffectiveQRTTL(); got != 5*time.Minute {
			t.Errorf("QRTTL 缺省=%v", got)
		}
		if got := c.EffectiveRequestTimeout(); got != 30*time.Second {
			t.Errorf("RequestTimeout 缺省=%v", got)
		}
		if got := c.EffectiveFriendsCacheTTL(); got != 5*time.Minute {
			t.Errorf("FriendsCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveGroupsCacheTTL(); got != 5*time.Minute {
			t.Errorf("GroupsCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveInfoMinInterval(); got != 1500*time.Millisecond {
			t.Errorf("InfoMinInterval 缺省=%v", got)
		}
		if got := c.EffectiveInfoCacheTTL(); got != 10*time.Minute {
			t.Errorf("InfoCacheTTL 缺省=%v", got)
		}
		if got := c.EffectiveSendMinDelay(); got != 3*time.Second {
			t.Errorf("SendMinDelay 缺省=%v", got)
		}
		if got := c.EffectiveSendMaxDelay(); got != 5*time.Second {
			t.Errorf("SendMaxDelay 缺省=%v", got)
		}
		if got := c.EffectiveSendMaxPerHour(); got != 30 {
			t.Errorf("SendMaxPerHour 缺省=%d", got)
		}
	}
}

// TestZaloEffectiveClamps 低于截图下限的值必须被抬到下限；send min/max 交叉时按 max 取。
func TestZaloEffectiveClamps(t *testing.T) {
	c := &ZaloSessionsConfig{
		QRTTLSec:           10,
		RequestTimeoutSec:  1,
		FriendsCacheTTLSec: 10,
		GroupsCacheTTLSec:  10,
		InfoMinIntervalMs:  100,
		InfoCacheTTLSec:    1,
		SendMinDelayMs:     100,
		SendMaxDelayMs:     1000,
		SendMaxPerHour:     -1,
	}
	if got := c.EffectiveQRTTL(); got != 2*time.Minute {
		t.Errorf("QRTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveRequestTimeout(); got != 5*time.Second {
		t.Errorf("RequestTimeout 下限钳制=%v", got)
	}
	if got := c.EffectiveFriendsCacheTTL(); got != 5*time.Minute {
		t.Errorf("FriendsCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveGroupsCacheTTL(); got != 5*time.Minute {
		t.Errorf("GroupsCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveInfoMinInterval(); got != 1500*time.Millisecond {
		t.Errorf("InfoMinInterval 下限钳制=%v", got)
	}
	if got := c.EffectiveInfoCacheTTL(); got != 10*time.Minute {
		t.Errorf("InfoCacheTTL 下限钳制=%v", got)
	}
	if got := c.EffectiveSendMinDelay(); got != 3*time.Second {
		t.Errorf("SendMinDelay 下限钳制=%v", got)
	}
	if got := c.EffectiveSendMaxDelay(); got != 3*time.Second {
		t.Errorf("SendMaxDelay 低于 min 时按 min 取=%v", got)
	}
	if got := c.EffectiveSendMaxPerHour(); got != 30 {
		t.Errorf("SendMaxPerHour 非正值应折算 30，实得 %d", got)
	}

	big := &ZaloSessionsConfig{SendMinDelayMs: 9000, SendMaxDelayMs: 5000}
	if got := big.EffectiveSendMinDelay(); got != 5*time.Second {
		t.Errorf("min>max 时应按 max 取，实得 %v", got)
	}
}

// TestZaloListenerDefault 指针区分「缺省开」与「显式关」；整段 nil 读作关。
func TestZaloListenerDefault(t *testing.T) {
	off := false
	on := true
	cases := []struct {
		name string
		cfg  *ZaloSessionsConfig
		want bool
	}{
		{"段缺失", nil, false},
		{"键缺省", &ZaloSessionsConfig{}, true},
		{"显式关", &ZaloSessionsConfig{ListenerEnabled: &off}, false},
		{"显式开", &ZaloSessionsConfig{ListenerEnabled: &on}, true},
	}
	for _, cs := range cases {
		if got := cs.cfg.EffectiveListenerEnabled(); got != cs.want {
			t.Errorf("%s: listener=%v 期望 %v", cs.name, got, cs.want)
		}
	}
}

// TestZaloUserAgentLanguageTrim 只 trim，留空保持空串交给 zcago 默认。
func TestZaloUserAgentLanguageTrim(t *testing.T) {
	c := &ZaloSessionsConfig{UserAgent: "  UA  ", Language: " vi "}
	if got := c.EffectiveUserAgent(); got != "UA" {
		t.Errorf("UA=%q", got)
	}
	if got := c.EffectiveLanguage(); got != "vi" {
		t.Errorf("Language=%q", got)
	}
	var nilCfg *ZaloSessionsConfig
	if nilCfg.EffectiveUserAgent() != "" || nilCfg.EffectiveLanguage() != "" {
		t.Error("nil 配置应返回空串")
	}
}

// TestZaloLegacyConfigRejected 旧 zalo.login/templates 必须在启动期报错，
// 不能被 encoding/json 静默丢弃。
//
// login 的类型是 json.RawMessage（旧供应商 SDK 表已整张删除，这里只需要「这个键
// 出现过没有」），所以字面量直接给原始 JSON 字节；`{}` 这种空对象也算写了——
// 它同样说明部署还在按旧形态配置。
func TestZaloLegacyConfigRejected(t *testing.T) {
	cases := []*ZaloConfig{
		{Login: json.RawMessage(`{"sdk":"supplier_zalo"}`)},
		{Login: json.RawMessage(`{}`)},
		{Templates: map[string]string{"auth_verify": "tpl"}},
	}
	for _, zc := range cases {
		cfg := &Cfg{Zalo: zc}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("旧配置 %+v 未被拦下", zc)
		}
		if !strings.Contains(err.Error(), "已废弃") {
			t.Errorf("报错应指明废弃迁移：%v", err)
		}
	}

	// 新配置（即使 sessions 为空）不该被墓碑检查拦下。
	if err := (&Cfg{Zalo: &ZaloConfig{Enable: "off", Sessions: &ZaloSessionsConfig{}}}).Validate(); err != nil {
		t.Errorf("新配置不应被拦: %v", err)
	}
}

// TestExampleZaloHasNoLegacyKeys 示例的 zalo 段只允许 enable + sessions；
// 旧 login/templates 是墓碑字段，不得出现在示例里（否则会被当成可照抄的配置）。
func TestExampleZaloHasNoLegacyKeys(t *testing.T) {
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var doc struct {
		Zalo map[string]json.RawMessage `json:"zalo"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if doc.Zalo == nil {
		t.Fatal(`example.json 缺少 "zalo" 配置块`)
	}
	if _, ok := doc.Zalo["sessions"]; !ok {
		t.Fatal(`example.json 的 zalo 段缺少 sessions`)
	}
	for _, legacy := range []string{"login", "templates"} {
		if _, ok := doc.Zalo[legacy]; ok {
			t.Errorf("example.json 的 zalo 段仍含已废弃键 %q", legacy)
		}
	}
}
