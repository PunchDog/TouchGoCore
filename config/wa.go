package config

import "time"

// 以下方法是 WhatsappSessionsConfig 的「缺省值」口径：纯读取、不写日志。
// 所有方法都对 nil 接收者安全——整机没配 whatsapp.sessions 段时，消息侧仍要能拿到可用缺省
//（此时 DSN 为空、消息侧本来就不启动，但 Effective* 不能因此 panic）。

const (
	waDefaultConnectTimeout = 15 * time.Second
	waDefaultReconnectMin   = 3 * time.Second
	waDefaultReconnectMax   = 60 * time.Second
	waDefaultSendMinDelay   = 1 * time.Second
	waDefaultSendMaxDelay   = 5 * time.Second
	waDefaultSendMaxPerHour = 30
)

// EffectiveConnectTimeout 补默认连接超时（ConnectTimeoutSec<=0 → 15 秒）。
func (c *WhatsappSessionsConfig) EffectiveConnectTimeout() time.Duration {
	if c == nil || c.ConnectTimeoutSec <= 0 {
		return waDefaultConnectTimeout
	}
	return time.Duration(c.ConnectTimeoutSec) * time.Second
}

// EffectiveReconnectMax 补默认重连退避上界（ReconnectMaxSec<=0 → 60 秒）。
func (c *WhatsappSessionsConfig) EffectiveReconnectMax() time.Duration {
	if c == nil || c.ReconnectMaxSec <= 0 {
		return waDefaultReconnectMax
	}
	return time.Duration(c.ReconnectMaxSec) * time.Second
}

// EffectiveReconnectMin 补默认重连退避下界（ReconnectMinSec<=0 → 3 秒）。
//
// 下界压过上界时按上界取：配出 min>max 只会让退避永远停在 max，
// 与其静默按错的方向跑，不如让两个读数一致。
func (c *WhatsappSessionsConfig) EffectiveReconnectMin() time.Duration {
	min := waDefaultReconnectMin
	if c != nil && c.ReconnectMinSec > 0 {
		min = time.Duration(c.ReconnectMinSec) * time.Second
	}
	if max := c.EffectiveReconnectMax(); min > max {
		return max
	}
	return min
}

// EffectiveMaxConcurrent 返回在跑 Client 数的软上限；<=0 归一成 0，读作「不限制」。
//
// 缺省不限是用户裁决（来一个配一个），归一成 0 是让消费侧只写一个 `>0` 判断，
// 不必再区分「没配」与「配了负数」两种「不限」。
func (c *WhatsappSessionsConfig) EffectiveMaxConcurrent() int {
	if c == nil || c.MaxConcurrent <= 0 {
		return 0
	}
	return c.MaxConcurrent
}

// EffectiveSendMaxDelay 补默认批内间隔上界（SendMaxDelaySec<=0 → 5 秒）。
func (c *WhatsappSessionsConfig) EffectiveSendMaxDelay() time.Duration {
	if c == nil || c.SendMaxDelaySec <= 0 {
		return waDefaultSendMaxDelay
	}
	return time.Duration(c.SendMaxDelaySec) * time.Second
}

// EffectiveSendMinDelay 补默认批内间隔下界（SendMinDelaySec<=0 → 1 秒）。
//
// 与退避同款纪律：下界压过上界时按上界取，配出 min>max 只会让随机区间塌成空集，
// 不如让两个读数一致。
func (c *WhatsappSessionsConfig) EffectiveSendMinDelay() time.Duration {
	min := waDefaultSendMinDelay
	if c != nil && c.SendMinDelaySec > 0 {
		min = time.Duration(c.SendMinDelaySec) * time.Second
	}
	if max := c.EffectiveSendMaxDelay(); min > max {
		return max
	}
	return min
}

// EffectiveSendMaxPerHour 补默认单账号小时闸（SendMaxPerHour<=0 → 30 条）。
//
// 与 max_concurrent 的「<=0 即不限」刻意相反：这条闸防的是非官方客户端的风控封号，
// 「没配」读作「用保守缺省」而不是「裸奔」；要调高就显式配正数。
func (c *WhatsappSessionsConfig) EffectiveSendMaxPerHour() int {
	if c == nil || c.SendMaxPerHour <= 0 {
		return waDefaultSendMaxPerHour
	}
	return c.SendMaxPerHour
}

// EffectiveAntibanEnabled 返回防封闸开关：段在而键缺省 → 开（用户裁决，防封是常态
// 诉求，裸奔要显式声明）；整段 nil → 关——没配 sessions 连消息侧都不启动，
// 闸无处可挂，不把「段缺失」和「显式关」混成一种要单独解释的形态。
func (c *WhatsappSessionsConfig) EffectiveAntibanEnabled() bool {
	if c == nil || c.AntibanEnabled == nil {
		return c != nil
	}
	return *c.AntibanEnabled
}

// EffectiveAntibanPreset 返回防封闸档位名；留空折算成 moderate。
// 写错的名字不纠——库的 Resolve 自己兜底回 moderate，这里不复制那份表。
func (c *WhatsappSessionsConfig) EffectiveAntibanPreset() string {
	if c == nil {
		return "moderate"
	}
	return c.AntibanPreset
}

// EffectiveAntibanStateDir 返回防封闸状态目录；留空折算成工作目录下的 antiban_state。
func (c *WhatsappSessionsConfig) EffectiveAntibanStateDir() string {
	if c == nil || c.AntibanStateDir == "" {
		return "antiban_state"
	}
	return c.AntibanStateDir
}
