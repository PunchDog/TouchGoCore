package ton

import (
	"context"
	"errors"
	"strings"
	"testing"

	"touchgocore/pay"
)

func TestQueryAccountMapsChainState(t *testing.T) {
	cases := []struct {
		name      string
		balance   int64
		status    string
		wantState string
		wantAvail int64
	}{
		{"active 扣余量", 100_000_000, "active", pay.AcctActive, 90_000_000},
		{"余额等于余量", defaultTONFeeReserveNanoton, "active", pay.AcctActive, 0},
		{"余额低于余量", 5_000_000, "active", pay.AcctActive, 0},
		{"frozen", 100_000_000, "frozen", pay.AcctFrozen, 0},
		{"uninit", 100_000_000, "uninit", pay.AcctUnknown, 0},
		{"nonexist", 100_000_000, "nonexist", pay.AcctUnknown, 0},
		{"未知状态词", 100_000_000, "weird", pay.AcctUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestTONChannel(t, &fakeTONGateway{balance: tc.balance, status: tc.status}, nil)
			info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if info.Balance != tc.balance || info.Available != tc.wantAvail || info.Status != tc.wantState {
				t.Fatalf("映射不符: %+v", info)
			}
			if info.Currency != pay.CurrencyTON || strings.TrimSpace(info.MerchantID) == "" {
				t.Fatalf("缺币种/地址: %+v", info)
			}
			if !strings.Contains(info.RawNote, tc.status) {
				t.Fatalf("RawNote 没有保留链上状态词: %q", info.RawNote)
			}
		})
	}
}

func TestQueryOnchainAccountAddressAndAssetGates(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{balance: 100_000_000, status: "active"}, nil)
	if _, err := c.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: tonAcctBounceable}); err != nil {
		t.Fatalf("合法地址查询失败: %v", err)
	}
	for _, q := range []*pay.OnchainAccountQuery{
		{Address: "not-a-ton-address"},
		{Contract: "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"},
		{Currency: pay.CurrencyUSDT},
	} {
		if _, err := c.QueryOnchainAccount(context.Background(), q); err == nil {
			t.Errorf("非法查询 %+v 未被拒", q)
		}
	}
	if _, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: pay.CurrencyUSDT}); err == nil {
		t.Error("非 TON 币种的账户查询未被拒")
	}
}

func TestQueryAccountRedactsGatewaySecret(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{acctErr: errors.New("401 with " + tonFakeAPIKey)}, nil)
	_, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
	pe, ok := pay.ProviderErrorOf(err)
	if !ok || pe.Code != "account_unread" || !pe.Retryable {
		t.Fatalf("读失败应为可重试 account_unread，实得 %v", err)
	}
	if strings.Contains(err.Error(), tonFakeAPIKey) {
		t.Fatal("API Key 进了错误文案")
	}
}
