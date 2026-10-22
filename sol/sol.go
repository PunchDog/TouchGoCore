// Package sol 是 Solana 侧的链侧知识归属地：地址规则、SPL 出款、按签名查单、
// 热钱包 token account 余额。
//
// Solana 地址是 ed25519 公钥的 Base58 编码（解出恰好 32 字节），原生 SOL 与 SPL
// 代币共用这一个地址形态；代码内币种一律用 pay.CurrencyUSDT（本包只发 USDT-SPL）或
// pay.CurrencySOL（原生币手续费），金额按最小单位原样透传。
//
// 备注口径同样是链侧知识而不是供应商偏好：Solana 的原生转账与 SPL 转账都没有 TON
// comment 那种附在转账上的认款机制，所以本包登记的规则是「填了就拒」而不是「填了忽略」
// （见 address.go 的 solRule 与 chainrule.go 的 checkOrderAddress）。
//
// 本包交付的是 USDT-SPL 一条链上通道（usdt.go 的三门面 + SolStart/SolStop）：
// 本地组 transferChecked、热钱包私钥签名、经 JSON-RPC 广播，台账保证同一单号永不签
// 第二笔（ledger.go、onchain.go、query.go）。原生 SOL 的提现/查询/账户三个门面仍是
// 哨兵——本仓没有交付原生币出款，保留函数并返回哨兵比删掉函数让上游当场编译失败
// 更容易收口（见下方 ErrPayoutNotWired）。
package sol

import (
	"context"
	"errors"

	"touchgocore/pay"
)

// ErrPayoutNotWired 原生 SOL 的出款链路没有接入。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条链」（部署缺陷，
// 该修配置或补实现）与「链上拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
//
// 注意它只覆盖原生币：USDT-SPL 走 SolUsdtWithdraw / SolUsdtQuery / SolUsdtAccount。
var ErrPayoutNotWired = errors.New("SOL 原生币出款链路未接入（本包只交付 USDT-SPL，原生币调用请改用其他通道）")

// SolWithdraw 原生 SOL 出款未接入：保留签名，不假装受理。
//
// nil 订单也一样返回哨兵：参数校验拦不住「没接入」这个事实，先报部署缺陷才轮得到报订单。
func SolWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// SolQuery 原生 SOL 查询未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func SolQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// SolAccount 原生 SOL 账户查询未接入：不返回零值账户，零值会被下游读成「余额不足」。
func SolAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
