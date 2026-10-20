package config

import (
	"strings"
	"time"
)

// 以下方法是 ZaloSessionsConfig 的「缺省值」口径：纯读取、不写日志。
// 所有方法都对 nil 接收者安全——整机没配 zalo.sessions 段时，Effective* 不能 panic。
//
// 下限不是随手取的保守值，而是 zca-js 社区给出的风控口径：
//   - getAllFriends / getAllGroups 极高风险，两次拉取间隔 >=5 分钟，必须缓存；
//   - getUserInfo / getGroupInfo 中风险，两次调用间隔 >=1.5 秒，缓存 TTL 10 分钟；
//   - sendMessage 中风险，连续两条随机 3-5 秒。
//
// 配置写得更小一律抬到下限：这几条是封号风险的软闸，不能靠「记得别配小」。
const (
	zaloDefaultQRTTL           = 5 * time.Minute
	zaloMinQRTTL               = 2 * time.Minute
	zaloDefaultRequestTimeout  = 30 * time.Second
	zaloMinRequestTimeout      = 5 * time.Second
	zaloDefaultFriendsCacheTTL = 5 * time.Minute
	zaloMinFriendsCacheTTL     = 5 * time.Minute
	zaloDefaultGroupsCacheTTL  = 5 * time.Minute
	zaloMinGroupsCacheTTL      = 5 * time.Minute
	zaloDefaultInfoMinInterval = 1500 * time.Millisecond
	zaloMinInfoMinInterval     = 1500 * time.Millisecond
	zaloDefaultInfoCacheTTL    = 10 * time.Minute
	zaloMinInfoCacheTTL        = 10 * time.Minute
	zaloDefaultSendMinDelay    = 3 * time.Second
	zaloMinSendMinDelay        = 3 * time.Second
	zaloDefaultSendMaxDelay    = 5 * time.Second
	zaloDefaultSendMaxPerHour  = 30
)

// EffectiveQRTTL 补默认扫码会话存活上限（QRTTLSec<=0 → 5 分钟，下限 2 分钟）。
func (c *ZaloSessionsConfig) EffectiveQRTTL() time.Duration {
	ttl := zaloDefaultQRTTL
	if c != nil && c.QRTTLSec > 0 {
		ttl = time.Duration(c.QRTTLSec) * time.Second
	}
	if ttl < zaloMinQRTTL {
		return zaloMinQRTTL
	}
	return ttl
}

// EffectiveRequestTimeout 补默认单请求超时（RequestTimeoutSec<=0 → 30 秒，下限 5 秒）。
func (c *ZaloSessionsConfig) EffectiveRequestTimeout() time.Duration {
	timeout := zaloDefaultRequestTimeout
	if c != nil && c.RequestTimeoutSec > 0 {
		timeout = time.Duration(c.RequestTimeoutSec) * time.Second
	}
	if timeout < zaloMinRequestTimeout {
		return zaloMinRequestTimeout
	}
	return timeout
}

// EffectiveListenerEnabled 返回入站消息长连接开关：段在而键缺省 → 开；
// 整段 nil → 关——没配 sessions 连消息侧都不启动，listener 无处可挂。
func (c *ZaloSessionsConfig) EffectiveListenerEnabled() bool {
	if c == nil || c.ListenerEnabled == nil {
		return c != nil
	}
	return *c.ListenerEnabled
}

// EffectiveFriendsCacheTTL 补默认好友缓存 TTL（<=0 → 5 分钟，下限 5 分钟）。
func (c *ZaloSessionsConfig) EffectiveFriendsCacheTTL() time.Duration {
	ttl := zaloDefaultFriendsCacheTTL
	if c != nil && c.FriendsCacheTTLSec > 0 {
		ttl = time.Duration(c.FriendsCacheTTLSec) * time.Second
	}
	if ttl < zaloMinFriendsCacheTTL {
		return zaloMinFriendsCacheTTL
	}
	return ttl
}

// EffectiveGroupsCacheTTL 补默认群列表缓存 TTL（<=0 → 5 分钟，下限 5 分钟）。
func (c *ZaloSessionsConfig) EffectiveGroupsCacheTTL() time.Duration {
	ttl := zaloDefaultGroupsCacheTTL
	if c != nil && c.GroupsCacheTTLSec > 0 {
		ttl = time.Duration(c.GroupsCacheTTLSec) * time.Second
	}
	if ttl < zaloMinGroupsCacheTTL {
		return zaloMinGroupsCacheTTL
	}
	return ttl
}

// EffectiveInfoMinInterval 补用户/群资料请求的最小间隔（<=0 → 1.5 秒，下限 1.5 秒）。
func (c *ZaloSessionsConfig) EffectiveInfoMinInterval() time.Duration {
	interval := zaloDefaultInfoMinInterval
	if c != nil && c.InfoMinIntervalMs > 0 {
		interval = time.Duration(c.InfoMinIntervalMs) * time.Millisecond
	}
	if interval < zaloMinInfoMinInterval {
		return zaloMinInfoMinInterval
	}
	return interval
}

// EffectiveInfoCacheTTL 补用户/群资料缓存 TTL（<=0 → 10 分钟，下限 10 分钟）。
func (c *ZaloSessionsConfig) EffectiveInfoCacheTTL() time.Duration {
	ttl := zaloDefaultInfoCacheTTL
	if c != nil && c.InfoCacheTTLSec > 0 {
		ttl = time.Duration(c.InfoCacheTTLSec) * time.Second
	}
	if ttl < zaloMinInfoCacheTTL {
		return zaloMinInfoCacheTTL
	}
	return ttl
}

// EffectiveSendMaxDelay 补单账号连续发送间隔上界（<=0 → 5 秒）。
func (c *ZaloSessionsConfig) EffectiveSendMaxDelay() time.Duration {
	delay := zaloDefaultSendMaxDelay
	if c != nil && c.SendMaxDelayMs > 0 {
		delay = time.Duration(c.SendMaxDelayMs) * time.Millisecond
	}
	if delay < zaloMinSendMinDelay {
		return zaloMinSendMinDelay
	}
	return delay
}

// EffectiveSendMinDelay 补连续发送间隔下界（<=0 → 3 秒，下限 3 秒）。
// 下界压过上界时按上界取，避免随机区间塌成空集。
func (c *ZaloSessionsConfig) EffectiveSendMinDelay() time.Duration {
	delay := zaloDefaultSendMinDelay
	if c != nil && c.SendMinDelayMs > 0 {
		delay = time.Duration(c.SendMinDelayMs) * time.Millisecond
	}
	if delay < zaloMinSendMinDelay {
		delay = zaloMinSendMinDelay
	}
	if max := c.EffectiveSendMaxDelay(); delay > max {
		return max
	}
	return delay
}

// EffectiveSendMaxPerHour 补单账号小时闸（<=0 → 30 条）。
// 与 max_concurrent 的「<=0 即不限」刻意相反：这条防的是非官方客户端的风控封号，
// 「没配」读作「用保守缺省」而不是「裸奔」。
func (c *ZaloSessionsConfig) EffectiveSendMaxPerHour() int {
	if c == nil || c.SendMaxPerHour <= 0 {
		return zaloDefaultSendMaxPerHour
	}
	return c.SendMaxPerHour
}

// EffectiveUserAgent 返回新登录使用的 UA；留空由 zcago 用其默认值。
func (c *ZaloSessionsConfig) EffectiveUserAgent() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.UserAgent)
}

// EffectiveLanguage 返回新登录使用的语言；留空由 zcago 用其默认值。
func (c *ZaloSessionsConfig) EffectiveLanguage() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Language)
}
