// Package paysdk 把「通道段里的 SDK 引用」装配成一条可用的资金通道。
//
// 它站在 config 与 pay 的交界上：config 只描述配置（不引 pay，免得将来两头引用成环），
// pay 只认与配置无关的 ProviderOptions（不引 config），于是「读配置表 + 按驱动标记取
// 实现 + 注入密钥钩子」这一小段装配逻辑需要一个两边都能引的地方。三条通道包
// （whatsapp/ustd/telegram）共用它，密钥钩子的键名口径在全仓就只有一份。
package paysdk

import (
	"fmt"
	"strings"
	"time"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
)

// Resolved 是一次成功装配的结果。
//
// 字段都可以进日志：段名、驱动名、商户号都是定位信息，凭证与基址不在其中。
type Resolved struct {
	// Section 是 pay_sdks 里的段名（通道段引用的那个名字）。
	Section string
	// Driver 是实际生效的驱动标记。
	Driver string
	// MerchantID 是我方在该供应商侧的商户号；只发消息、不动资金的链路可以为空。
	MerchantID string
	// Channel 是可直接调用的资金通道。
	Channel pay.Channel
}

// Open 按通道段的引用装配一条会动资金的通道：取 SDK 段 → 取商户账户 → 交密钥注入钩子 → 按驱动标记开。
//
// channel 是通道名（whatsapp/ustd/ton），只用于错误定位；extras 是该通道特有的报文字段
// （如 USDT 的 contract、TON 的 jetton），由通道包给出——它们追加在签名域末尾，
// 所以只有知道报文形态的一方才能定。
//
// 任何一步缺项都返回错误而不返回半成品：出款链路的配置错必须在启动时看得见。
func Open(cfg *config.Cfg, channel string, ref *config.PaySDKRef, extras []pay.ExtraField) (*Resolved, error) {
	return open(cfg, channel, ref, extras, true)
}

// OpenService 装配只发消息、不动资金的通道（验证码下发、登录换会话）。
//
// 与 Open 的差别只有一条：被引用的 SDK 段没有商户账户时照样能用。
// 出账主体这件事对这类链路没有意义，硬要一个只会逼配置里填假商户号；
// 但账户若存在，歧义规则与资金链路完全一致（多账户必须点名）。
func OpenService(cfg *config.Cfg, channel string, ref *config.PaySDKRef) (*Resolved, error) {
	return open(cfg, channel, ref, nil, false)
}

func open(cfg *config.Cfg, channel string, ref *config.PaySDKRef, extras []pay.ExtraField, needAccount bool) (*Resolved, error) {
	if ref.Empty() {
		return nil, fmt.Errorf("通道[%s]未引用任何 SDK 段（sdk 留空即不启用）", channel)
	}
	section := strings.TrimSpace(ref.SDK)
	sdk, acc, err := resolve(ref, sdkTable(cfg), needAccount)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]的 SDK 配置不可用: %w", channel, err)
	}
	// 密钥注入必须在构造之前：pay 侧见到空 secret_key 会直接拒绝启动，
	// 而「配置里留空、启动前由下游注入」正是让密钥不进版本库的那个口径。
	// 传的是字段指针，回调改的就是配置对象本身。
	util.DefaultCallFunc.Do(util.CallPaySDKMsg+section, &sdk.SecretKey)
	driver := strings.TrimSpace(sdk.Driver)
	if driver == "" {
		return nil, fmt.Errorf("通道[%s]引用的 pay_sdks.%s 未标 driver，不知道该用哪家 SDK 的实现", channel, section)
	}
	merchantID := ""
	if acc != nil {
		merchantID = acc.MerchantID
	}
	ch, err := pay.Open(driver, pay.ProviderOptions{
		Name:        channel,
		Driver:      driver,
		MerchantID:  merchantID,
		BaseURL:     sdk.BaseURL,
		Timeout:     seconds(sdk.TimeoutSec),
		MaxRetry:    sdk.MaxRetries,
		AppID:       sdk.AppID,
		SecretKey:   sdk.SecretKey,
		AuthHeader:  sdk.AuthHeader,
		TokenHeader: sdk.TokenHeader,
		NotifyURL:   sdk.NotifyURL,
		Endpoint:    sdk.Endpoint,
		Extras:      extras,
	})
	if err != nil {
		// 不透传 err 里可能的基址细节之外再加一层段名：报错要能一眼看出是哪一段配错。
		return nil, fmt.Errorf("通道[%s]用 pay_sdks.%s 装配失败: %w", channel, section, err)
	}
	return &Resolved{Section: section, Driver: driver, MerchantID: merchantID, Channel: ch}, nil
}

func resolve(ref *config.PaySDKRef, sdks map[string]*config.PaySDKConfig, needAccount bool) (*config.PaySDKConfig, *config.PayMerchantAccount, error) {
	if needAccount {
		return ref.Resolve(sdks)
	}
	return ref.ResolveService(sdks)
}

func sdkTable(cfg *config.Cfg) map[string]*config.PaySDKConfig {
	if cfg == nil {
		return nil
	}
	return cfg.PaySDks
}

// seconds 把配置里的秒数换算成超时；<=0 交给 pay 用默认值，不在这里另立一套默认。
func seconds(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}
