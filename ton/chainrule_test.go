package ton

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// 本文件钉的是包内收口的 `checkOrderAddress`——它取代了原来登记在 pay 的
// ChainRule 表里的那条 TON 规则。
//
// 与 tron 侧同理：旧表「未登记即放行」，币种名拼错一个字母校验就整段消失；
// 现在规则是函数调用，漏接是编译错误。所以断言对象是这个收口函数本身，
// 而不是它内部用到的 tonRule——只测 rule 的话，把函数体改空依然全绿。

// TestTonCheckOrderAddressRejectsBadAddress 坏地址一定拒，合法地址必须放行。
func TestTonCheckOrderAddressRejectsBadAddress(t *testing.T) {
	bad := []string{
		tonAcctBadChecksum,
		"garbage",
		"EQ",            // 长度不够
		tonAcctRaw[:20], // 原始形态被截断
		"2:0001020304",  // 分片号越界
	}
	for _, addr := range bad {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: addr, Network: pay.NetworkMainnet}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("地址 %q 未被拦下", addr)
		}
	}

	good := []string{tonAcctBounceable, tonAcctNonBounce, tonAcctRaw, tonAcctRawMaster}
	for _, addr := range good {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: addr, Network: pay.NetworkMainnet}}); err != nil {
			t.Errorf("合法地址 %q 被拒: %v", addr, err)
		}
	}
}

// TestTonCheckOrderAddressMatchesNetwork 地址自带的测试网位必须与本单 network 对齐。
//
// 方向是选过的：network 留空按主网口径处理，带测试网标记的地址此时被拒。
// 拒单一准错，而把测试网地址送进主网报文是不可逆的资损。
func TestTonCheckOrderAddressMatchesNetwork(t *testing.T) {
	// 测试网地址 + 主网单：拒。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet, Network: pay.NetworkMainnet}}); err == nil {
		t.Error("测试网地址不能进主网单")
	}
	// 测试网地址 + network 留空：按主网口径，同样拒。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet}}); err == nil {
		t.Error("network 留空应按主网口径拒掉测试网地址")
	}
	// 测试网地址 + 测试网单：放行。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet, Network: pay.NetworkTestnet}}); err != nil {
		t.Errorf("测试网地址配测试网单被拒: %v", err)
	}
	// 主网地址 + 测试网单：拒（两边都得对上，不是只看一边）。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkTestnet}}); err == nil {
		t.Error("主网地址不能进测试网单")
	}
}

// TestTonCheckOrderAddressMemoLimits memo 只校长度与字符集，不强制非空——
// 该不该必填取决于收款方是不是交易所归集账户，那是业务侧的事实。
func TestTonCheckOrderAddressMemoLimits(t *testing.T) {
	base := func(memo string) *pay.PayOrder {
		return &pay.PayOrder{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: memo}}
	}
	if err := checkOrderAddress(base(strings.Repeat("1", maxMemoRunes))); err != nil {
		t.Errorf("恰好等于上限的备注被拒: %v", err)
	}
	if err := checkOrderAddress(base(strings.Repeat("1", maxMemoRunes+1))); err == nil {
		t.Error("超过上限的备注未被拦下")
	}
	// 控制字符会顺着报文进到对方的对账行与日志里，一条备注变成两行记录
	// 是能改变别人账本形状的。
	for _, memo := range []string{"12\n34", "12\r34", "\x00", "1\t2"} {
		if err := checkOrderAddress(base(memo)); err == nil {
			t.Errorf("含控制字符的备注 %q 未被拦下", memo)
		}
	}
	// 空与纯空白都算没有备注。
	for _, memo := range []string{"", "   ", "\n", "\t"} {
		if err := checkOrderAddress(base(memo)); err != nil {
			t.Errorf("空/纯空白备注 %q 被当成有值: %v", memo, err)
		}
	}
}

// TestTonCheckOrderAddressLeavesEmptinessToCheckOrder 空地址在这一层放行，
// 「收款方必填」归契约层；但必须钉住契约层确实还在判，否则两端一起放行。
func TestTonCheckOrderAddressLeavesEmptinessToCheckOrder(t *testing.T) {
	empty := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{}}
	if err := checkOrderAddress(empty); err != nil {
		t.Fatalf("格式层不该判非空: %v", err)
	}
	if err := pay.CheckOrder(empty); err == nil {
		t.Fatal("契约层必须拦住空收款地址")
	}
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单不该报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("无收款方的订单不该在格式层报错: %v", err)
	}
}

// TestTonCheckOrderAddressCoversAllRules 收口函数必须同时用上地址与 memo 两条规则。
// 两条规则各有正例与反例，缺哪一条都会有一格对不上。
func TestTonCheckOrderAddressCoversAllRules(t *testing.T) {
	rule := tonRule{}
	cases := []*pay.PayOrder{
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Address: tonAcctBadChecksum, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Address: "garbage"}},
		{To: &pay.Party{Address: tonAcctTestnet}},
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: strings.Repeat("9", maxMemoRunes+1)}},
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: "12\n34"}},
		{To: &pay.Party{}},
		{},
	}
	for i, o := range cases {
		want := rule.CheckAddress(o)
		if want == nil {
			want = rule.CheckMemo(o)
		}
		got := checkOrderAddress(o)
		if (got == nil) != (want == nil) {
			t.Errorf("case %d: checkOrderAddress 与两条规则的组合不等价: got %v want %v", i, got, want)
		}
	}
}
