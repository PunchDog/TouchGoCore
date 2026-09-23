package ustd

import (
	"strings"
	"testing"

	"touchgocore/pay"
)

// TRON 地址夹具：官方 USDT-TRC20 合约，以及用 20 字节账户哈希现算校验和造出的几个。
//
// 造向量而不是抄网上的：抄来的地址一旦本身是编的，测试就会把「校验和不过」这件事
// 永远藏住——本仓的示例配置就中过这一枪（原 ustd.contract 值 34 位、版本字节对、
// 校验和不通过）。
const (
	tronOfficialUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	tronAllZeroAcct  = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
	tronRangeAcct    = "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"
	tronFFAcct       = "TZJozAg1ruapycCicgz31GxvYJ1FraLjZa"
	// tronBogusChecksum 长度与版本字节全对，只有末尾 4 字节校验和不过。
	tronBogusChecksum = "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR"
)

func TestIsValidTRONAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{tronOfficialUSDT, true, "官方 USDT-TRC20 合约"},
		{tronAllZeroAcct, true, "全零账户哈希"},
		{tronRangeAcct, true, "递增账户哈希"},
		{tronFFAcct, true, "全 F 账户哈希"},
		{tronBogusChecksum, false, "校验和不符的编造地址"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{tronRangeAcct[:33], false, "少一位"},
		{tronRangeAcct + "1", false, "多一位"},
		{strings.ReplaceAll(tronRangeAcct, "T", "1"), false, "首字符换成 1：前导零让解码长度变成 26 字节"},
		// 0OIl 这四个字符不在 Base58 字母表里，混进来必须当场拒，不能当成易混字符容错。
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6O", false, "含非法字符 O"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6l", false, "含非法字符 l"},
	}
	// 逐位篡改：每一位换成另一个合法字符都得被校验和判出来。
	tampered := []rune(tronRangeAcct)
	for i := range tampered {
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + "X" + string(tampered[i+1:]), false, "逐位篡改"})
	}
	for _, cs := range cases {
		if got := IsValidTRONAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidTRONAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

func TestTRC20RuleRejectsBadAddressBeforeSending(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyUSDT)
	if !ok {
		t.Fatal("USDT 的收款端规则没登记，本包可能没被链进测试二进制")
	}
	// 充值侧地址为空是合法形态：非空检查只在提现侧，由 pay.CheckOrder 负责。
	if err := rule.CheckAddress(&pay.PayOrder{Address: ""}); err != nil {
		t.Errorf("空地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{Address: tronRangeAcct}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	for _, bad := range []string{tronBogusChecksum, "UQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcY", "0x0000"} {
		if err := rule.CheckAddress(&pay.PayOrder{Address: bad}); err == nil {
			t.Errorf("非法地址 %s 未被拦下", bad)
		}
	}
	// 报错文案带原地址便于定位，但不得带密钥。
	err := rule.CheckAddress(&pay.PayOrder{Address: tronBogusChecksum})
	if err == nil || strings.Contains(err.Error(), testSecret) {
		t.Errorf("报错文案不安全或未给出: %v", err)
	}
}

func TestTRC20RuleRejectsMemo(t *testing.T) {
	rule, ok := pay.ChainRuleFor(pay.CurrencyTRX)
	if !ok {
		t.Fatal("TRX 与 USDT 共用地址形态，规则也要一并登记")
	}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{Memo: memo}); err == nil {
			t.Errorf("TRC20 侧的备注 %q 未被拦下", memo)
		}
	}
	// 纯空白按「没有备注」处理：真正不发出去的那一步在 NewOrderRequest 的 trim。
	if err := rule.CheckMemo(&pay.PayOrder{Memo: "   "}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestUstdRejectsBadChainFieldsWithoutSending 地址或备注不合链上规则的单，
// 必须一次请求都不发出、一条回执都不广播：发出去的最坏结果是钱动了但落错了地方。
func TestUstdRejectsBadChainFieldsWithoutSending(t *testing.T) {
	f, url := newFakeSupplier(t)
	startWith(t, payCfg(url, allEndpoints()))
	recorded()

	if _, err := UstdWithdraw(nil, &pay.PayOrder{OrderNo: "O9", Amount: 10, Address: tronRangeAcct, Memo: "8890"}); err == nil {
		t.Fatal("TRC20 带备注的提现应当被拒")
	}
	if _, err := UstdWithdraw(nil, &pay.PayOrder{OrderNo: "O8", Amount: 10, Address: "TAddr1"}); err == nil {
		t.Fatal("格式不对的收款地址应当被拒")
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("被拒的单广播了回执: %+v", got)
	}
	// 纯空白备注按「没有备注」处理，且不得出现在报文里：omitempty 认得出空串，
	// 认不出两个空格，供应商会把这串空格当成一条真有内容的备注。
	if _, err := UstdWithdraw(nil, &pay.PayOrder{OrderNo: "O7", Amount: 10, Address: tronRangeAcct, Memo: "  "}); err != nil {
		t.Fatalf("空白备注的正常提现被拒: %v", err)
	}
	reqs := f.seen()
	if len(reqs) != 1 {
		t.Fatalf("请求次数=%d，期望 1", len(reqs))
	}
	if strings.Contains(reqs[0].body, "memo") {
		t.Fatalf("空白备注进了报文: %s", reqs[0].body)
	}
}

func TestNewClientRejectsBogusContract(t *testing.T) {
	cfg := payCfg("https://pay.invalid", nil)
	if _, ok := newClient(cfg); !ok {
		t.Fatal("夹具合约应当能通过启动校验，否则本用例说明不了任何事")
	}
	cfg = payCfg("https://pay.invalid", nil)
	cfg.Ustd.Contract = tronBogusChecksum
	if c, ok := newClient(cfg); ok || c != nil {
		t.Errorf("校验和不过的合约地址不应让通道启动，实得 ok=%v", ok)
	}
	// 原生 TRX 通道留空合约是合法形态。
	cfg = payCfg("https://pay.invalid", nil)
	cfg.Ustd.Contract = ""
	if _, ok := newClient(cfg); !ok {
		t.Error("合约留空（原生 TRX）应当可启动")
	}
}

func TestNewClientRejectsTokenStandardAsNetwork(t *testing.T) {
	for _, net := range []string{"trc20", "TRC20", "erc20", "bep-20"} {
		cfg := payCfg("https://pay.invalid", nil)
		cfg.Ustd.Network = net
		if c, ok := newClient(cfg); ok || c != nil {
			t.Errorf("network=%s 是代币标准不是网络，不该让通道启动，实得 ok=%v", net, ok)
		}
	}
	for _, net := range []string{"", pay.NetworkMainnet, pay.NetworkShasta, pay.NetworkNile, "supplier_custom_net"} {
		cfg := payCfg("https://pay.invalid", nil)
		cfg.Ustd.Network = net
		if _, ok := newClient(cfg); !ok {
			t.Errorf("network=%q 应当放行（未知写法不由本包断言），实得 ok=false", net)
		}
	}
}
