package sol

import (
	"context"
	"strings"
	"testing"

	"touchgocore/pay"
)

// 本文件钉「收款地址与备注」这道闸：它既在包内函数上直接断言，
// 也从 Withdraw 的真入口再走一遍——只测函数的话，把调用点删掉测试仍然全绿。

// TestCheckOrderAddressAcceptsWallet 合法地址放行。
func TestCheckOrderAddressAcceptsWallet(t *testing.T) {
	for _, addr := range []string{solBytes1To32, solAllFFAcct, solPatternAcct, "  " + solBytes1To32 + "  "} {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: addr}}); err != nil {
			t.Errorf("合法地址 %q 被拒: %v", addr, err)
		}
	}
}

// TestCheckOrderAddressRejectsBadAddress 长度不足、含字母表外字符、跨链地址一律拒。
func TestCheckOrderAddressRejectsBadAddress(t *testing.T) {
	for _, cs := range []struct {
		name string
		addr string
	}{
		{"少一位", solBytes1To32[:42]},
		{"多一位", solAllFFAcct + "1"},
		{"含 0", "0wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"},
		{"EVM 地址", "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"},
		{"TRON 地址", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
	} {
		err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: cs.addr}})
		if err == nil {
			t.Errorf("%s: 非法地址未被拦下", cs.name)
			continue
		}
		if !strings.Contains(err.Error(), "不合法") {
			t.Errorf("%s: 错误文案未点明地址不合法: %v", cs.name, err)
		}
	}
}

// TestCheckOrderAddressRejectsMemo Solana 没有 TON comment 那种认款机制：填了就拒。
func TestCheckOrderAddressRejectsMemo(t *testing.T) {
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
	for _, memo := range []string{"123", "复转到交易所", " " + solBytes1To32} {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Memo: memo}}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
}

// TestCheckOrderAddressToleratesEmptyOrder 空订单/空收款方交给 pay.CheckOrder 拒，
// 这里不越位重复判：两处判同一件事，漂移的那一次通常是「新增一类收款方只改了一处」。
func TestCheckOrderAddressToleratesEmptyOrder(t *testing.T) {
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单在地址规则上不应报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("缺 To 在地址规则上不应报错: %v", err)
	}
}

// TestWithdrawRunsAddressGateBeforeRPC 从真入口证明这道闸在出款路径上。
//
// 通道的 rpc 字段留 nil：如果地址闸没拦住，下一步就会解引用它并 panic。
// 于是「返回地址错误而不是 panic」同时证明了两件事——闸在路径上，且它排在动钱之前。
func TestWithdrawRunsAddressGateBeforeRPC(t *testing.T) {
	c := solTestChannel(t)
	ctx := context.Background()

	bad := &pay.PayOrder{
		OrderNo:  "SOL-GATE-1",
		Amount:   1_000_000,
		Currency: pay.CurrencyUSDT,
		To:       &pay.Party{Kind: pay.PartyWallet, Address: solBytes1To32[:42]},
	}
	_, err := c.Withdraw(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法地址应被出款前置拒掉，实得 %v", err)
	}

	withMemo := *bad
	withMemo.To = &pay.Party{Kind: pay.PartyWallet, Address: solBytes1To32, Memo: "123"}
	_, err = c.Withdraw(ctx, &withMemo)
	if err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("带备注的订单应被出款前置拒掉，实得 %v", err)
	}
}
