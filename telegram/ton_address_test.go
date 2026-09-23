package telegram

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// TON 地址夹具。account 部分是 0x00..0x1F 与 0x20..0x3F 两段递增字节，
// 末尾两字节是按 CRC-16/CCITT-FALSE 现算的校验和。
//
// 为什么现场造而不是抄：本包原先的 jetton 夹具（EQDtFpEw...BB1q）就是编的——
// 48 位、EQ 开头、workchain 0，肉眼全然正常，校验和却对不上。
// 抄来的向量一旦本身是编的，测试就永远藏不住这个错。
const (
	tonAcctBounceable  = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH7Qb"
	tonAcctNonBounce   = "UQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH-ne"
	tonAcctTestnet     = "kQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHw-R"
	tonAcctTestnetNonB = "0QAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH1JU"
	tonAcctMasterchain = "Ef8AAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH0tT"
	tonAcctBadChecksum = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH7Qc"
	tonAcctRaw         = "0:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	tonAcctRawMaster   = "-1:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

// TestCRC16CCITTFalseCheckVector 钉住 CRC 的参数选择。
//
// CCITT-FALSE（初值 0xFFFF）的 "123456789" 检值是 0x29B1；常被混用的 XMODEM
// （同多项式、初值 0）检值是 0x31C3。抄错初值的实现能编出地址、也能验出自己的
// 编造，唯独验不了任何真实地址——症状是全线拒单，最难往回查。
func TestCRC16CCITTFalseCheckVector(t *testing.T) {
	if got := crc16CCITTFalse([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("crc16CCITTFalse(\"123456789\")=0x%04X，期望 0x29B1", got)
	}
	if got := crc16CCITTFalse(nil); got != 0xFFFF {
		t.Fatalf("空输入的初值应为 0xFFFF，实得 0x%04X", got)
	}
	// 夹具自检：抄错的地址在这一步就会露馅，不用等到跑校验用例。
	for _, s := range []string{
		tonAcctBounceable, tonAcctNonBounce, tonAcctTestnet,
		tonAcctTestnetNonB, tonAcctMasterchain,
	} {
		if !IsValidTONAddress(s) {
			t.Errorf("夹具地址 %s 校验不过，说明它是编的或本实现错了", s)
		}
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
		{strings.ReplaceAll(tonAcctNonBounce, "-", "+"), false, false, false, false, 0, "换成标准 base64 字母表"},
		{"", false, false, false, false, 0, "空串"},
	}
	for _, cs := range cases {
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

// TestNewTonClientRejectsBogusJetton 配置里编造的 jetton 地址必须在启动时就拒：
// 它是广播时真正的收端之一，等到第一笔出款才暴露就已经晚了。
func TestNewTonClientRejectsBogusJetton(t *testing.T) {
	cfg := payTonCfg("https://pay.invalid", nil)
	if _, ok := newTonClient(cfg); !ok {
		t.Fatal("夹具 jetton 应当通过启动校验")
	}
	cfg = payTonCfg("https://pay.invalid", nil)
	cfg.Telegram.Jetton = "EQDtFpEwcR-fm552Nv6h3FDFdv3TbHx8w9Wm7FqYqU8dBB1q"
	if c, ok := newTonClient(cfg); ok || c != nil {
		t.Errorf("校验和不过的 jetton 地址不应让通道启动，实得 ok=%v", ok)
	}
	cfg = payTonCfg("https://pay.invalid", nil)
	cfg.Telegram.Jetton = ""
	if _, ok := newTonClient(cfg); !ok {
		t.Error("jetton 留空（原生 TON）应当可启动")
	}
}

// TestTonWithdrawRejectsBadChainFieldsWithoutSending 错地址一次请求都不该发出。
func TestTonWithdrawRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newTonFakeSupplier(t)
	startTon(t, payTonCfg(url, tonEndpoints()))
	tonRecorded()
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O1", Amount: 100, Address: tonAcctBadChecksum}); err == nil {
		t.Fatal("校验和不过的地址应当被拒")
	}
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O2", Amount: 100, Address: tonAcctBounceable, Memo: "备注里\n换行"}); err == nil {
		t.Fatal("含控制字符的备注应当被拒")
	}
	if n := f.seen(); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
	if got := tonRecorded(); len(got) != 0 {
		t.Fatalf("被拒的单广播了回执: %+v", got)
	}
	// 带合法备注的提现走通，且 memo 进了报文与签名域。
	if _, err := TonWithdraw(nil, &pay.PayOrder{OrderNo: "O3", Amount: 100, Address: tonAcctBounceable, Memo: "8890"}); err != nil {
		t.Fatalf("合法提现被拒: %v", err)
	}
	_, sign, body := f.request(0)
	if !strings.Contains(body, `"memo":"8890"`) {
		t.Fatalf("备注没进报文: %s", body)
	}
	want := pay.SignMD5(tonSecret,
		"app1", tonMerchant, "O3", "100", pay.CurrencyTON, "mainnet", tonAcctBounceable, "", "8890", "",
		tonJetton)
	if sign != want {
		t.Fatalf("签名=%s，期望 %s", sign, want)
	}
}
