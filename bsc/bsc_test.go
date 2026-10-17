package bsc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"touchgocore/pay"
)

// EVM 地址夹具：两个 EIP-55 校验和形态由 Keccak-256 现算（不是抄来的），
// 单一大小写的历史形态不带校验和语义，格式过关即放行。
const (
	evmChecksumMixed  = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmChecksumMixed2 = "0xDEaDbEeFdEADbEeFdEaDbEEFdEaDbeEFdEaDbeeF"
	evmAllLower       = "0xfb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"
	evmAllUpper       = "0xFB6916095CA1D6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmAllDigits      = "0x1234567890123456789012345678901234567890"
	// evmBogusChecksum 长度、前缀、字符集全对，只有大小写与校验和不符。
	evmBogusChecksum = "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3B"
)

// TestIsValidEVMAddress EIP-55 的正负用例：混合大小写必须等于校验和形态；
// 全小写、全大写、纯数字是历史形态，不携带校验和信息，格式过关即放行。
func TestIsValidEVMAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{evmChecksumMixed, true, "EIP-55 校验和形态"},
		{evmChecksumMixed2, true, "另一枚现算的校验和形态"},
		{evmAllLower, true, "全小写（无校验和语义的历史形态）"},
		{evmAllUpper, true, "全大写（同上）"},
		{evmAllDigits, true, "纯数字（无字母，大小写规则不适用）"},
		{evmBogusChecksum, false, "混合大小写但校验和不符"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{"fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a", false, "缺 0x 前缀"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3", false, "少一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3AA", false, "多一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3Z", false, "含非十六进制字符"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2b4D0F3a", false, "校验和形态上翻转两个字母的大小写"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 EVM 地址"},
	}
	// 逐位替换成 '0' / 'f' 的三向断言：结果若是混合大小写，除非恰好落在
	// EIP-55 形态上，否则必须被校验和拦下；单一大小写与纯数字则必须放行。
	body := []byte(evmChecksumMixed)
	for i := 2; i < len(body); i++ {
		orig := body[i]
		for _, cand := range []byte{'0', 'f'} {
			if cand == orig {
				continue
			}
			body[i] = cand
			addr := string(body)
			hex40 := addr[2:]
			// 替换后恰好落在校验和形态上的属巧合，不在被拒之列。
			if hex40 == toEIP55(hex40) {
				continue
			}
			mixed := hex40 != strings.ToLower(hex40) && hex40 != strings.ToUpper(hex40)
			if got := IsValidEVMAddress(addr); got == mixed {
				t.Errorf("替换后的地址 %q 判定不符：混合大小写=%v，实得 ok=%v", addr, mixed, got)
			}
		}
		body[i] = orig
	}
	// 逐位替换跑完后原夹具必须完好：循环里的写回如果漏了一位，这里会先红。
	if string(body) != evmChecksumMixed || !IsValidEVMAddress(evmChecksumMixed) {
		t.Fatalf("夹具 %s 在替换循环里被改坏了", evmChecksumMixed)
	}
	for _, cs := range cases {
		if got := IsValidEVMAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidEVMAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestToEIP55Derivation oracle 单独钉一次：对已知摘要手工推导前几位的大小写，
// 防止 toEIP55 的 nibble 取位（高低半字节 / 奇偶下标）被改反而无人察觉。
func TestToEIP55Derivation(t *testing.T) {
	// 夹具已核：evmChecksumHash("fb69...") 前两字节 0xF4 0x6C——
	// 第 0 位看高 nibble F >= 8，'f' 大写；第 1 位看低 nibble 4 < 8，'b' 保持小写；
	// 第 2 位看下一字节高 nibble 6 < 8，'6' 无字母；第 3 位看低 nibble C >= 8，'9' 无字母。
	want := "fB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	if got := toEIP55("fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"); got != want {
		t.Fatalf("toEIP55 推导不符:\n got=%s\nwant=%s", got, want)
	}
	// 反向钉 nibble 取位：大小写颠倒的串不能等于自身推导结果。
	if got := toEIP55("Fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"); got == "Fb6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A" {
		t.Fatal("nibble 取位方向不符：颠倒大小写的串应被推导回校验和形态")
	}
}

// TestBNBRuleRegistered 本包必须把 BNB 规则登记进 pay 的注册表。
func TestBNBRuleRegistered(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyBNB)
	if !ok {
		t.Fatalf("BNB 规则未登记，已登记的是 %v", pay.CurrenciesWithRules())
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: evmAllLower}); err != nil {
		t.Errorf("全小写合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: evmBogusChecksum}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestBNBRuleRejectsMemo EVM 转账没有 memo 概念：填了就拒而不是忽略。
func TestBNBRuleRejectsMemo(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyBNB)
	if !ok {
		t.Fatal("BNB 规则未登记")
	}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	if err := rule.CheckMemo(&pay.PayOrder{Memo: "   "}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestPayoutInterfaceStaysWired 三个门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = BscWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = BscQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = BscAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, Address: evmAllLower}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := BscWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := BscQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := BscAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := BscWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}
