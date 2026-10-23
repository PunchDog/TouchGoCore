package ton

import (
	"context"
	"fmt"
	"strings"

	tongoton "github.com/tonkeeper/tongo/ton"

	"touchgocore/pay"
)

// 本文件是 TON 的账户查询面。
//
// TON 没有供应商 HTTP 账户：QueryAccount 与 QueryOnchainAccount 读的都是
// 公链上那个地址还剩多少 TON。前者查配置热钱包（与 bsc/sol 的门面口径一致），
// 后者允许指定地址（水位门禁/按线路核对用）。两者都不返回授信与费率——
// 那些是供应商账本的事实，链上读不出来。

// QueryAccount 读默认热钱包的链上余额。
func (c *tonChannel) QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	if q != nil {
		if cur := strings.ToUpper(strings.TrimSpace(q.Currency)); cur != "" && cur != pay.CurrencyTON {
			return nil, fmt.Errorf("通道[%s]只有原生 TON 一个币种，查不了 %s", c.opt.Name, cur)
		}
	}
	return c.queryTONAccount(ctx, c.acct)
}

// QueryOnchainAccount 读指定地址（缺省为配置热钱包）的原生币余额。
func (c *tonChannel) QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error) {
	acct := c.acct
	if q != nil {
		if v := strings.TrimSpace(q.Contract); v != "" {
			return nil, fmt.Errorf("通道[%s]只发原生 TON，链上余额查询不支持合约/jetton", c.opt.Name)
		}
		if v := strings.TrimSpace(q.Address); v != "" {
			id, ok := tonAccountID(v)
			if !ok {
				return nil, fmt.Errorf("通道[%s]链上余额查询的地址不是合法 TON 地址", c.opt.Name)
			}
			acct = id
		}
		if v := strings.TrimSpace(q.Currency); v != "" && !strings.EqualFold(v, pay.CurrencyTON) {
			return nil, fmt.Errorf("通道[%s]只发原生 TON，链上余额查询不支持币种 %s", c.opt.Name, v)
		}
	}
	return c.queryTONAccount(ctx, acct)
}

// queryTONAccount 把链上账户状态归一成 pay.AccountInfo。
//
// Available 扣掉手续费余量：TON 的出款币与 gas 是同一资产，不扣会让
// AccountInfo.CanWithdraw 比 precheckFunds 宽松（余额刚好等于金额时链上会失败并烧费）。
// Balance 仍报链上全额；Frozen 不另造字段（链上没有单独的冻结余额）。
func (c *tonChannel) queryTONAccount(ctx context.Context, acct tongoton.AccountID) (*pay.AccountInfo, error) {
	state, err := c.gw.account(ctx, acct)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "读不出链上余额: " + c.redact(err.Error()), Retryable: true}
	}
	if state == nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "链上余额回执为空", Retryable: true}
	}
	status := pay.AcctUnknown
	switch strings.ToLower(strings.TrimSpace(state.Status)) {
	case "active":
		status = pay.AcctActive
	case "frozen":
		status = pay.AcctFrozen
	case "uninit", "nonexist":
		// 未部署/不存在的账户余额不可动用；不判 Closed，因为链上没有「已注销」这种状态。
		status = pay.AcctUnknown
	}
	available := int64(0)
	if status == pay.AcctActive {
		available = state.BalanceNanoton - c.feeReserve()
		if available < 0 {
			available = 0
		}
	}
	return &pay.AccountInfo{
		MerchantID: acct.ToRaw(),
		Currency:   pay.CurrencyTON,
		Balance:    state.BalanceNanoton,
		Available:  available,
		Status:     status,
		RawNote:    "链上余额 status=" + state.Status,
	}, nil
}
