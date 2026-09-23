package ustd

import (
	"strings"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// client 把资金通道与「这条链路属于哪条链、哪个合约」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换 network，会出现用新网关发旧链的单子。
type client struct {
	ch      pay.Channel
	network string
}

// newClient 按配置装配链路。引用缺失、SDK 段没开、商户账户没点名，都返回 ok=false：
// USDT 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// 合约地址作为通道特有字段交给 paysdk：它会并进报文并追加到签名域末尾，
// 所以登记顺序由这里（唯一知道报文形态的一方）决定。
func newClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Ustd == nil {
		return nil, false
	}
	contract := strings.TrimSpace(cfg.Ustd.Contract)
	var extras []pay.ExtraField
	if contract != "" {
		extras = append(extras, pay.ExtraField{Name: "contract", Value: contract})
	}
	res, err := paysdk.Open(cfg, "ustd", cfg.Ustd.Provider, extras)
	if err != nil {
		vars.Info("USDT 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("USDT 通道已就绪: sdk=%s 商户号=%s 合约=%s", res.Section, res.MerchantID, contract)
	return &client{ch: res.Channel, network: strings.TrimSpace(cfg.Ustd.Network)}, true
}

// order 把链路配置补进订单里未指定的字段。
//
// 本包只管 USDT：调用方漏填币种时补成 USDT，而不是发出一张「币种为空」的单。
func (c *client) order(o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencyUSDT
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
