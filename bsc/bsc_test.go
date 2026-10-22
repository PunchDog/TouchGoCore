package bsc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"touchgocore/pay"
)

// EVM 地址夹具：两个 EIP-55 校验和形态按官方规则现算（并与 go-ethereum 的
// Address.Hex() 逐字对拍过），单一大小写的历史形态不带校验和语义，格式过关即放行。
const (
	evmChecksumMixed  = "0xfb6916095cA1d6Ab8C1E3E5E1C0e5a2C2b4d0F3A"
	evmChecksumMixed2 = "0xDeaDbeefdEAdbeefdEadbEEFdeadbeEFdEaDbeeF"
	evmAllLower       = "0xfb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"
	evmAllUpper       = "0xFB6916095CA1D6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmAllDigits      = "0x1234567890123456789012345678901234567890"
	// evmBogusChecksum 与 evmChecksumMixed 只差一位字母的大小写，
	// 长度、前缀、字符集全对，只有校验和不符。
	evmBogusChecksum = "0xfb6916095ca1d6Ab8C1E3E5E1C0e5a2C2b4d0F3A"
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

// TestToEIP55OfficialVectors 用 EIP-55 的官方测试向量钉住推导本身。
//
// 这些向量是本包唯一的「外部真源」：只用自算的夹具时，推导与校验一旦同错同对
// （比如把摘要的整个字节拿去比 >= 8，而不是按奇偶取高低半字节），用例会全绿，
// 而真实地址全被判成非法。下面八个向量覆盖了全大写、全小写与真正带校验和的形态。
func TestToEIP55OfficialVectors(t *testing.T) {
	vectors := []string{
		"0x52908400098527886E0F7030069857D2E4169EE7",
		"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
		"0xde709f2102306220921060314715629080e2fb77",
		"0x27b1fdb04752bbc536007a920d24acb045561c26",
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	}
	for _, v := range vectors {
		body := v[2:]
		if got := "0x" + toEIP55(strings.ToLower(body)); got != v {
			t.Errorf("EIP-55 推导不符:\n got=%s\nwant=%s", got, v)
		}
		if !IsValidEVMAddress(v) {
			t.Errorf("官方校验和形态被拒: %s", v)
		}
	}

	// 逐位翻转字母大小写：只要翻转后仍是混合大小写，就必须被校验和拦下。
	// 这是比「固定几个坏串」更硬的判据——五分之四种单点手抖都能被它覆盖。
	body := []byte(evmChecksumMixed)
	for i := 2; i < len(body); i++ {
		orig := body[i]
		var flipped byte
		switch {
		case orig >= 'a' && orig <= 'f':
			flipped = orig - 32
		case orig >= 'A' && orig <= 'F':
			flipped = orig + 32
		default:
			continue
		}
		body[i] = flipped
		addr := string(body)
		hex40 := addr[2:]
		mixed := hex40 != strings.ToLower(hex40) && hex40 != strings.ToUpper(hex40)
		if got := IsValidEVMAddress(addr); got == mixed {
			t.Errorf("翻转第 %d 位大小写后的 %q 判定不符：混合大小写=%v，实得 ok=%v", i, addr, mixed, got)
		}
		body[i] = orig
	}
	if string(body) != evmChecksumMixed {
		t.Fatal("翻转循环把夹具改坏了")
	}
}

// TestBNBRuleChecksAddress 本包的规则直接以 bnbRule 存在，不再经 pay 的注册表。
//
// 注册表已在 P5 随 pay 瘦身删除：规则只服务「本包自己那条链」，改成包内函数调用后
// 就不存在「忘了登记 ⇒ 未登记即放行」这条静默通道了。
func TestBNBRuleChecksAddress(t *testing.T) {
	rule := bnbRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: evmAllLower}}); err != nil {
		t.Errorf("全小写合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: evmBogusChecksum}}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestBNBRuleRejectsMemo EVM 转账没有 memo 概念：填了就拒而不是忽略。
func TestBNBRuleRejectsMemo(t *testing.T) {
	rule := bnbRule{}
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
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = BscWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = BscQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = BscAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: evmAllLower}}
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
