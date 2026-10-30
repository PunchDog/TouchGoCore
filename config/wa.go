package config

import (
	"strconv"
	"strings"
	"time"
)

// 以下方法是 WhatsappSessionsConfig 的「缺省值」口径：纯读取、不写日志。
// 所有方法都对 nil 接收者安全——整机没配 whatsapp.sessions 段时，消息侧仍要能拿到可用缺省
//（此时 DSN 为空、消息侧本来就不启动，但 Effective* 不能因此 panic）。

const (
	waDefaultConnectTimeout = 15 * time.Second
	waDefaultReconnectMin   = 3 * time.Second
	waDefaultReconnectMax   = 60 * time.Second
	waDefaultSendMinDelay   = 1 * time.Second
	waDefaultSendMaxDelay   = 5 * time.Second

	// waDefaultSendRateWindow/Max 是单账号发送闸的缺省速率：每滚动 1 分钟 1 条。
	//
	// 取 1/1 而不是「换算过来的旧缺省 30 条/小时」：后者 ≈ 每 2 分钟一条，
	// 而 1 条/分钟 = 严格每 60 秒一条，正好是第三方实测的安全间隔下界
	// （冷启动号建议 60-120 秒一条）。要更慢就配 `2|1`、`5|3` 这种。
	waDefaultSendRateWindow = time.Minute
	waDefaultSendRateMax    = 1

	// waSendRateMaxWindow 是窗口上限（24h），waSendRateMaxCount 是窗口内条数上限。
	// 上限只用于挡手滑（`100000|1` 之类），不是业务建议值。
	waSendRateMaxWindow = 24 * time.Hour
	waSendRateMaxCount  = 1000
)

// WaSendRate 是解析后的发送速率：滚动 window 内最多发 max 条。
//
// 【为什么窗口也是配置的一部分】固定 1 小时会让一个批次等满 1 小时才能续发，
// 而真正决定封号概率的是两条消息之间的间隔，不是小时总量。把窗口做成可配，
// 上层分批的「批间等待」就能直接取 window，两处永远同一个数。
type WaSendRate struct {
	Window time.Duration
	Max    int
}

// ParseWaSendRate 解析 `分钟|条数` 形态（如 "1|1"、"5|3"）。
//
// 容错口径：只容忍空白，不容忍缺项、0、负数、非数字、正号 —— 配错的速率会直接改变
// 出网节奏，静默纠错比拒绝更危险。调用方按 ok 决定「用默认」还是「报配置错」。
//
// 判据与后台写侧（t1job 的 model.validateSendRate / parseStrictInt）**逐字对齐**：
// 同一个串在配置文件里能用、在后台存不进去（或反之）会造出「为什么这里能填那里不能」
// 这类没法解释的问题。所以两段都走 waSendRateSegment 的纯数字判定，不用 strconv.Atoi
// 裸判（Atoi 认 "+1"）。
func ParseWaSendRate(raw string) (WaSendRate, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return WaSendRate{}, false
	}
	parts := strings.Split(s, "|")
	if len(parts) != 2 {
		return WaSendRate{}, false
	}
	mins, ok := waSendRateSegment(parts[0])
	if !ok || mins <= 0 {
		return WaSendRate{}, false
	}
	count, ok := waSendRateSegment(parts[1])
	if !ok || count <= 0 {
		return WaSendRate{}, false
	}
	window := time.Duration(mins) * time.Minute
	if window > waSendRateMaxWindow || count > waSendRateMaxCount {
		return WaSendRate{}, false
	}
	return WaSendRate{Window: window, Max: count}, true
}

// waSendRateSegment 把一段解析为正整数：只接受纯 ASCII 数字，前后空白容忍，其余一律拒。
//
// 为什么不用 strconv.Atoi 直接判：Atoi 认 "+1"、认 "007"，也认 Unicode 数字之外的
// 各种形态，而写侧的正则 `^[0-9]+$` 不认 "+1"。两套规则并存时，" +1|1 " 这种串
// 会在配置文件里跑起来、在后台被拒，排查时看不到任何提示。
func waSendRateSegment(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// 超长数字（溢出 int）走到这里：按非法处理，不静默截断。
		return 0, false
	}
	return n, true
}

// EffectiveSendRate 返回单账号发送速率：配置文件 `send_rate`，缺省 1 分钟 1 条。
//
// 与 max_concurrent 的「<=0 即不限」刻意相反：这条闸防的是非官方客户端的风控封号，
// 「没配」读作「用保守缺省」而不是「裸奔」；要调高就显式配。
//
// 【本方法只是兜底】线上真正的速率来源是 sys_config 的 wa_send_rate（后台「基础配置」
// 页可改、改完即时生效）。调用方必须先读那一行，只有它不存在/非法时才回落到这里 ——
// 两处都读会把「后台改了但没生效」变成一类查不出原因的故障。
//
// 【旧键已删】send_max_per_hour 整条移除，不再有换算分支：它的语义（固定 1 小时窗口）
// 与「窗口可配」互斥，两个键并存会让优先级说不清。
func (c *WhatsappSessionsConfig) EffectiveSendRate() WaSendRate {
	if c == nil {
		return WaSendRate{Window: waDefaultSendRateWindow, Max: waDefaultSendRateMax}
	}
	if r, ok := ParseWaSendRate(c.SendRate); ok {
		return r
	}
	return WaSendRate{Window: waDefaultSendRateWindow, Max: waDefaultSendRateMax}
}

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

// EffectiveSendMaxPerMinute 已废弃：窗口不再固定 1 分钟，改用 EffectiveSendRate。
//
// 保留为薄包装只为不让外部调用方编译断裂；返回值是「当前速率窗口内的条数」，
// 窗口长度必须另外取 EffectiveSendRate().Window，不能假设是 1 分钟。
//
// Deprecated: 用 EffectiveSendRate。
func (c *WhatsappSessionsConfig) EffectiveSendMaxPerMinute() int {
	return c.EffectiveSendRate().Max
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
