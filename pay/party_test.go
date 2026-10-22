package pay

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件是资金契约层的前置校验用例，迁移时补齐了 P2.5 断代后的新闸。
//
// 原用例住 pay/provider_test.go：那时 pay 还装着供应商客户端，CheckOrder 与一大
// 堆 HTTP 行为混在同一个包里。P5 之后 pay 只剩三个文件（types / party / transfer），
// 于是「订单长什么样才算能发」这件事成了本包唯一需要钉死的行为。

// TestCheckOrderGuards 幂等键、正整数金额、收款方三项前置校验。
func TestCheckOrderGuards(t *testing.T) {
	ok := &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}}
	if err := CheckOrder(ok); err != nil {
		t.Fatalf("合法订单被拒: %v", err)
	}
	cases := []struct {
		name  string
		order *PayOrder
	}{
		{"nil 订单", nil},
		{"缺订单号", &PayOrder{Amount: 1, To: &Party{Address: "TAddr"}}},
		{"订单号空白", &PayOrder{OrderNo: "  ", Amount: 1, To: &Party{Address: "TAddr"}}},
		{"零金额", &PayOrder{OrderNo: "O1", To: &Party{Address: "TAddr"}}},
		{"负金额", &PayOrder{OrderNo: "O1", Amount: -5, To: &Party{Address: "TAddr"}}},
		{"缺收款方", &PayOrder{OrderNo: "O1", Amount: 1}},
		// 契约断代后新增的闸：To 在但整个人是空的、或写了一个没登记的性质，
		// 都必须在发出之前被拦下，而不是让下游拿到一张「发给空地址」的单。
		{"收款方整段空白", &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{}}},
		{"收款方性质未登记", &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Kind: "unionpay", Address: "6222"}}},
		{"银行收款方缺卡号", &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Kind: PartyBank, Bank: &BankAccount{}}}},
		{"From 性质未登记", &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}, From: &Party{Kind: "vault", Address: "TAddr"}}},
	}
	for _, cs := range cases {
		if err := CheckOrder(cs.order); err == nil {
			t.Errorf("%s 未被拒", cs.name)
		}
	}
}

// TestPartyKindInference 三处推断口径：Bank 段在场即 bank、有地址即 wallet、
// 只有商户号即 merchant。判错一个的结果是把卡号当地址拿去解析。
func TestPartyKindInference(t *testing.T) {
	cases := []struct {
		name string
		p    Party
		want PartyKind
	}{
		{"银行段优先", Party{Address: "6222", Bank: &BankAccount{AccountNo: "6222"}}, PartyBank},
		{"有地址是钱包", Party{Address: "TAddr"}, PartyWallet},
		{"只有商户号", Party{Account: "MCH-1"}, PartyMerchant},
		{"空", Party{}, ""},
		{"显式 Kind 优先", Party{Kind: PartyMerchant, Address: "TAddr"}, PartyMerchant},
	}
	for _, cs := range cases {
		if got := cs.p.KindOf(); got != cs.want {
			t.Errorf("%s: KindOf=%q，期望 %q", cs.name, got, cs.want)
		}
	}
	// 空白 Kind 与大小写不影响判定：配置里写 " WALLET " 也要认。
	p := Party{Kind: " WALLET ", Address: "TAddr"}
	if got := p.KindOf(); got != PartyWallet {
		t.Errorf("空白/大小写未归一: %q", got)
	}
}

// TestPartyNormalizeIsIdempotent 归一只能补空位、不得改已有值，且跑两遍与跑一遍相同。
func TestPartyNormalizeIsIdempotent(t *testing.T) {
	p := &Party{Account: "MCH-1"}
	p.Normalize()
	if p.Kind != PartyMerchant {
		t.Fatalf("Kind 未推断成 merchant: %q", p.Kind)
	}
	if p.Address != "MCH-1" {
		t.Fatalf("merchant 的 Address 未由 Account 补上: %q", p.Address)
	}
	before := *p
	p.Normalize()
	if *p != before {
		t.Fatalf("Normalize 不幂等: %+v → %+v", before, *p)
	}

	// 已有值不得被覆盖。
	q := &Party{Kind: PartyWallet, Address: "TAddr", Account: "MCH-1"}
	q.Normalize()
	if q.Address != "TAddr" {
		t.Fatalf("已有地址被覆盖: %q", q.Address)
	}
}

// TestCheckTargetDeclaresWhatEachChannelCanSend 通道能发到哪几类收款方必须显式声明。
//
// 银行代付本仓尚无实现：任何通道都该在这里给出明确错误，而不是落到地址解析里
// 报一句「TRON 收款地址 6222… 不合法」——那会让人以为填错了地址，而不是走错了通道。
func TestCheckTargetDeclaresWhatEachChannelCanSend(t *testing.T) {
	chainOnly := WithdrawTarget{Wallet: true}
	cases := []struct {
		name string
		to   *Party
		tgt  WithdrawTarget
		ok   bool
	}{
		{"链上收钱包", &Party{Kind: PartyWallet, Address: "TAddr"}, chainOnly, true},
		{"链上收银行", &Party{Kind: PartyBank, Bank: &BankAccount{AccountNo: "6222"}}, chainOnly, false},
		{"链上收商户号", &Party{Kind: PartyMerchant, Account: "MCH-1"}, chainOnly, false},
		{"账本通道收商户号", &Party{Kind: PartyMerchant, Account: "MCH-1"}, WithdrawTarget{Wallet: true, Merchant: true}, true},
		{"未登记性质", &Party{Kind: "unionpay", Address: "6222"}, chainOnly, false},
	}
	for _, cs := range cases {
		err := CheckTarget(&PayOrder{OrderNo: "O1", Amount: 1, To: cs.to}, cs.tgt)
		if cs.ok && err != nil {
			t.Errorf("%s: 本应放行，实得 %v", cs.name, err)
		}
		if !cs.ok && err == nil {
			t.Errorf("%s: 本应拒单，实得放行", cs.name)
		}
	}
	// 收款方为空时给的是「to 为空」而不是「性质未登记」——两者处置完全不同。
	if err := CheckTarget(&PayOrder{OrderNo: "O1", Amount: 1}, chainOnly); err == nil ||
		!strings.Contains(err.Error(), "收款方") {
		t.Errorf("空收款方应报「收款方为空」，实得 %v", err)
	}
}

// TestOrderSerializesAsSnakeCase 报文必须是供应商惯用的蛇形字段 + 整数金额：
// 缺 json tag 会直接透出 Go 字段名，而浮点金额会在元/分换算上丢精度。
//
// 断代后收付两端是嵌套结构：这一条同时钉住「地址不得再平铺回顶层」，
// 否则下游按新契约解析时会把 to 里的地址读成空。
func TestOrderSerializesAsSnakeCase(t *testing.T) {
	b, err := json.Marshal(&PayOrder{OrderNo: "O1", Amount: 1_000_000, Currency: CurrencyUSDT})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, ".") {
		t.Fatalf("金额出现浮点形态: %s", got)
	}
	for _, want := range []string{`"order_no":"O1"`, `"amount":1000000`, `"currency":"USDT"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("报文缺 %s，实得 %s", want, got)
		}
	}
	// 未填的可选字段不得以空串或 null 出现，否则供应商会当成显式赋值。
	// to 是必填项，刻意留着 `json:"to"`（不带 omitempty）：缺收款方时序列化成
	// `"to":null` 而不是默默消失，这样日志与回执里能一眼看出这是张没填收方的单。
	for _, bad := range []string{`"from"`, `"phone"`, `"code"`, `"extra"`, `"decimals"`} {
		if strings.Contains(got, bad) {
			t.Fatalf("空可选字段仍进报文 %s: %s", bad, got)
		}
	}
	if !strings.Contains(got, `"to":null`) {
		t.Fatalf("必填项 to 留空应序列化成 null 留痕: %s", got)
	}

	nested, err := json.Marshal(&PayOrder{
		OrderNo: "O2", Amount: 1, To: &Party{Kind: PartyWallet, Address: "TAddr", Memo: "8890"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nested), `"to":{"kind":"wallet","address":"TAddr"`) {
		t.Fatalf("收付两端应为嵌套结构，实得 %s", nested)
	}
	if strings.Contains(string(nested), `"address":"TAddr"`+"},"+`"memo"`) {
		t.Fatalf("地址被平铺回顶层: %s", nested)
	}
}

// TestNormalizeAccountStatus 未登记的账户状态词一律 unknown，不许滑到 active。
// 把「没看懂」当成「账户正常」，等于给一个被冻结的商户号继续送单。
func TestNormalizeAccountStatus(t *testing.T) {
	cases := map[string]string{
		"active":   AcctActive,
		" ACTIVE ": AcctActive,
		"frozen":   AcctFrozen,
		"closed":   AcctClosed,
		"":         AcctUnknown,
		"weird":    AcctUnknown,
		"供应商自造词":   AcctUnknown,
	}
	for raw, want := range cases {
		if got := NormalizeAccountStatus(raw); got != want {
			t.Errorf("NormalizeAccountStatus(%q)=%s，期望 %s", raw, got, want)
		}
	}
	if IsActive(AcctUnknown) {
		t.Fatal("unknown 不得判成 active")
	}
}

// TestIsFinal 只有 success/failed 是终态；pending 与 unknown 都必须留给对账。
func TestIsFinal(t *testing.T) {
	for s, want := range map[string]bool{
		StatusSuccess: true, StatusFailed: true,
		StatusPending: false, StatusUnknown: false, "": false,
	} {
		if got := IsFinal(s); got != want {
			t.Errorf("IsFinal(%q)=%v，期望 %v", s, got, want)
		}
	}
	if (*PayResult)(nil).IsFinal() {
		t.Fatal("nil 回执不得判为终态")
	}
}

// TestProviderErrorNoCredential 错误文案只含通道名与业务码，凭证不得顺带出去。
func TestProviderErrorNoCredential(t *testing.T) {
	e := &ProviderError{Channel: "whatsapp", Code: "401002", Msg: "签名校验失败"}
	got := e.Error()
	for _, bad := range []string{"SecretKey", "secret", "token", "AppID"} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(bad)) {
			t.Fatalf("错误文案含敏感字样 %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, "401002") {
		t.Fatalf("错误文案丢了错误码: %s", got)
	}
}

// TestIsProviderRetryable 可重试性只能由 ProviderError.Retryable 决定：
// 未包装的普通 error（含 nil）一律按不可重试处理，否则一次本地参数错误会被重发。
func TestIsProviderRetryable(t *testing.T) {
	if IsProviderRetryable(nil) {
		t.Fatal("nil 不得判为可重试")
	}
	if IsProviderRetryable(errPlain{}) {
		t.Fatal("非 ProviderError 不得判为可重试")
	}
	if IsProviderRetryable(&ProviderError{Code: "1"}) {
		t.Fatal("Retryable=false 不得判为可重试")
	}
	if !IsProviderRetryable(&ProviderError{Code: "1", Retryable: true}) {
		t.Fatal("Retryable=true 应判为可重试")
	}
	// 包了一层同样要能判出来：重试决策不该依赖错误被直接返回。
	if !IsProviderRetryable(fmtWrap{&ProviderError{Code: "1", Retryable: true}}) {
		t.Fatal("包装后丢失可重试性")
	}
	if pe, ok := ProviderErrorOf(fmtWrap{&ProviderError{Code: "9"}}); !ok || pe.Code != "9" {
		t.Fatalf("ProviderErrorOf 未能从包装里取出: %+v ok=%v", pe, ok)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "plain" }

type fmtWrap struct{ err error }

func (w fmtWrap) Error() string { return "wrapped: " + w.err.Error() }
func (w fmtWrap) Unwrap() error { return w.err }

// TestCheckAmountUnits 大额字符串金额：与 int64 Amount 二选一，且只接受纯数字正整数。
func TestCheckAmountUnits(t *testing.T) {
	const units = "100000000000000000000" // 100 USDT，18 位最小单位
	base := &PayOrder{OrderNo: "O1", AmountUnits: units, To: &Party{Kind: PartyWallet, Address: "TAddr"}}
	if err := base.Check(); err != nil {
		t.Fatalf("合法的 AmountUnits 被拒: %v", err)
	}
	v, err := base.AmountBig()
	if err != nil || v.String() != units {
		t.Fatalf("AmountBig 解析错: %v %v", v, err)
	}
	both := *base
	both.Amount = 1
	if err := both.Check(); err == nil {
		t.Fatal("Amount 与 AmountUnits 同时非零必须拒")
	}
	for _, bad := range []string{"0", "-1", "1.5", "12a", ""} {
		o := &PayOrder{OrderNo: "O1", AmountUnits: bad, To: &Party{Kind: PartyWallet, Address: "TAddr"}}
		if err := o.Check(); err == nil {
			t.Fatalf("非法 amount_units %q 被放行", bad)
		}
	}
	tooLong := &PayOrder{OrderNo: "O1", AmountUnits: strings.Repeat("9", 79), To: &Party{Kind: PartyWallet, Address: "TAddr"}}
	if err := tooLong.Check(); err == nil {
		t.Fatal("超长 amount_units 必须拒")
	}
}
