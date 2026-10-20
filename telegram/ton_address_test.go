package telegram

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// TON 地址夹具。account 部分是 0x00..0x1F 递增字节，五种标签形态各编一次。
//
// 校验和由 tongo 的 AccountID.ToHuman 生成（另一套实现，与本包不共享一行代码），
// 而不是用本包自己的 CRC 现算：本包曾经把 CRC 抄成 CCITT-FALSE（初值错），
// 自编的夹具全部自证通过，直到拿 tongo 的输出对拍才暴露——用被测实现产出的向量
// 永远测不出被测实现的错，这一条就是那件事的补丁。
//
// 为什么仍要现场造而不是抄一个网上的地址：本包原先的 jetton 夹具（EQDtFpEw...BB1q）
// 就是编的——48 位、EQ 开头、workchain 0，肉眼全然正常，校验和却对不上。
// 抄来的向量一旦本身是编的，测试就永远藏不住这个错。
const (
	tonAcctBounceable  = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHx2j"
	tonAcctNonBounce   = "UQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH0Bm"
	tonAcctTestnet     = "kQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH6Yp"
	tonAcctTestnetNonB = "0QAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH_vs"
	tonAcctMasterchain = "Ef8AAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH-Lr"
	tonAcctBadChecksum = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHx2b"
	tonAcctRaw         = "0:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	tonAcctRawMaster   = "-1:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

	// tonAcctRealWorld 是 tongo 官方示例里那条真实主网地址（一个真实存在的账户）。
	// 它不参与上面的形态表，只用来做下面那条「本包必须认得真地址」的断言——
	// 自编夹具在 CRC 抄错时照样自证通过，只有真地址能把它照出来。
	tonAcctRealWorld = "EQBszTJahYw3lpP64ryqscKQaDGk4QpsO7RO6LYVvKHSINS0"
)

// TestCRC16XMODEMCheckVector 钉住 CRC 的参数选择。
//
// XMODEM（多项式 0x1021、初值 0x0000）的 "123456789" 检值是 0x31C3；常被混用的
// CCITT-FALSE（同多项式、初值 0xFFFF）检值是 0x29B1。抄错初值的实现能编出地址、
// 也能验出自己的编造，唯独验不了任何真实地址——症状是全线拒单，最难往回查。
func TestCRC16XMODEMCheckVector(t *testing.T) {
	if got := crc16XMODEM([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("crc16XMODEM(\"123456789\")=0x%04X，期望 0x31C3", got)
	}
	if got := crc16XMODEM(nil); got != 0x0000 {
		t.Fatalf("空输入的初值应为 0x0000，实得 0x%04X", got)
	}
	// 夹具自检：抄错的地址在这一步就会露馅，不用等到跑校验用例。
	for _, s := range []string{
		tonAcctBounceable, tonAcctNonBounce, tonAcctTestnet,
		tonAcctTestnetNonB, tonAcctMasterchain, tonAcctRealWorld,
	} {
		if !IsValidTONAddress(s) {
			t.Errorf("夹具地址 %s 校验不过，说明它是编的或本实现错了", s)
		}
	}
	// 真地址的第二道判据：本包解出的 workchain 必须是 0（主网 basechain）。
	a, ok := parseTONAddress(tonAcctRealWorld)
	if !ok || a.Workchain != 0 || !a.Bounceable || a.Testnet {
		t.Errorf("真地址 %s 解析结果不对: %+v ok=%v", tonAcctRealWorld, a, ok)
	}
	if tonAcctBadChecksum == tonAcctBounceable {
		t.Fatal("坏校验和夹具与好夹具相同，这道反例是空的")
	}
}

func TestParseTONAddressForms(t *testing.T) {
	cases := []struct {
		addr      string
		ok        bool
		testnet   bool
		bounce    bool
		hasFlag   bool
		workchain int8
		desc      string
	}{
		{tonAcctBounceable, true, false, true, true, 0, "EQ 主网可退回"},
		{tonAcctNonBounce, true, false, false, true, 0, "UQ 主网不可退回（同一账户）"},
		{tonAcctTestnet, true, true, true, true, 0, "kQ 测试网可退回"},
		{tonAcctTestnetNonB, true, true, false, true, 0, "0Q 测试网不可退回"},
		{tonAcctMasterchain, true, false, true, true, -1, "Ef 主链分片"},
		{tonAcctBadChecksum, false, false, false, false, 0, "末位改动，校验和不过"},
		{tonAcctRaw, true, false, false, false, 0, "原始十六进制形态"},
		{tonAcctRawMaster, true, false, false, false, -1, "主链原始形态"},
		{"0:" + strings.Repeat("0", 63), false, false, false, false, 0, "十六进制少一位"},
		{"0:" + strings.Repeat("z", 64), false, false, false, false, 0, "十六进制含非法字符"},
		{"2:" + strings.Repeat("0", 64), false, false, false, false, 0, "不存在的分片号"},
		{tonAcctBounceable[:47], false, false, false, false, 0, "用户友好地址少一位"},
		{strings.ReplaceAll(tonAcctTestnetNonB, "_", "/"), false, false, false, false, 0, "换成标准 base64 字母表"},
		{"", false, false, false, false, 0, "空串"},
	}
	for _, cs := range cases {
		if cs.desc == "换成标准 base64 字母表" && cs.addr == tonAcctTestnetNonB {
			// 夹具本身不含 URL-safe 字符时这条反例是空的（上一轮就因为它悄悄退化过）。
			t.Fatalf("字母表反例没生效：夹具 %s 里没有被替换的字符", tonAcctTestnetNonB)
		}
		a, ok := parseTONAddress(cs.addr)
		if ok != cs.ok {
			t.Errorf("%s: parseTONAddress(%q) ok=%v，期望 %v", cs.desc, cs.addr, ok, cs.ok)
			continue
		}
		if !ok {
			continue
		}
		if a.Testnet != cs.testnet || a.Bounceable != cs.bounce || a.HasTestnetFlag != cs.hasFlag {
			t.Errorf("%s: 标志位不符 testnet=%v bounce=%v hasFlag=%v", cs.desc, a.Testnet, a.Bounceable, a.HasTestnetFlag)
		}
		if a.Workchain != cs.workchain {
			t.Errorf("%s: workchain=%d，期望 %d", cs.desc, a.Workchain, cs.workchain)
		}
	}
}

// TestTonRuleMatchesTestnetBit 地址自带的测试网位必须与订单的 network 对齐。
func TestTonRuleMatchesTestnetBit(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyTON)
	if !ok {
		t.Fatal("TON 的收款端规则没登记，本包可能没被链进测试二进制")
	}
	okCases := []pay.PayOrder{
		{Address: tonAcctBounceable, Network: pay.NetworkMainnet},
		{Address: tonAcctNonBounce, Network: pay.NetworkMainnet},
		{Address: tonAcctTestnet, Network: pay.NetworkTestnet},
		{Address: tonAcctRaw, Network: pay.NetworkMainnet},
		{Address: ""},
	}
	for i := range okCases {
		if err := rule.CheckAddress(&okCases[i]); err != nil {
			t.Errorf("合法组合被拒 %+v: %v", okCases[i], err)
		}
	}
	badCases := []pay.PayOrder{
		{Address: tonAcctTestnet, Network: pay.NetworkMainnet},
		{Address: tonAcctBounceable, Network: pay.NetworkTestnet},
		{Address: tonAcctBounceable, Network: pay.NetworkShasta},
	}
	for i := range badCases {
		if err := rule.CheckAddress(&badCases[i]); err == nil {
			t.Errorf("主网/测试网错配未被拦下 %+v", badCases[i])
		}
	}
	// network 留空按主网口径处理：测试网地址此时同样被拒，方向是宁可拒单不可错投。
	if err := rule.CheckAddress(&pay.PayOrder{Address: tonAcctTestnet}); err == nil {
		t.Error("network 留空时测试网地址应当被拒")
	}
}

func TestTonRuleMemo(t *testing.T) {
	rule, _ := pay.ChainRuleFor(pay.CurrencyTON)
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"8890", "复转到交易所 A 账户", strings.Repeat("x", maxMemoRunes)} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err != nil {
			t.Errorf("合法备注被拒 %q: %v", memo, err)
		}
	}
	for _, memo := range []string{strings.Repeat("x", maxMemoRunes+1), "第一行\n第二行", "tab\t分隔"} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err == nil {
			t.Errorf("越界备注未被拦下（长度 %d）", len(memo))
		}
	}
}
