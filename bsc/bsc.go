// Package bsc 是 BSC（BNB Smart Chain，EVM 形态）的链侧知识归属地：EVM 地址与
// EIP-55 校验和的校验、以及向 pay 登记的 BNB 收款端规则。
//
// 提现实现已在 2026-10 的资金面收口里删掉：全仓真正会动钱的出款路径只剩
// USDT-TRC20 一条（在 tron 包，本地签名后直接上链），其余通道不再各自维护一套
// 「把提现单交给 HTTP 供应商」的装配机器。
//
// 留下链侧知识而不是整包删干净，理由有两条：
//   - 地址算法与规则登记是「那条链的知识」，与谁去广播交易无关。删掉 pay 的 BNB 规则
//     会让 BNB 单走「未登记即放行」，那是资金安全退化而不是简化（口径见 pay/chainrule.go）。
//   - 三个门面的函数签名是包外可见的调用面（服务注册与下游代码都按它写），
//     保留函数并返回哨兵，比删掉函数让上游当场编译失败更容易收口。
package bsc

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
var ErrPayoutNotWired = errors.New("BSC 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// BscWithdraw 未接入：保留签名，不假装受理。
func BscWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// BscQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func BscQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// BscAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func BscAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
