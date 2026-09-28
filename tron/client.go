package tron

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
// TRX 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// 代币合约 id（ tron.token ，TRC10 的纯数字 id）作为通道特有字段交给 paysdk：
// 它会并进报文并追加到签名域末尾，所以登记顺序由这里（唯一知道报文形态的一方）决定。
func newClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Tron == nil {
		return nil, false
	}
	token := strings.TrimSpace(cfg.Tron.Token)
	// TRC10 的合约 id 是供应商按数字登记的纯串，混进 TRC20 的 Base58 地址等于
	// 把这一通道的每一单送进错误的资产域——格式在这里就要过一遍，别等第一笔出款。
	if token != "" && !isTRC10TokenID(token) {
		vars.Info("TRON 通道不启动: 配置的 TRC10 代币 id %s 不是纯数字串（TRC20 合约地址请填 usdt.contract，不要填在这里）", token)
		return nil, false
	}
	network := strings.TrimSpace(cfg.Tron.Network)
	var extras []pay.ExtraField
	if token != "" {
		extras = append(extras, pay.ExtraField{Name: "contract", Value: token})
	}
	res, err := paysdk.Open(cfg, "tron", cfg.Tron.Provider, extras)
	if err != nil {
		vars.Info("TRON 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("TRON 通道已就绪: sdk=%s 商户号=%s 代币id=%s 网络=%s", res.Section, res.MerchantID, token, network)
	return &client{ch: res.Channel, network: network}, true
}

// isTRC10TokenID TRC10 代币 id 是否为纯数字串（如 1000101）。
func isTRC10TokenID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// order 把链路配置补进订单里未指定的字段。
//
// 本包只管 TRX：调用方漏填币种时补成 TRX，而不是发出一张「币种为空」的单。
func (c *client) order(o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencyTRX
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
