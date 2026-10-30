package config

import (
	"testing"
	"time"
)

// TestParseWaSendRateValid `分钟|条数` 的合法形态（含空白容错）。
func TestParseWaSendRateValid(t *testing.T) {
	for _, c := range []struct {
		raw    string
		window time.Duration
		max    int
	}{
		{"1|1", time.Minute, 1},
		{"5|3", 5 * time.Minute, 3},
		{"60|30", time.Hour, 30},
		{" 2 | 7 ", 2 * time.Minute, 7},
		{"1440|1", 24 * time.Hour, 1},
		{"1|1000", time.Minute, 1000},
		// 前导零是纯数字，与写侧正则 `^[0-9]+$` 同口径：解析成数值而不是报错。
		{"007|7", 7 * time.Minute, 7},
	} {
		got, ok := ParseWaSendRate(c.raw)
		if !ok {
			t.Errorf("✘ %q 应解析成功", c.raw)
			continue
		}
		if got.Window != c.window || got.Max != c.max {
			t.Errorf("✘ %q 解析成 window=%s max=%d，期望 window=%s max=%d",
				c.raw, got.Window, got.Max, c.window, c.max)
		}
	}
}

// TestParseWaSendRateInvalid 配错的速率必须被拒，不能静默纠错 ——
// 它会直接改变出网节奏，猜一个值比拒绝更危险。
//
// 正号两项是 2026-10-29 与后台写侧对齐时补的：strconv.Atoi 认 "+1"，而写侧的
// parseStrictInt 只认 `^[0-9]+$`。两侧不一致时 " +1|1 " 会在配置文件里生效、
// 在后台被拒，属于「同一件事两个答案」，所以这里一并钉住。
func TestParseWaSendRateInvalid(t *testing.T) {
	for _, raw := range []string{
		"", "   ", "1", "1|", "|1", "0|1", "1|0", "-1|1", "1|-1",
		"a|b", "1|1|1", "1;1", "1441|1", "1|1001", "0|0",
		"+1|1", "1|+1", "1.5|1", "1|1.0", "0x1|1", "1|1e3",
	} {
		if got, ok := ParseWaSendRate(raw); ok {
			t.Errorf("✘ %q 应被拒，实际解析成 %+v", raw, got)
		}
	}
}

// TestEffectiveSendRatePrecedence send_rate 优先；其次旧键（换算成 60 分钟）；
// 都没有才用缺省 1 分钟 1 条。
func TestEffectiveSendRatePrecedence(t *testing.T) {
	var nilCfg *WhatsappSessionsConfig
	if got := nilCfg.EffectiveSendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Errorf("nil 配置缺省应为 1 分钟 1 条，实得 %+v", got)
	}

	empty := &WhatsappSessionsConfig{}
	if got := empty.EffectiveSendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Errorf("空配置缺省应为 1 分钟 1 条，实得 %+v", got)
	}

	// 配了合法值就用它。
	cfg := &WhatsappSessionsConfig{SendRate: "5|3"}
	if got := cfg.EffectiveSendRate(); got.Window != 5*time.Minute || got.Max != 3 {
		t.Errorf("send_rate=5|3 应解析成 5 分钟 3 条，实得 %+v", got)
	}

	// 配了非法值回落缺省（不回落到任何旧键：send_max_per_hour 已删除）。
	bad := &WhatsappSessionsConfig{SendRate: "oops"}
	if got := bad.EffectiveSendRate(); got.Window != time.Minute || got.Max != 1 {
		t.Errorf("send_rate 非法时应回落缺省 1|1，实得 %+v", got)
	}
}

// TestEffectiveSendMaxPerMinuteCompat 已废弃的薄包装仍返回窗口内条数。
func TestEffectiveSendMaxPerMinuteCompat(t *testing.T) {
	c := &WhatsappSessionsConfig{SendRate: "5|3"}
	if got := c.EffectiveSendMaxPerMinute(); got != 3 {
		t.Errorf("废弃包装应返回条数 3，实得 %d", got)
	}
}
