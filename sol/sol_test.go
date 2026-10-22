package sol

import (
	"context"
	"errors"
	"testing"

	"touchgocore/pay"
)

// Solana 地址夹具：base58 解码后恰为 32 字节的 ed25519 公钥形态。
// 三个向量都是先定字节（1..32 递增、全 FF、按公式生成）再现编码并 roundtrip 核过的，
// 不是抄来的——抄来的串一旦本身解不出 32 字节，测试就会把长度判断的错误永远藏住。
const (
	solBytes1To32  = "4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"
	solAllFFAcct   = "JEKNVnkbo3jma5nREBBJCDoXFVeKkD56V3xKrvRmWxFG"
	solPatternAcct = "5PzNBPqQ1c4QcMg9zC2G5eF1P3nZRbZNWf471X2mYtBh"
)

// TestBase58DecodeRoundtrip 夹具的来历单独钉一次：解码后必须恰好 32 字节，
// 且内容与造向量时的原始字节一致——Solana 没有内嵌校验和，长度是本地唯一能核的性质。
func TestBase58DecodeRoundtrip(t *testing.T) {
	raw, err := base58Decode(solBytes1To32)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solBytes1To32, len(raw), err)
	}
	for i, b := range raw {
		if b != byte(i+1) {
			t.Fatalf("第 %d 字节=%02x，期望递增序列", i, b)
		}
	}
	raw, err = base58Decode(solAllFFAcct)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solAllFFAcct, len(raw), err)
	}
	for i, b := range raw {
		if b != 0xFF {
			t.Fatalf("第 %d 字节=%02x，期望 FF", i, b)
		}
	}
}

// TestIsValidSolanaAddress 32 字节 base58 的正负用例。
func TestIsValidSolanaAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{solBytes1To32, true, "1..32 递增字节"},
		{solAllFFAcct, true, "全 FF 字节"},
		{solPatternAcct, true, "公式生成字节"},
		{"  " + solBytes1To32 + "  ", true, "两侧空白应被 trim 后过关"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{solBytes1To32[:42], false, "少一位（解码不足 32 字节）"},
		{solAllFFAcct + "1", false, "尾部多一个前导零字符（解码 33 字节）"},
		{"1" + solBytes1To32, false, "首部多一个前导零字符"},
		// 0OIl 不在 Base58 字母表里，混进来必须当场拒。
		{"0wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 0"},
		{"OwBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 O"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4VigI", false, "含非法字符 I"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigl", false, "含非法字符 l"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A", false, "EVM 地址不是 Solana 地址"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 Solana 地址"},
	}
	// 逐位篡改：Solana 没有校验和，改一位仍然可能是「另一个格式合法的地址」——
	// 这里钉的是格式判定不会被篡改绕过，字母表外的替换必须拒。
	tampered := []rune(solPatternAcct)
	for i := range tampered {
		c := []rune{'0', 'O'}[i%2]
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + string(c) + string(tampered[i+1:]), false, "逐位替换为字母表外字符"})
	}
	for _, cs := range cases {
		if got := IsValidSolanaAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidSolanaAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestSOLRuleChecksAddress 本包的规则直接以 solRule 存在，不再经 pay 的注册表。
//
// 注册表已在 P5 随 pay 瘦身删除：规则只服务「本包自己那条链」，改成包内函数调用后
// 就不存在「忘了登记 ⇒ 未登记即放行」这条静默通道了。
func TestSOLRuleChecksAddress(t *testing.T) {
	rule := solRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32[:42]}}); err == nil {
		t.Error("长度不足的地址未被拦下")
	}
}

// TestSOLRuleRejectsMemo Solana 原生与 SPL 转账都没有 TON comment 那种认款机制：
// 填了就拒而不是忽略。
func TestSOLRuleRejectsMemo(t *testing.T) {
	rule := solRule{}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestPayoutInterfaceStaysWired 三个门面仍是包外可见的调用面，且一律返回未接入哨兵。
//
// 资金实现已删除，这里只钉两件事：签名没被人顺手改掉（下游按它编译），
// 以及「没接入」绝不被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = SolWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = SolQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = SolAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000000, To: &pay.Party{Address: solBytes1To32}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := SolWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := SolQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := SolAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := SolWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}
