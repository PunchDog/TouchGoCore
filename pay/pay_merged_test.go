package pay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// TestSecretNeverLeaks 私钥会跟着 PayOrder 穿过日志、回调与错误包装；
// 类型必须在 fmt 的四种常用动词与 JSON 两条出口上都只给掩码。
func TestSecretNeverLeaks(t *testing.T) {
	const plain = "0xdeadbeefcafebabe"
	s := Secret(plain)
	order := &PayOrder{
		OrderNo: "O1", Amount: 1,
		To:   &Party{Kind: PartyWallet, Address: "TAddr"},
		From: &Party{Kind: PartyWallet, Address: "TFrom", Contract: "TContract", PrivateKey: s},
	}

	for _, got := range []string{
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", order),
		fmt.Sprintf("%+v", order),
		fmt.Sprintf("%#v", order),
	} {
		if strings.Contains(got, plain) {
			t.Fatalf("私钥明文进了格式化输出: %s", got)
		}
	}

	b, err := json.Marshal(order)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), plain) {
		t.Fatalf("私钥明文进了 JSON: %s", b)
	}
	if strings.Contains(string(b), "private_key") {
		t.Fatalf("私钥字段不该出现在 JSON 里: %s", b)
	}

	// 直接序列化 Secret 也不能泄露。
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatalf("Secret 直接序列化泄露: %s", raw)
	}

	if s.Reveal() != plain {
		t.Fatal("Reveal 应返回明文，供签名/派生地址使用")
	}
}

// TestPartyKeepsPerOrderContract 合约是可序列化的订单事实（不是密钥），
// 空值仍按 omitempty 消失。它进 JSON 才能让回执与审计看出这一单发的是哪张合约。
func TestPartyKeepsPerOrderContract(t *testing.T) {
	b, err := json.Marshal(&Party{Kind: PartyWallet, Address: "TAddr", Contract: "TContract"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"contract":"TContract"`) {
		t.Fatalf("合约未进 JSON: %s", b)
	}
	empty, err := json.Marshal(&Party{Kind: PartyWallet, Address: "TAddr"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "contract") {
		t.Fatalf("空合约不该出现在 JSON: %s", empty)
	}
}

// 本文件钉的是「一单该交给哪条通道」的那张表。
//
// 它是 P5 新写的代码（取代 pay 的驱动注册表 Register/OpenDriver）：键是通道种类，
// 值是已装配的实现。资金安全上它承担一件事——同一单号在一处分派、一个实现执行，
// 不允许出现「两个实现都认这个 kind」的静默覆盖。

// fakeTransfer 记下收到的订单号，用来说明「这一单落到了哪个实现」。
type fakeTransfer struct {
	got []string
	err error
}

func (f *fakeTransfer) Withdraw(_ context.Context, o *PayOrder) (*PayResult, error) {
	if o == nil {
		f.got = append(f.got, "<nil>")
	} else {
		f.got = append(f.got, o.OrderNo)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &PayResult{OrderNo: o.OrderNo, Status: StatusPending}, nil
}

// fakeChannel 是完整统一调用面的假实现：四个方法各自留下调用痕迹。
type fakeChannel struct {
	fakeTransfer
	orderCalls   []string
	accountCalls int
	onchainCalls int
}

func (f *fakeChannel) QueryOrder(_ context.Context, orderNo string) (*PayResult, error) {
	f.orderCalls = append(f.orderCalls, orderNo)
	return &PayResult{OrderNo: orderNo, Status: StatusPending}, nil
}

func (f *fakeChannel) QueryAccount(_ context.Context, q *AccountQuery) (*AccountInfo, error) {
	f.accountCalls++
	return &AccountInfo{Status: AcctActive, Available: 1}, nil
}

func (f *fakeChannel) QueryOnchainAccount(_ context.Context, q *OnchainAccountQuery) (*AccountInfo, error) {
	f.onchainCalls++
	return &AccountInfo{Status: AcctActive, Available: 1}, nil
}

// TestBindRejectsEmptyAndDuplicate 空 kind、空实现、重复登记都必须拒。
//
// 重复登记不许覆盖：覆盖等于让「哪个实现在管这条通道」取决于装配顺序，
// 而两个实现同时活着时，先注册的那个可能已经在出款了。
func TestBindRejectsEmptyAndDuplicate(t *testing.T) {
	if err := Bind("", &fakeTransfer{}); err == nil {
		t.Error("空 Kind 应当被拒")
	}
	if err := Bind(KindTronUSDT, nil); err == nil {
		t.Error("nil 实现应当被拒")
	}
	if err := Bind(KindTronUSDT, &fakeTransfer{}); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	defer Unbind(KindTronUSDT)
	if err := Bind(KindTronUSDT, &fakeTransfer{}); err == nil {
		t.Error("重复登记应当报错，不得静默覆盖")
	} else if !strings.Contains(err.Error(), string(KindTronUSDT)) {
		t.Errorf("重复登记的错误文案应指名种类，实得 %v", err)
	}
}

// TestUnbindAllowsRestart 停掉再起必须能重新登记：Start/Stop 可重复调用，
// Unbind 不释放的话第二次启动会在 Bind 处撞死。
func TestUnbindAllowsRestart(t *testing.T) {
	if err := Bind(KindTON, &fakeTransfer{}); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if !Unbind(KindTON) {
		t.Fatal("Unbind 应报告确实解开了一个绑定")
	}
	if Unbind(KindTON) {
		t.Fatal("重复 Unbind 不应报告解开了东西")
	}
	if err := Bind(KindTON, &fakeTransfer{}); err != nil {
		t.Fatalf("解开后重新登记失败: %v", err)
	}
	defer Unbind(KindTON)
}

// TestTransferForUnboundListsBound 未装配时要给出明确错误，并列出当前装了哪些——
// 运维看到的是「少装了这条」而不是一句「没有实现」。
func TestTransferForUnboundListsBound(t *testing.T) {
	if _, err := TransferFor(KindBscUSDT); err == nil {
		t.Fatal("未装配的通道应当报错")
	} else if !strings.Contains(err.Error(), string(KindBscUSDT)) {
		t.Errorf("错误文案应指名缺的是哪条通道: %v", err)
	}

	impl := &fakeTransfer{}
	if err := Bind(KindSolUSDT, impl); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindSolUSDT)
	got, err := TransferFor(KindSolUSDT)
	if err != nil {
		t.Fatalf("已装配却查不到: %v", err)
	}
	if got != impl {
		t.Fatal("取回的不是登记的那个实现")
	}
	if _, err := TransferFor(KindTronUSDT); err == nil ||
		!strings.Contains(err.Error(), string(KindSolUSDT)) {
		t.Errorf("未装配时的错误应列出已装配的通道，实得 %v", err)
	}
}

// TestTransferToDispatchesToBoundImplementation 分派只能落到登记的同一个实现上。
func TestTransferToDispatchesToBoundImplementation(t *testing.T) {
	impl := &fakeTransfer{}
	if err := Bind(KindTronUSDT, impl); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindTronUSDT)

	res, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}})
	if err != nil {
		t.Fatalf("分派失败: %v", err)
	}
	if res == nil || res.OrderNo != "O1" {
		t.Fatalf("回执不符: %+v", res)
	}
	if len(impl.got) != 1 || impl.got[0] != "O1" {
		t.Fatalf("实现收到的单号不符: %v", impl.got)
	}

	// 实现侧的错误必须原样透出：分派层不得把「说不清」改写成成功或另一种失败。
	sentinel := errors.New("供应商拒绝")
	buggy := &fakeTransfer{err: sentinel}
	Unbind(KindTronUSDT)
	if err := Bind(KindTronUSDT, buggy); err != nil {
		t.Fatal(err)
	}
	if _, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O2", Amount: 1, To: &Party{Address: "TAddr"}}); !errors.Is(err, sentinel) {
		t.Fatalf("实现错误未被透出: %v", err)
	}

	// 未装配的通道在分派层就要拒，不能返回一个「已受理」。
	if _, err := TransferTo(context.Background(), KindBscUSDT, &PayOrder{OrderNo: "O3", Amount: 1, To: &Party{Address: "0x0"}}); err == nil {
		t.Fatal("未装配的通道不该分派成功")
	}
}

// TestBoundKindsIsSorted 已装配清单必须排序：它进启动日志，
// 顺序随 map 遍历漂移会让同一份配置每次日志都不一样，对账时无法比对。
func TestBoundKindsIsSorted(t *testing.T) {
	if got := BoundKinds(); len(got) != 0 {
		t.Fatalf("初始应无绑定，实得 %v", got)
	}
	for _, k := range []Kind{KindSolUSDT, KindTronUSDT, KindTON} {
		if err := Bind(k, &fakeTransfer{}); err != nil {
			t.Fatal(err)
		}
		defer Unbind(k)
	}
	got := BoundKinds()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("清单未排序: %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("清单项数不符: %v", got)
	}
}

// TestKindsCoverEveryDeclaredChannel 四值枚举与实际分派表必须一致：
// 新增一条链却忘了加进 Kinds()，启动日志与「已装配清单」就会同时漏报。
func TestKindsCoverEveryDeclaredChannel(t *testing.T) {
	want := map[Kind]bool{KindTronUSDT: true, KindTON: true, KindBscUSDT: true, KindSolUSDT: true}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("Kinds()=%v，期望四值", got)
	}
	seen := map[Kind]bool{}
	for _, k := range got {
		if seen[k] {
			t.Fatalf("Kinds() 有重复项: %v", got)
		}
		seen[k] = true
		if !want[k] {
			t.Fatalf("Kinds() 出现未登记的值 %q", k)
		}
	}
}

// TestChannelForKeepsLegacyTransferCompatible 只实现 Withdraw 的旧装配方仍可绑定、
// 仍可分派出款；但查询面必须明确报「未提供」，不能退化成空实现。
func TestChannelForKeepsLegacyTransferCompatible(t *testing.T) {
	legacy := &fakeTransfer{}
	if err := Bind(KindTronUSDT, legacy); err != nil {
		t.Fatalf("旧实现绑定失败: %v", err)
	}
	defer Unbind(KindTronUSDT)

	if _, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}}); err != nil {
		t.Fatalf("旧实现出款分派失败: %v", err)
	}
	if _, err := ChannelFor(KindTronUSDT); err == nil {
		t.Fatal("只实现 Withdraw 的通道不该被当成完整 Channel")
	} else if !strings.Contains(err.Error(), "统一查询面") {
		t.Fatalf("错误文案应指明缺查询面，实得 %v", err)
	}
}

// TestChannelForDispatchesEveryOperation 四个动作必须落到同一个已登记实现上：
// 统一接口的全部意义就是「按 Kind 找到同一条通道」，不允许查单/查余额各走一条。
func TestChannelForDispatchesEveryOperation(t *testing.T) {
	ch := &fakeChannel{}
	if err := Bind(KindBscUSDT, ch); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindBscUSDT)

	got, err := ChannelFor(KindBscUSDT)
	if err != nil {
		t.Fatalf("已装配却取不到统一接口: %v", err)
	}
	if got != ch {
		t.Fatal("ChannelFor 返回的不是登记的那个实现")
	}
	res, err := QueryOrderTo(context.Background(), KindBscUSDT, "O1")
	if err != nil || res == nil || res.OrderNo != "O1" {
		t.Fatalf("QueryOrderTo 分派失败: res=%+v err=%v", res, err)
	}
	if _, err := QueryAccountTo(context.Background(), KindBscUSDT, &AccountQuery{}); err != nil {
		t.Fatalf("QueryAccountTo 分派失败: %v", err)
	}
	if _, err := QueryOnchainAccountTo(context.Background(), KindBscUSDT, &OnchainAccountQuery{}); err != nil {
		t.Fatalf("QueryOnchainAccountTo 分派失败: %v", err)
	}
	if len(ch.orderCalls) != 1 || ch.orderCalls[0] != "O1" || ch.accountCalls != 1 || ch.onchainCalls != 1 {
		t.Fatalf("四个动作没有落到同一实现: order=%v account=%d onchain=%d",
			ch.orderCalls, ch.accountCalls, ch.onchainCalls)
	}

	if _, err := QueryOrderTo(context.Background(), KindTronUSDT, "O2"); err == nil {
		t.Fatal("未装配的通道查询不该成功")
	}
}
