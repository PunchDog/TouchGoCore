package nft

import (
	"strings"

	"touchgocore/config"
	"touchgocore/util"
	"touchgocore/vars"
)

// client 把资产通道与「这条链路属于哪条链/哪个网络」绑成一个不可拆的快照（bsc/client.go:12-18 同构）。
//
// 三者必须同时生效：只换通道不换 chain/network，会出现用新网关发旧链的单子。
// contract 是铸造/转账的默认合约，供 fillOrder 补默认用（§8 要求 fillOrder 补 chain/network/contract；
// §1.5 的 struct 草图只点名 chain/network，这里为落地 §8 的默认合约而多带一个非导出字段）。
type client struct {
	ch       NftChannel
	chain    string
	network  string
	contract string
}

// newClient 按配置装配链路：取 SDK 段 → 取签发主体 → 注密钥 → 按驱动标记 Open。
//
// 引用缺失、SDK 段没开、账户没点名、密钥没注入、驱动 Open 失败，都返回 ok=false：
// 资产链路宁可不起，也不能带着「不知道该从哪个主体签发」的快照上线（§8 就地拒绝口径）。
// 本函数把 paysdk.Open 的四步（取段/取账户/注密钥/按驱动 Open）同构复刻进来——
// paysdk 返回 pay.Channel，import 即破硬约束 1（ADR-3）。
func newClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Nft == nil {
		return nil, false
	}
	ref := cfg.Nft.Provider
	if ref.Empty() {
		return nil, false
	}
	sdk, acc, err := ref.Resolve(cfg.NftSDks)
	if err != nil {
		vars.Info("NFT 通道不启动: %v", err)
		return nil, false
	}
	contract := strings.TrimSpace(cfg.Nft.Contract)
	// 合约地址在这里就要过一遍形态：它是供应商广播时的资产域坐标，写错等于把这一通道
	// 的每一单都送进错误的资产域，而报错要等到第一次铸造之后。真实链上校验待供应商文档
	// 到位后补（NF-8 接入点口径），本阶段先拦最明显的形态错（含空白即非法地址）。
	if contract != "" && !validContractShape(contract) {
		vars.Info("NFT 通道不启动: 配置的默认合约地址形态非法（地址不得含空白）")
		return nil, false
	}
	section := strings.TrimSpace(ref.SDK)
	// 密钥注入必须在构造之前：NewProvider 见到空 secret_key 会直接拒绝启动，
	// 而「配置里留空、启动前由下游注入」正是让密钥不进版本库的那个口径。
	// 传的是字段指针，回调改的就是配置对象本身（§7）。
	util.DefaultCallFunc.Do(util.CallNftSDKMsg+section, &sdk.SecretKey)
	driver := strings.TrimSpace(sdk.Driver)
	if driver == "" {
		vars.Info("NFT 通道不启动: nft_sdks.%s 未标 driver，不知道该用哪家 SDK 的实现", section)
		return nil, false
	}
	merchantID := ""
	if acc != nil {
		merchantID = acc.MerchantID
	}
	ch, err := Open(driver, ProviderOptions{
		Name:        "nft",
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
	})
	if err != nil {
		vars.Info("NFT 通道不启动: 用 nft_sdks.%s 装配失败: %v", section, err)
		return nil, false
	}
	vars.Info("NFT 通道已就绪: sdk=%s 商户号=%s 链=%s 网络=%s", section, merchantID,
		strings.TrimSpace(cfg.Nft.Chain), strings.TrimSpace(cfg.Nft.Network))
	return &client{
		ch:       ch,
		chain:    strings.TrimSpace(cfg.Nft.Chain),
		network:  strings.TrimSpace(cfg.Nft.Network),
		contract: contract,
	}, true
}

// fillOrder 把链路配置补进订单里未指定的字段：chain/network/contract 留空时取快照默认。
//
// 复制而不是就地改：调用方交来的 NftOrder 可能还要用于重试与落库，门面把它改了就等于
// 替上游决定了「这一单实际发出去的是什么」（bsc/client.go:50-70 论证同构）。
// OrderNo 是幂等键，任何路径都不得改写（§3.4），这里只碰 chain/network/contract。
func (c *client) fillOrder(o *NftOrder) *NftOrder {
	if o == nil {
		return o
	}
	chain := strings.TrimSpace(o.Chain)
	if chain == "" {
		chain = c.chain
	}
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	contract := strings.TrimSpace(o.Contract)
	if contract == "" {
		contract = c.contract
	}
	if chain == o.Chain && network == o.Network && contract == o.Contract {
		return o
	}
	cp := *o
	cp.Chain, cp.Network, cp.Contract = chain, network, contract
	return &cp
}

// validContractShape 是接入点阶段的合约形态粗闸：地址不得含空白。真实链上校验
// （EVM 校验和、TRON Base58、SOL Base58 长度）待供应商文档到位后补，本包不 import 链侧包。
func validContractShape(s string) bool {
	return !strings.ContainsAny(s, " \t\r\n")
}
