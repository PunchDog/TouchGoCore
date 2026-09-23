package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"touchgocore/pay"
)

// TonRecharge 向供应商下 TON 充值单。
//
// order.OrderNo 是幂等键：同一单重复调用只是重发同一张单，不会产生第二笔资金动作。
// 返回 UNKNOWN 状态时不要直接重下单，先 TonQuery 核对供应商到底收没收到。
func TonRecharge(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Recharge", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Recharge(ctx, c.order(order))
	})
}

// TonWithdraw 向供应商下 TON 提现单，要求报文里已有收款地址。
func TonWithdraw(ctx context.Context, order *pay.PayOrder) (*pay.PayResult, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Withdraw", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.Withdraw(ctx, c.order(order))
	})
}

// TonQuery 按订单号查供应商侧的处置结果。
func TonQuery(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("TON Query 失败: 订单号（幂等键）为空")
	}
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	return tonOrderCall(ctx, "Query", func(ctx context.Context) (*pay.PayResult, error) {
		return c.ch.QueryOrder(ctx, orderNo)
	})
}

// TonAccount 查我方在该供应商名下的 TON 商户账户（余额/可用/授信/费率/状态）。
//
// 这是读操作，不广播回执：下游要的是当场判断（能不能提这么多），
// 而额度账本与风控都在业务侧，本包给的只是一次供应商侧的快照。
func TonAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	c, err := currentTon()
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if strings.TrimSpace(q.Currency) == "" {
		cp := *q
		cp.Currency = pay.CurrencyTON
		q = &cp
	}
	if ctx == nil {
		ctx = runCtx()
	}
	info, err := c.ch.QueryAccount(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("TON 账户查询失败: %w", err)
	}
	return info, nil
}
