package usdt

import (
	"context"

	"touchgocore/pay"
	"touchgocore/tron"
)

// 下面三个门面是纯转调：语义、前置校验、回执广播键与错误文案都在 tron 包的
// USDT 侧门面里（tron/usdt.go），本包不加一层判断也不换一层口径。
// 签名逐字保留是对下游的契约（见 usdt.go 头注释），改动这里等于改下游。

// UsdtWithdraw 向供应商下 USDT 提现单，要求报文里已有收款地址。
func UsdtWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	return tron.UsdtWithdraw(ctx, order)
}

// UsdtQuery 按订单号查供应商侧的处置结果。
func UsdtQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	return tron.UsdtQuery(ctx, orderNo)
}

// UsdtAccount 查我方在该供应商名下的 USDT 商户账户（余额/可用/授信/费率/状态）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，这里给的只是一次供应商侧的快照。
func UsdtAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	return tron.UsdtAccount(ctx, q)
}
