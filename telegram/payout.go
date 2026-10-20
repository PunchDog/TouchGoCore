package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 TON 侧的装配与门面。实现住在 telegram 包而不是新建一个 ton 包：
// 收付入口在 Telegram 这一侧，配置也是 telegram.ton 一段；与 Bot 长连接的区别只在
// 凭证与失败路径，不在归属（口径与被删前的 telegram/ton.go 一致）。
//
// 出款不再经过供应商：本包按 telegram.ton 引用的 pay_sdks 段装配一条链上通道
// （telegram/ton_onchain.go），提现单在本地签名后直接广播，结论从链上读回。
// 商户账户查询仍然留在被引用段的 HTTP 供应商实现里（没配就没有这一项）。

var (
	// tonActive 为 nil 表示资金链路未启动，对应操作会就地拒绝而不是发出请求。
	// 复用 telegramRunCtx 作为运行上下文，不新建第二个包级 ctx 快照：
	// 两份快照会在 TelegramStart 与 TonStart 先后写入时互相漂移。
	tonActive atomic.Pointer[tonClient]
)

// tonClient 把资金通道与「哪张 TON 网络」绑成一个不可拆的快照。
//
// 两者必须同时生效：只换通道不换网络，会出现用新网关发旧链的单子。
type tonClient struct {
	ch      pay.Channel
	network string
}

// tonNetworkLabel 是配置里允许的 TON 网络名。
//
// 只认 pay 的规范名（mainnet / testnet，留空按主网），刻意不接受 TonAPI 自己回显的
// "testnet-v4" 这类写法：pay.IsTestnet 不认识它，收下来就等于配置说测试网、
// 校验按主网走——测试网地址会被判成「与 network 不符」，而这条链的闸正是防这个的。
// 也别把 shasta/nile（TRON 的测试网）或 ton/jetton（链名与代币标准）当网络收进来：
// 填错的人在配置里看不出问题，等第一笔出款才看得见。
func isKnownTONNetwork(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "", pay.NetworkMainnet, pay.NetworkTestnet:
		return true
	}
	return false
}

// TonStart 按 telegram.ton 的引用装配 TON 资金链路。
//
// 引用缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// Bot 能不能起来与钱包通道配没配好互不牵制，两边都不该阻断整机启动。
func TonStart(ctx context.Context) {
	if ctx == nil {
		ctx = runCtx()
	}
	setRunCtx(ctx)
	if c, ok := newTonClient(corectx.CfgFrom(ctx)); ok {
		tonActive.Store(c)
		return
	}
	vars.Info("不启动TON")
}

// TonStop 摘掉 TON 资金通道并释放接入点。可重复调用。
func TonStop(ctx context.Context) {
	c := tonActive.Swap(nil)
	if c == nil {
		return
	}
	// 链上通道背后是带超时的 HTTP 客户端与连接池：只清空指针的话，
	// 第二次 start 就多一条不会关的连接，停启几次就把进程挂在泄漏的 socket 上。
	// 走接口断言而不是按类型开关——供应商侧的 pay.Provider 没有要释放的东西。
	if cl, ok := c.ch.(interface{ Close() error }); ok {
		if err := cl.Close(); err != nil {
			vars.Info("TON 通道停服时释放连接失败: %v", err)
		}
	}
}

// currentTon 取资金链路；未启动时返回明确错误而不是 panic。
func currentTon() (*tonClient, error) {
	if c := tonActive.Load(); c != nil {
		return c, nil
	}
	return nil, errors.New("TON 通道未启动（检查 telegram.ton.provider 是否引用了已开启的 pay_sdks 段与商户账户）")
}

// newTonClient 按配置装配 TON 链路。引用缺失、SDK 段没开、商户账户没点名，都返回 ok=false：
// 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
func newTonClient(cfg *config.Cfg) (*tonClient, bool) {
	if cfg == nil || cfg.Telegram == nil || cfg.Telegram.Ton == nil {
		return nil, false
	}
	tg := cfg.Telegram.Ton
	network := strings.TrimSpace(tg.Network)
	if !isKnownTONNetwork(network) {
		vars.Info("TON 通道不启动: telegram.ton.network=%s 不是 TON 的网络取值（mainnet / testnet，留空按主网）", network)
		return nil, false
	}
	jetton := strings.TrimSpace(tg.Jetton)
	if jetton != "" {
		// 硬拒而不是忽略：配了 jetton 的人想发的是代币，静默按原生币发出去就是把钱
		// 发成了另一种资产。本版只发原生 TON 的原因见 pay.ChainOptions 的 Decimals 注释
		// ——jetton 的小数位没有可验证的单一来源时，「按猜的位数转帐」是 10^n 倍的多付。
		vars.Info("TON 通道不启动: telegram.ton.jetton=%s 指向 jetton 合约，本仓的 TON 通道只发原生 TON", jetton)
		return nil, false
	}
	// 通道名写死而不是读包级变量：本函数与 pay 装配层之间只有一个字面量的契约，
	// 换成变量引用就可能与错误文案里的键名对不上。
	res, err := pay.OpenChain(cfg, "ton", tg.Provider, pay.ChainParams{Network: network}, nil)
	if err != nil {
		vars.Info("TON 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("TON 通道已就绪: sdk=%s 商户号=%s 网络=%s", res.Section, res.MerchantID, network)
	return &tonClient{ch: res.Channel, network: network}, true
}

// tonPublish 把归一后的资金回执广播给下游，键为 util.CallTonMsg+kind。
//
// 无人订阅不算失败：本包不要求下游一定要落库，只给一个不必层层转发的出口。
// 载荷是 *pay.PayResult，回调签名据此写。
func tonPublish(kind string, res *pay.PayResult) {
	if res == nil {
		return
	}
	if ok := util.DefaultCallFunc.Do(util.CallTonMsg+kind, res); !ok {
		vars.Debug("TON 回执无人订阅: kind=%s order_no=%s", kind, res.OrderNo)
	}
}

// tonOrderCall 是下单/查单门面共用的收尾：调用→补通道前缀→广播回执。
//
// ctx 为 nil 时回落到包级运行上下文，让「启动时快照的配置」仍然生效。
// 前置校验与状态归一都在 pay 侧做，这里只负责本包的留痕口径。
func tonOrderCall(ctx context.Context, kind string, call func(context.Context) (*pay.PayResult, error)) (*pay.PayResult, error) {
	if ctx == nil {
		ctx = runCtx()
	}
	res, err := call(ctx)
	if err != nil {
		return nil, fmt.Errorf("TON %s 失败: %w", kind, err)
	}
	tonPublish(kind, res)
	return res, nil
}

// tonOrder 把链路配置补进订单里未指定的字段。
//
// 本通道只管 TON：调用方漏填币种时补成 TON，而不是发出一张「币种为空」的单。
// 复制而不是就地改：调用方交来的 PayOrder 可能还要用于重试与落库，
// 门面把它改了就等于替上游决定了「这一单实际发出去的是什么」。
func tonOrder(c *tonClient, o *pay.PayOrder) *pay.PayOrder {
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
	cp := *o
	cp.Network, cp.Currency = network, currency
	return &cp
}

// TonWithdraw 直接上链出款：本地组外部消息、用热钱包私钥签名、经接入点广播。
//
// order.OrderNo 是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按外部消息 hash 查证并回 UNKNOWN（带 TxHash 供对账）。
// 收款地址与备注走 pay 的 ChainRule 唯一那一份校验，网络与币种不符本通道的单一律拒。
func TonWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Withdraw(ctx, tonOrder(c, order))
	})
}

// TonQuery 按订单号查链上处置结果（台账里的消息 hash → 交易 → 执行阶段）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func TonQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("TON Query 失败: 订单号（幂等键）为空")
	}
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// TonAccount 查我方在该供应商名下的 TON 商户账户（余额/可用/授信/费率/状态）。
//
// 注意这与链上热钱包余额是两件事：本方法读的是供应商侧的额度事实，
// 链上那个账户还有多少 TON 只有出款前置闸会去看（见 ton_onchain.go 的 precheckFunds）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func TonAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if strings.TrimSpace(q.Currency) == "" {
		cp := *q
		cp.Currency = pay.CurrencyTON
		q = &cp
	}
	if ctx == nil {
		ctx = runCtx()
	}
	info, err := c.ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("TON 账户查询失败: %w", err)
	}
	return info, nil
}
