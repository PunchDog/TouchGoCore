package sol

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
// SOL 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// SPL 代币合约地址（sol.token）作为通道特有字段交给 paysdk，键名沿用 contract，
// 与 usdt 侧口径一致——供应商按同一个字段名认「这一单走哪个代币合约」。
// 它会并进报文并追加到签名域末尾，所以登记顺序由这里（唯一知道报文形态的一方）决定。
func newClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Sol == nil {
		return nil, false
	}
	token := strings.TrimSpace(cfg.Sol.Token)
	// 代币合约地址同样在启动时就判：它是广播时真正的收端之一，写错要等到
	// 第一笔出款才暴露，而那时钱已经在一个谁也解不出私钥的地址上了。
	// 留空是合法取值，表示这一通道走原生 SOL。
	if token != "" && !IsValidSolanaAddress(token) {
		vars.Info("SOL 通道不启动: 配置的 SPL 代币地址 %s 不合法（应为 Base58 编码且解出恰好 32 字节）", token)
		return nil, false
	}
	network := strings.TrimSpace(cfg.Sol.Network)
	var extras []pay.ExtraField
	if token != "" {
		extras = append(extras, pay.ExtraField{Name: "contract", Value: token})
	}
	res, err := paysdk.Open(cfg, "sol", cfg.Sol.Provider, extras)
	if err != nil {
		vars.Info("SOL 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("SOL 通道已就绪: sdk=%s 商户号=%s 代币合约=%s 网络=%s", res.Section, res.MerchantID, token, network)
	return &client{ch: res.Channel, network: network}, true
}

// order 把链路配置补进订单里未指定的字段。
//
// 本包只管 SOL：调用方漏填币种时补成 SOL，而不是发出一张「币种为空」的单。
func (c *client) order(o *pay.PayOrder) *pay.PayOrder {
	network := strings.TrimSpace(o.Network)
	if network == "" {
		network = c.network
	}
	currency := strings.TrimSpace(o.Currency)
	if currency == "" {
		currency = pay.CurrencySOL
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
