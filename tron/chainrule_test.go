package tron

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// 本文件钉的是包内收口的 `checkOrderAddress`——它取代了原来登记在 pay 的
// ChainRule 表里的那条规则。
//
// 那张表的病不在「多一次查表」，而在「未登记即放行」：币种名拼错一个字母，
// 地址校验就静默失效，钱照样发得出去。现在规则是函数调用，调用点漏掉就是
// 编译错误。所以这里断言的对象必须是这个函数本身，而不是它内部用到的某一个
// rule——只测 rule 的话，把 checkOrderAddress 改成 `return nil` 依然全绿。

// TestCheckOrderAddressRejectsBadAddress 坏地址一定拒。
//
// 这一条同时也是「未登记不再放行」的替身：旧结构下忘了登记会让校验整段消失，
// 现在唯一的失效方式是有人把函数体改空，而这个用例会当场红。
func TestCheckOrderAddressRejectsBadAddress(t *testing.T) {
	bad := []struct {
		desc string
		addr string
	}{
		{"校验和不过", tronBogusChecksum},
		{"空串之外的垃圾", "not-a-tron-address"},
		{"版本字节不对的 BTC 地址", "1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2"},
		{"长度断了一截", tronRangeAcct[:len(tronRangeAcct)-1]},
		{"含非 Base58 字符", strings.Replace(tronRangeAcct, "T", "0", 1)},
	}
	for _, c := range bad {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: c.addr}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("%s: 地址 %q 未被拦下", c.desc, c.addr)
		}
	}

	// 合法地址必须放行，否则上面那组「全拒」也可能来自一个永远报错的实现。
	if err := checkOrderAddress(&pay.PayOrder{OrderNo: "O2", Amount: 1, To: &pay.Party{Address: tronRangeAcct}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
}

// TestCheckOrderAddressRejectsMemo TRON 没有 memo 概念，填了就拒。
//
// 地址合法、只有 memo 有问题时仍然必须拒：这条链上 memo 送不出去，
// 吞掉它等于让上游以为「备注已经带上了」，而对方永远收不到那串认款码。
func TestCheckOrderAddressRejectsMemo(t *testing.T) {
	for _, memo := range []string{"123456", " 1 ", "复转到交易所", "12\n34"} {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tronRangeAcct, Memo: memo}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	// 纯空白（含换行）按没有备注：真正不发出去的那一步在构造报文时 trim。
	for _, memo := range []string{"   ", "\n", "\t\r"} {
		o := &pay.PayOrder{OrderNo: "O2", Amount: 1, To: &pay.Party{Address: tronRangeAcct, Memo: memo}}
		if err := checkOrderAddress(o); err != nil {
			t.Errorf("纯空白备注 %q 被当成有值: %v", memo, err)
		}
	}
}

// TestCheckOrderAddressLeavesEmptinessToCheckOrder 空地址在这一层放行，
// 因为「收款方必填」是契约层的事（pay.CheckOrder）。
//
// 两个地方都判一遍不是冗余加分，而是判据漂移的入口；但「分层之后是否还有人在判」
// 必须钉住——否则某天有人把 pay.CheckOrder 的那条删掉，两端都会安安静静地放行。
func TestCheckOrderAddressLeavesEmptinessToCheckOrder(t *testing.T) {
	empty := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{}}
	if err := checkOrderAddress(empty); err != nil {
		t.Fatalf("格式层不该判非空: %v", err)
	}
	if err := pay.CheckOrder(empty); err == nil {
		t.Fatal("契约层必须拦住空收款地址，否则空地址会一路发到出款")
	}

	// nil 订单同样不 panic：格式规则只回答「这个地址对不对」。
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单不该报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("无收款方的订单不该在格式层报错: %v", err)
	}
}

// TestCheckOrderAddressCoversBothRules 收口函数必须同时用上地址与 memo 两条规则。
//
// 手工组合一遍 TRONRule 再对比：两条规则各自都有正例与反例，缺哪一条都会有一格对不上。
// 明天有人来往规则里加第三条时，这里也会立刻变成「收口函数漏了一条」。
func TestCheckOrderAddressCoversBothRules(t *testing.T) {
	rule := TRONRule{}
	cases := []*pay.PayOrder{
		{To: &pay.Party{Address: tronRangeAcct}},
		{To: &pay.Party{Address: tronBogusChecksum}},
		{To: &pay.Party{Address: "garbage"}},
		{To: &pay.Party{Address: tronRangeAcct, Memo: "m"}},
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
