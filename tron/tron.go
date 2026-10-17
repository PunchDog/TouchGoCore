// Package tron 是 TRON 链上资金通道的接入点：同链代币 USDT-TRC20 的出款门面，
// 以及原生币 TRX 的未接入哨兵。
//
// 目录、包名与币种常量按通道分：TRX 用 pay.CurrencyTRX，USDT 用 pay.CurrencyUSDT
// 读 cfg.Usdt。网络取值走 pay.NetworkMainnet 这类公链名，USDT 侧的合约位是
// usdt.contract（TRC20 的 Base58 地址）——把 trc20 当网络填进配置会被本包在启动时
// 拒掉（见 usdt.go）。
//
// 全仓真正会动钱的出款只有 USDT-TRC20 一条：本地组交易、私钥签名、经节点 gRPC 广播，
// 幂等由本包的出款台账保证（onchain.go、idempotent.go）。TRX 那条供应商提现链路已在
// 2026-10 的资金面收口里删除——链上原生币出款并没有对应实现，留着一条没人实测的
// 「把提现单交给 HTTP 供应商」的路径只是多一个能被配置点亮的出资口。三个 TRX 门面
// 保留（签名是包外可见调用面），一律返回 ErrPayoutNotWired。
//
// 地址算法与两条链规则的登记点都在本包（address.go、chainrule.go）：「地址怎么编码」
// 是这条链的知识，两种币共用同一个地址形态，两份实现迟早漂移。usdt 包只剩薄转发，
// 目的是保住 touchgocore/usdt 这个已被下游按 import 路径写死的接入点。
// TRX 的规则登记不随提现实现删除：删了它，带 TRX 币种的单会走「未登记即放行」，
// 那是资金安全退化而不是简化（口径见 pay/chainrule.go）。
//
// 本包不碰 SQL、不碰积分与额度；下游按 util.CallTronMsg+"Xxx" / util.CallUsdtMsg+"Xxx"
// 注册回调接手结果。与 whatsapp 通道相比这里没有登录链路：收付靠签名凭证即可。
//
// 供应商接入不在这个包里配置：usdt.provider 只是对 pay_sdks 一段的引用，凭证、端点表
// 与商户账户都集中在 pay_sdks，装配动作在 pay（assemble.go）。
package tron

import (
	"context"
	"errors"

	"touchgocore/pay"
)

// ErrPayoutNotWired TRX 侧没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条链」（部署缺陷，
// 该补实现或改走 USDT 通道）与「这一单被拒」（业务结果，该走状态机）。
// 三个门面一律返回 nil 结果加这个错误，是为了让「没接入」永远不可能被读成「已受理」。
var ErrPayoutNotWired = errors.New("TRON(TRX) 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// TronWithdraw 未接入：保留签名，不假装受理。
func TronWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// TronQuery 未接入：给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func TronQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// TronAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func TronAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
