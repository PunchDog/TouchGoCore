// 本文件是 pay 包的装配层：把「通道段里的 SDK 引用」装配成一条可用的资金通道——
// 读配置表、按驱动标记取实现、注入密钥钩子。十条通道包（whatsapp/zalo/facebook/
// instagram/usdt/tron/bsc/sol/telegram）共用它，密钥钩子的键名口径在全仓只有一份。
// 本文件是全包唯一 import config 的文件（分层总说明见 types.go 的包文档）。
//
// 三个入口按「会不会动资金、动不动公链」分：Open 装配供应商链路（要商户账户），
// OpenService 装配只发消息的链路（不要求账户），OpenChain 装配出款直接上链的通道
// （多读一个 chain 子段、多过一道私钥注入钩子）。
package pay

import (
	"fmt"
	"strings"
	"time"

	"touchgocore/config"
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
	Channel Channel
}

// Open 按通道段的引用装配一条会动资金的通道：取 SDK 段 → 取商户账户 → 交密钥注入钩子 → 按驱动标记开。
//
// channel 是通道名（whatsapp/zalo/facebook/instagram/usdt/ton），只用于错误定位；extras 是该通道特有的报文字段
// （如 USDT 的 contract、TON 的 jetton），由通道包给出——它们追加在签名域末尾，
// 所以只有知道报文形态的一方才能定。
//
// 任何一步缺项都返回错误而不返回半成品：出款链路的配置错必须在启动时看得见。
func Open(cfg *config.Cfg, channel string, ref *config.PaySDKRef, extras []ExtraField) (*Resolved, error) {
	return open(cfg, channel, ref, extras, true, nil)
}

// OpenService 装配只发消息、不动资金的通道（验证码下发、登录换会话）。
//
// 与 Open 的差别只有一条：被引用的 SDK 段没有商户账户时照样能用。
// 出账主体这件事对这类链路没有意义，硬要一个只会逼配置里填假商户号；
// 但账户若存在，歧义规则与资金链路完全一致（多账户必须点名）。
func OpenService(cfg *config.Cfg, channel string, ref *config.PaySDKRef) (*Resolved, error) {
	return open(cfg, channel, ref, nil, false, nil)
}

// ChainParams 是通道包对「这条链上通道发的是哪个资产」的陈述，只有两个维度。
//
// 刻意不给通道包 Node/APIKey/PrivateKey：那些是接入点与凭证，权威值只应该在
// pay_sdks.<段>.chain 里；通道包只知道自己发的是哪张合约、哪张网。
// 反过来 chain 段也不写合约与网络——否则「usdt 包校验过的合约」与「广播时用的合约」
// 可以不是同一个，而这种错要到第一笔出款落链才看得见。
type ChainParams struct {
	// Contract 是代币合约地址（TRC20 为 Base58）；原生币通道留空。
	Contract string
	// Network 是公链网络标识（mainnet/shasta/nile）。
	Network string
}

// OpenChain 装配一条「出款与查单直接动公链」的通道，其余操作仍走被引用段的 HTTP 供应商实现。
//
// 与 Open 的差别只有两件事：一是被引用段必须带 chain 子段（节点、热钱包地址、API Key），
// 二是私钥在这里多过一次注入钩子（util.CallPayChainMsg+段名，载荷 *string）。
// 驱动名照旧由 pay_sdks.<段>.driver 决定：把这一段改成链上驱动名就换资金路径，
// 改回 generic_md5 就整条退回供应商侧，通道代码不动。
//
// extras 仍然照原样交给供应商侧报文与签名域（USDT 的 contract 就是这么带出去的）：
// 换驱动不该顺手改掉已经在发的字段，那会让「只换出款实现」变成一次全链路改口。
func OpenChain(cfg *config.Cfg, channel string, ref *config.PaySDKRef, params ChainParams, extras []ExtraField) (*Resolved, error) {
	return open(cfg, channel, ref, extras, true, &params)
}

func open(cfg *config.Cfg, channel string, ref *config.PaySDKRef, extras []ExtraField, needAccount bool, params *ChainParams) (*Resolved, error) {
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
	// 私钥注入排在构造之前，口径与上面的签名密钥一致：链上通道见到空私钥会直接拒绝装配，
	// 而「配置里留空、启动前由下游注入」正是让私钥不进版本库的那一条。
	// 两个钩子分开发：一次 Do 只交出一个 *string，共用键名会把两个秘密写进同一格。
	if sdk.Chain != nil {
		util.DefaultCallFunc.Do(util.CallPayChainMsg+section, &sdk.Chain.PrivateKey)
	}
	driver := strings.TrimSpace(sdk.Driver)
	if driver == "" {
		return nil, fmt.Errorf("通道[%s]引用的 pay_sdks.%s 未标 driver，不知道该用哪家 SDK 的实现", channel, section)
	}
	merchantID := ""
	if acc != nil {
		merchantID = acc.MerchantID
	}
	chain, err := buildChain(channel, section, sdk.Chain, params)
	if err != nil {
		return nil, err
	}
	ch, err := OpenDriver(driver, ProviderOptions{
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
		Chain:       chain,
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

// buildChain 把 chain 子段（接入点与凭证）与通道段给出的资产维度合成 ChainOptions。
//
// 缺哪一侧就在装配期说清楚：只配了 chain 段而通道没报合约与网络，出来的链上通道
// 「知道广播给哪个节点、却不知道发的是哪张合约」；反过来通道报了链上参数而段里没有
// chain 子段，等于配置写着要直接出款却没有任何接入点，两种都必须在第一笔出款之前拒掉。
//
// 返回 nil 只有一种情况：段里没配 chain 子段且通道没报链上参数（即纯供应商通道）。
func buildChain(channel, section string, src *config.PayChainConfig, params *ChainParams) (*ChainOptions, error) {
	if src == nil {
		if params == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("通道[%s]声明了链上出款，但引用的 pay_sdks.%s 没有 chain 子段（不知道该把交易广播给哪个节点、以哪个地址出资）", channel, section)
	}
	if params == nil {
		// 同一段凭证可能被多条通道引用：带 chain 子段的段也可以只用于供应商侧。
		// 这里不拒，交给驱动自己判定「要链上参数却没拿到」，免得把消息侧通道一起卡死。
		return nil, nil
	}
	return &ChainOptions{
		Node: strings.TrimSpace(src.Node),
		// TLS 只有写成 "off" 才降级为明文：公共接入点要求 TLS，而 grpc-go 拒绝
		// 「没声明传输安全」的拨号，这一项不能留给驱动去猜缺省。
		PlaintextNode: strings.EqualFold(strings.TrimSpace(src.TLS), "off"),
		HTTPBase:      strings.TrimSpace(src.HTTPBase),
		APIKey:        strings.TrimSpace(src.APIKey),
		WalletAddress: strings.TrimSpace(src.WalletAddress),
		PrivateKey:    strings.TrimSpace(src.PrivateKey),
		Contract:      strings.TrimSpace(params.Contract),
		Network:       strings.TrimSpace(params.Network),
		FeeLimitSun:   src.FeeLimit,
		Timeout:       seconds(src.TimeoutSec),
		MaxRetry:      src.MaxRetries,
	}, nil
}
