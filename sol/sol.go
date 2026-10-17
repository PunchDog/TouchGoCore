// Package sol 是 Solana 侧的链侧知识归属地：Base58 地址的校验、以及向 pay 登记的
// SOL 收款端规则。Solana 地址是 ed25519 公钥的 Base58 编码（解出恰好 32 字节），
// 原生 SOL 与 SPL 代币共用这一个地址形态；代码内币种一律用 pay.CurrencySOL，
// 金额按 lamports（9 位最小单位）原样透传，测试网取 Solana 专名的 pay.NetworkDevnet。
//
// 备注口径同样是链侧知识而不是供应商偏好：Solana 的原生转账与 SPL 转账都没有 TON
// comment 那种附在转账上的认款机制，所以本包登记的规则是「填了就拒」而不是「填了忽略」
// （见 address.go 的 solRule）。
//
// 提现实现已在 2026-10 的资金面收口里删掉（充值接口已整条删除）：全仓真正会动钱的出款路径只剩
// USDT-TRC20 一条（在 tron 包，本地签名后直接上链），SOL 这一侧不再自己养一套
// 「把单交给 HTTP 供应商」的装配机器——供应商客户端、报文签名、回执广播随之删除。
// SolStart / SolStop 同时摘掉：包里没有剩下要装配的东西，留一个空转的 Start 就是死代码，
// 更会留下一条「SOL 服务已启动」的启动日志，而实际上没有任何链路在跑。
// util.CallSolMsg 那组回执前缀还登记在 util 里，但本包已无回执可播：下游收不到订阅
// 回调时该读成「这条链没接入」，而不是「单还在路上」。
//
// 留下链侧知识而不是整包删干净，理由有两条：
//   - Solana 地址不像 TRON 那样内嵌校验和，「长度恰好 32 字节 + 字母表合法」是本地
//     唯一能白送的拦截。删掉 pay 的 SOL 规则会让 SOL 单走「未登记即放行」，那是
//     资金安全退化而不是简化（口径见 pay/chainrule.go）。
//   - 三个门面的函数签名是包外可见的调用面（服务注册与下游代码都按它写），
//     保留函数并返回哨兵，比删掉函数让上游当场编译失败更容易收口。
package sol

import (
	"context"
	"errors"

	"touchgocore/pay"
)

// ErrPayoutNotWired 本通道没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条链」（部署缺陷，
// 该修配置或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("SOL 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// SolWithdraw 未接入：保留签名，不假装受理。
//
// nil 订单也一样返回哨兵：参数校验拦不住「没接入」这个事实，先报部署缺陷才轮得到报订单。
func SolWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// SolQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func SolQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// SolAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func SolAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
