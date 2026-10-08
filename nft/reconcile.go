package nft

import "context"

// NftReconcile 是 UNKNOWN 单的唯一出路：查单读回供应商事实。
//
// 无论读回什么状态都按 kind "Reconcile" 广播（含仍 UNKNOWN 的结果），再返回结果。
// 读回仍是 UNKNOWN 也返回 (res, nil)——「查了，还是没结论」是事实，不是本方法的失败；
// 重试节奏归下游（本包无常驻协程，对账节奏/定时器归下游用 localtimer 挂，§3.5/ADR-10）。
// 本包任何代码不得把 UNKNOWN 就地改写成 FAILED/SUCCESS。
//
// currentClient 失败时直接透传「未启动」错误（不经 orderCall 二次包装，§8 精确口径）。
func NftReconcile(ctx context.Context, orderNo string) (*NftResult, error) {
	c, err := currentClient()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Reconcile", func(ctx context.Context) (*NftResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// NeedsReconcile 门面调用方（下游）以它决定是否进对账队列：
// res 非终态（PENDING/UNKNOWN），或 err 满足 InSubmitDoubt ⇒ true。
//
// 注意 InSubmitDoubt(nil) 恒 false，但一个 UNKNOWN 的 (res,nil) 照样 NeedsReconcile==true——
// 这个组合钉死「UNKNOWN 只能靠再查收敛，不能靠重发收敛」（§9.2-1）。
func NeedsReconcile(res *NftResult, err error) bool {
	if InSubmitDoubt(err) {
		return true
	}
	if res == nil {
		return false
	}
	return !res.IsFinal()
}

// InSubmitDoubt 标记「不能证明请求未被受理」的错误集合：err 链上是 *ProviderError 且
// （Code 为 transport/read_body/bad_body，或 HTTPStatus 为 502/504）⇒ true。
//
// 这个集合与 §5.5 重试白名单互为表里：凡 InSubmitDoubt 为真的，绝不允许自动重发
// （7eb7b89 提现白名单口径的 nft 版）。4xx 明确拒绝、包络明确失败码 ⇒ false。
// bad_body 也在集合里（NF-F5）：HTTP 都 2xx 了，说明请求大概率已进对方业务层，
// 只是回执读不出——把它算作「没发过」就是给已受理的单另起新单号重下。
func InSubmitDoubt(err error) bool {
	pe, ok := ProviderErrorOf(err)
	if !ok {
		return false
	}
	switch pe.Code {
	case "transport", "read_body", CodeBadBody:
		return true
	}
	return pe.HTTPStatus == 502 || pe.HTTPStatus == 504
}
