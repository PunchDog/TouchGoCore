package config

import "time"

// 以下方法是 WhatsappSessionsConfig 的「缺省值」口径：纯读取、不写日志。
// 所有方法都对 nil 接收者安全——整机没配 whatsapp.sessions 段时，消息侧仍要能拿到可用缺省
//（此时 DSN 为空、消息侧本来就不启动，但 Effective* 不能因此 panic）。

const (
	waDefaultConnectTimeout = 15 * time.Second
	waDefaultReconnectMin   = 3 * time.Second
	waDefaultReconnectMax   = 60 * time.Second
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
