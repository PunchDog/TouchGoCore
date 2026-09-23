package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"touchgocore/pay"
)

// WhatsappRecharge 向供应商下充值单。
//
// order.OrderNo 是幂等键：同一单重复调用只是重发同一张单，不会产生第二笔资金动作。
// 返回 UNKNOWN 状态时不要直接重下单，先 WhatsappQuery 核对供应商到底收没收到。
func WhatsappRecharge(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	p, err := currentFund()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Recharge", func(ctx context.Context) (*pay.PayResult, error) {
		return p.Recharge(ctx, order)
	})
}

// WhatsappWithdraw 向供应商下提现单，要求报文里已有收款地址。
func WhatsappWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	p, err := currentFund()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return p.Withdraw(ctx, order)
	})
}

// WhatsappQuery 按订单号查供应商侧的处置结果。
func WhatsappQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("whatsapp Query 失败: 订单号（幂等键）为空")
	}
	p, err := currentFund()
	if err != nil {
		return nil, err
	}
	return orderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return p.QueryOrder(ctx, orderNo)
	})
}

// WhatsappAccount 查我方在该供应商名下的商户账户（余额/可用/授信/费率/状态）。
//
// 本通道不预设币种：q.Currency 留空即按供应商为该商户号登记的默认口径返回，
// 与另两条按币种/合约区分账户的链路不同，这里补一个币种反而会被读成「就要这个币种」。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func WhatsappAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	p, err := currentFund()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if ctx == nil {
		ctx = runCtx()
	}
	info, err := p.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("whatsapp 账户查询失败: %w", err)
	}
	return info, nil
}
