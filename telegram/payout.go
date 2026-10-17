package telegram

import (
	"context"
	"errors"

	"touchgocore/pay"
)

// TON 的收付入口在 Telegram 侧，所以这三个门面留在本包；实现（客户端装配、报文签名、
// 回执广播）已随 2026-10 的资金面收口删除，TonStart/TonStop 一并摘掉：没有剩下要装配的
// 东西，留个空转的 Start 只会写出一条「TON 已启动」的日志，而实际没有任何链路在跑。
// util.CallTonMsg 前缀仍登记在 util 里，但本包已无回执可播——下游收不到订阅回调时该读成
// 「这条链没接入」，而不是「单还在路上」。
// 与出款实现一起留下的只有本文件的哨兵和 ton_address.go 的地址规则（口径见 pay/chainrule.go）。

// ErrPayoutNotWired 本通道没有接入出款链路。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条链」（部署缺陷，
// 该修配置或补实现）与「供应商拒了这一单」（业务结果，该走状态机）。
// 三个门面一律返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("TON 出款链路未接入（本仓资金出款只交付 USDT-TRC20 链上通道）")

// TonWithdraw 未接入：保留签名，不假装受理。
func TonWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// TonQuery 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func TonQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// TonAccount 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func TonAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
