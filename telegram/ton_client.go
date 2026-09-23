package telegram

import (
	"strings"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/paysdk"
	"touchgocore/vars"
)

// tonClient 把资金通道与「哪条 TON 网络、哪个 jetton」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换网络，会出现用新网关发旧链的单子。
type tonClient struct {
	ch      pay.Channel
	network string
}

// newTonClient 按配置装配 TON 链路。引用缺失、SDK 段没开、商户账户没点名，都返回 ok=false：
// 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// jetton 作为通道特有字段交给 paysdk：它会并进报文并追加到签名域末尾，
// 所以登记顺序由这里（唯一知道报文形态的一方）决定。
//
// 网络标识与 jetton 留在 telegram 段而不是引用里，是因为它们描述的是「这个 Bot
// 面向哪条链」，与供应商凭证无关；留空即按供应商为该 AppID 登记的默认值。
func newTonClient(cfg *config.Cfg) (*tonClient, bool) {
	if cfg == nil || cfg.Telegram == nil {
		return nil, false
	}
	tg := cfg.Telegram
	jetton := strings.TrimSpace(tg.Jetton)
	// jetton 合约地址同样在启动时就判：它是广播时真正的收端之一，写错要等到
	// 第一笔出款才暴露，而那时钱已经在一个谁也解不出私钥的地址上了。
	// 留空是合法取值，表示这一通道走原生 TON。
	if jetton != "" && !IsValidTONAddress(jetton) {
		vars.Info("TON 通道不启动: 配置的 jetton 合约地址 %s 不合法（既不是 48 位用户友好地址或 CRC16 不过，也不是 0: 开头的原始形态）", jetton)
		return nil, false
	}
	var extras []pay.ExtraField
	if jetton != "" {
		extras = append(extras, pay.ExtraField{Name: "jetton", Value: jetton})
	}
	res, err := paysdk.Open(cfg, "ton", tg.Ton, extras)
	if err != nil {
		vars.Info("TON 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("TON 通道已就绪: sdk=%s 商户号=%s jetton=%s", res.Section, res.MerchantID, jetton)
	return &tonClient{ch: res.Channel, network: strings.TrimSpace(tg.TonNetwork)}, true
}

// order 把链路配置补进订单里未指定的字段。
//
// 本通道只管 TON：调用方漏填币种时补成 TON，而不是发出一张「币种为空」的单。
func (c *tonClient) order(o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencyTON
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
