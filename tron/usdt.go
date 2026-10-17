package tron

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"touchgocore/config"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// 本文件是 USDT-TRC20 侧的装配与门面。住在本包而不是 usdt 包的原因：
// 收款端校验与地址算法只有 TRON 一份（address.go），代币通道复用同一条链的规则，
// 登记点分两处就会在下游同时 blank-import 两个包时重名 panic（见 chainrule.go）。
// touchgocore/usdt 这个 import 路径已被下游写死，所以它退成薄转发留在原地。

// usdtRoute 是 USDT 侧门面实例：装配读 cfg.Usdt，回执走 CallUsdtMsg。
var usdtRoute = route{
	label:     "USDT",
	channel:   "usdt",
	callback:  util.CallUsdtMsg,
	currency:  pay.CurrencyUSDT,
	newClient: newUSDTClient,
}

// UsdtStart 按配置装配 USDT 资金链路。
//
// 配置段缺失、SDK 段没开、密钥未注入，都只记日志并返回：
// 资金通道没配好不该阻断整机启动，但必须留下可查的痕迹。
func UsdtStart(ctx context.Context) { usdtRoute.start(ctx) }

// UsdtStop 摘掉供应商客户端。本包无常驻协程，清空指针即可；可重复调用。
func UsdtStop(ctx context.Context) { usdtRoute.stop() }

// newUSDTClient 按配置装配 USDT 链路。引用缺失、SDK 段没开、商户账户没点名，都返回 ok=false：
// USDT 出款链路宁可不起，也不能带着「不知道该从哪个商户号出钱」的快照上线。
//
// 合约地址作为通道特有字段交给 pay 装配层：它会并进报文并追加到签名域末尾，
// 所以登记顺序由这里（唯一知道报文形态的一方）决定。
func newUSDTClient(cfg *config.Cfg) (*client, bool) {
	if cfg == nil || cfg.Usdt == nil {
		return nil, false
	}
	contract := strings.TrimSpace(cfg.Usdt.Contract)
	// 合约地址在这里就要过一遍格式：它是供应商广播时的收端之一，写错等于把这一通道
	// 的每一单都送进黑洞，而报错要等到第一笔出款之后。
	if contract != "" && !IsValidTRONAddress(contract) {
		vars.Info("USDT 通道不启动: 配置的 TRC20 合约地址 %s 不合法（长度、版本字节或末尾 4 字节校验和对不上）", contract)
		return nil, false
	}
	network := strings.TrimSpace(cfg.Usdt.Network)
	if isTokenStandard(network) {
		// trc20 是「这张合约跑在 TRON 上」的标准名，不是网络名。混填进 network 的
		// 后果是换测试网时无从表达，而且供应商侧多半按主网口径处理。
		vars.Info("USDT 通道不启动: usdt.network=%s 是代币标准而不是公链网络，网络取值用 mainnet / shasta / nile，合约在 usdt.contract", network)
		return nil, false
	}
	var extras []pay.ExtraField
	if contract != "" {
		extras = append(extras, pay.ExtraField{Name: "contract", Value: contract})
	}
	// 通道名写死而不是读 usdtRoute.channel：route 的字面量引用了本函数，本函数再回头
	// 取 route 的字段就成包级变量初始化循环（go build 报 initialization cycle）。
	//
	// 出款恒定走链上入口：本通道的提现是本地签名广播，商户账户查询仍留在
	// pay_sdks 段的 HTTP 供应商实现里（链上通道内部委托）。
	// 这里不给「退回供应商提现」留任何静默路径——被引用的段没配 chain 子段就装配失败、
	// 通道不启动并记下原因，因为「配置看起来开了链上、实际把单发给别人」是这一层最贵的错。
	res, err := pay.OpenChain(cfg, "usdt", cfg.Usdt.Provider,
		pay.ChainParams{Contract: contract, Network: network}, extras)
	if err != nil {
		vars.Info("USDT 通道不启动: %v", err)
		return nil, false
	}
	vars.Info("USDT 通道已就绪: sdk=%s 商户号=%s 合约=%s 网络=%s", res.Section, res.MerchantID, contract, network)
	return &client{ch: res.Channel, network: network}, true
}

// isTokenStandard 判断 network 里填的是不是代币标准名。
//
// 只列这几个已被误用过的写法，不做「不在已知网络表里就拒」：供应商自己的网络叫法
// 我们没资格断言，硬拦会把能用的配置拦死。
func isTokenStandard(network string) bool {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "trc20", "trc-20", "trc10", "erc20", "bep20", "bep-20":
		return true
	}
	return false
}

// UsdtWithdraw 直接上链出款：本地组 TRC20 transfer、用热钱包私钥签名、经 gRPC 广播。
//
// order.OrderNo 仍然是幂等键，但保证它的不再是供应商，而是本包的出款台账：
// 同一单号永不签第二笔，广播结果说不清时按 txid 查证并回 UNKNOWN（带 TxHash 供对账）。
// 收款地址与备注走 pay 的 ChainRule 唯一那一份校验，网络与合约不符本通道的单一律拒。
func UsdtWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := usdtRoute.currentClient()
	if err != nil {
		return nil, err
	}
	return usdtRoute.orderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Withdraw(ctx, usdtRoute.order(c, order))
	})
}

// UsdtQuery 按订单号查链上处置结果（台账里的 txid → 区块高度 → 确认数）。
//
// 台账里没有这一单号时给 UNKNOWN 而不是 FAILED：把「查不到」说成失败，
// 上游就会重下一单，而那正是双付。
func UsdtQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New(usdtRoute.label + " Query 失败: 订单号（幂等键）为空")
	}
	c, err := usdtRoute.currentClient()
	if err != nil {
		return nil, err
	}
	return usdtRoute.orderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// UsdtAccount 查我方在该供应商名下的 USDT 商户账户（余额/可用/授信/费率/状态）。
//
// 注意这与链上热钱包余额是两件事：本方法读的是供应商侧的额度事实，
// 链上那个账户还有多少 USDT/TRX 只有出款前置闸会去看（见 onchain.go 的 precheckFunds）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func UsdtAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := usdtRoute.currentClient()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if strings.TrimSpace(q.Currency) == "" {
		cp := *q
		cp.Currency = usdtRoute.currency
		q = &cp
	}
	if ctx == nil {
		ctx = usdtRoute.runCtx()
	}
	info, err := c.ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s 账户查询失败: %w", usdtRoute.label, err)
	}
	return info, nil
}
