package ton

import (
	"context"
	"strings"
	"testing"
	"time"

	"touchgocore/ledger"
	"touchgocore/pay"
)

func TestWithdrawRejectsPerOrderAsset(t *testing.T) {
	cases := []struct {
		name string
		from *pay.Party
		want string
	}{
		{
			name: "按单私钥",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, PrivateKey: pay.Secret("abcd")},
			want: "按单传入出款私钥",
		},
		{
			name: "按单合约",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, Contract: "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"},
			want: "按单指定合约",
		},
		{
			name: "按单网络",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, Network: pay.NetworkTestnet},
			want: "出款方却写着",
		},
		{
			name: "非钱包性质",
			from: &pay.Party{Kind: pay.PartyMerchant, Account: "merchant-1"},
			want: "From 性质",
		},
		{
			name: "非配置钱包",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctBounceable},
			want: "本仓只持有那一个钱包的私钥",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeTONGateway{}
			c, sign := newTestTONChannel(t, gw, nil)
			o := newTONOrder("O-PER-ORDER")
			o.From = tc.from
			_, err := c.Withdraw(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应拒且报错含 %q，实得 %v", tc.want, err)
			}
			if sign.count() != 0 || gw.sendCount() != 0 {
				t.Fatalf("被拒的单还是动了笔（签名 %d、广播 %d）", sign.count(), gw.sendCount())
			}
		})
	}
}

// TestPreparePinsFrom 台账必须记下出款钱包，后续同一单号不得换钱包。
func TestPreparePinsFrom(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{}, nil)
	o := newTONOrder("O-FROM")
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatalf("出款失败: %v", err)
	}
	entry, found := c.led.Get("O-FROM")
	if !found || entry.From != c.wallet {
		t.Fatalf("台账没有钉住 From: %+v found=%v", entry, found)
	}

	// 旧条目/持久台账里记了另一个钱包：同一单号再来时必须拒，不能签第二笔。
	other := newTONOrder("O-FROM-OTHER")
	if err := c.led.Put(&ledger.Entry{
		OrderNo: "O-FROM-OTHER", From: tonAcctBounceable, To: other.To.Address,
		Amount: other.Amount, State: ledger.StatePrepared,
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Withdraw(context.Background(), other)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同一单号换钱包必须拒，实得 %v", err)
	}
}

// TestWithdrawTrimsOrderNo 锁键与台账键同源：首尾空白的同一单号只能签一次。
func TestWithdrawTrimsOrderNo(t *testing.T) {
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	if _, err := c.Withdraw(context.Background(), newTONOrder(" WD-TRIM ")); err != nil {
		t.Fatalf("第一次出款失败: %v", err)
	}
	if _, err := c.Withdraw(context.Background(), newTONOrder("WD-TRIM")); err != nil {
		t.Fatalf("第二次出款失败: %v", err)
	}
	if sign.count() != 1 || gw.sendCount() != 1 {
		t.Fatalf("同一单号被签/广播了多次：签名 %d、广播 %d", sign.count(), gw.sendCount())
	}
	if _, ok := c.led.Get("WD-TRIM"); !ok {
		t.Fatal("台账里没有 trim 后的单号")
	}
}
