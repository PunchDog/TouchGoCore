package bsc

import (
	"strings"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// client 把资金通道与「这条链路属于哪条网络」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换 network，会出现用新网关发旧链的单子。
type client struct {
	ch      pay.Channel
	network string
}

// newClient 按配置装配链路。引用缺失、SDK 段没开、商户账户没点名，都返回 ok=false：
// BNB 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// 合约地址（bsc.token ，BEP20 代币的 EVM 地址）作为通道特有字段交给 paysdk：
// 它会并进报文并追加到签名域末尾，所以登记顺序由这里（唯一知道报文形态的一方）决定。
func newClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Bsc == nil {
		return nil, false
	}
	contract := strings.TrimSpace(cfg.Bsc.Token)
	// 合约地址在这里就要过一遍格式：它是供应商广播时的收端之一，写错等于把这一通道
	// 的每一单都送进黑洞，而报错要等到第一笔出款之后。
	if contract != "" && !IsValidEVMAddress(contract) {
		vars.Info("BSC 通道不启动: 配置的 BEP20 合约地址 %s 不合法（应为 0x 开头的 40 位十六进制，混合大小写时还须过 EIP-55 校验和）", contract)
		return nil, false
	}
	network := strings.TrimSpace(cfg.Bsc.Network)
	var extras []pay.ExtraField
	if contract != "" {
		extras = append(extras, pay.ExtraField{Name: "contract", Value: contract})
	}
	res, err := paysdk.Open(cfg, "bsc", cfg.Bsc.Provider, extras)
	if err != nil {
		vars.Info("BSC 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("BSC 通道已就绪: sdk=%s 商户号=%s 合约=%s 网络=%s", res.Section, res.MerchantID, contract, network)
	return &client{ch: res.Channel, network: network}, true
}

// order 把链路配置补进订单里未指定的字段。
//
// 本包只管 BNB：调用方漏填币种时补成 BNB，而不是发出一张「币种为空」的单。
func (c *client) order(o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencyBNB
	}
	if network == o.Network && currency == o.Currency {
		return o
	}
	// 复制而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库，
	// 门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
	cp := *o
	cp.Network, cp.Currency = network, currency
	return &cp
}
