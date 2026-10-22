// Package bsc 是 BSC（BNB Smart Chain，EVM 形态）的链侧知识归属地。
//
// 本包交付一条会自己动公链的资金通道：USDT-BEP20 的出款、查单与链上余额查询。
// 与「把提现单交给 HTTP 供应商」的通道不同，这里的每一步都在本进程内完成——
// 组 ERC-20 transfer、用热钱包私钥做 EIP-155 签名、经 JSON-RPC 广播、按哈希查回执。
//
// 链侧知识留在这里而不是进 pay：地址怎么编码（EIP-55 大小写校验和）、签名怎么与
// 链 ID 绑定、gas 怎么计价、nonce 怎么排序，都是 BSC 那条链的知识，不是资金契约的知识。
//
// 三个原生币门面（BscWithdraw / BscQuery / BscAccount）仍是哨兵：本包只交付
// USDT-BEP20，原生 BNB 的出款与账户查询没有实现。保留函数签名而不是删掉，
// 是因为它们是包外可见的调用面（服务注册与下游代码按它写），返回哨兵比让上游
// 当场编译失败更容易收口——而「没接入」绝不能被读成「已受理」。
package bsc

import (
	"context"
	"errors"

	"touchgocore/pay"
)

// ErrPayoutNotWired 原生币（BNB）出款链路没有接入。
//
// 它是哨兵而不是普通错误：调用方要用 errors.Is 区分「本进程没接入这条链路」（部署缺陷，
// 该改配置或补实现）与「供应商/链上拒了这一单」（业务结果，该走状态机）。
// 把这三种情形都返回 nil 结果 + 这个错误，是为了让上游不可能把「没接入」误读成「已受理」。
var ErrPayoutNotWired = errors.New("BSC 原生币（BNB）出款未接入（本包只交付 USDT-BEP20，见 BscUsdtWithdraw）")

// BscWithdraw 原生 BNB 未接入：保留签名，不假装受理。
func BscWithdraw(context.Context, *pay.PayOrder) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// BscQuery 原生 BNB 未接入：查不出结论时给错误而不是给一个 UNKNOWN 回执——
// UNKNOWN 会被上游当成「发过了、待对账」，而这里连发都没发过。
func BscQuery(context.Context, string) (*pay.PayResult, error) {
	return nil, ErrPayoutNotWired
}

// BscAccount 原生 BNB 未接入：不返回零值账户，零值会被下游读成「余额不足」从而拒掉正常业务。
func BscAccount(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) {
	return nil, ErrPayoutNotWired
}
