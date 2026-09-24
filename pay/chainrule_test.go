package pay

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

// goodAddr 是 fakeRule 认得的唯一「合法地址」，测试用它区分两条规则各自的触发点。
const goodAddr = "GOODADDR"

// fakeRule 是测试用的收款端规则：地址只认一个值，备注一律拒绝。
type fakeRule struct{}

func (fakeRule) CheckAddress(o *PayOrder) error {
	// 空地址放行，与真规则一致：充值侧的收款端由供应商返回，非空要求只加在提现上。
	if a := strings.TrimSpace(o.Address); a != "" && a != goodAddr {
		return errors.New("地址不合法")
	}
	return nil
}

func (fakeRule) CheckMemo(o *PayOrder) error {
	if strings.TrimSpace(o.Memo) != "" {
		return errors.New("该链不支持备注")
	}
	return nil
}

const (
	// regCoinA/B 只给登记表用例，ruleCoin 只给拒单用例：注册表是进程级全局的，
	// 同一币种在两个用例里各注册一次会被第二个用例的重名拒绝挡住。
	regCoinA = "TESTCOIN-A"
	regCoinB = "TESTCOIN-B"
	ruleCoin = "TESTCOIN-RULE"
)

func TestChainRuleRegistry(t *testing.T) {
	if err := RegisterChainRule("  ", fakeRule{}); err == nil {
		t.Error("空币种应当被拒")
	}
	if err := RegisterChainRule(regCoinA, nil); err == nil {
		t.Error("nil 规则应当被拒")
	}
	if err := RegisterChainRule(regCoinA, fakeRule{}); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if err := RegisterChainRule("  "+strings.ToLower(regCoinA)+"  ", fakeRule{}); err == nil {
		t.Error("同一币种换大小写再来一次应当被拒——覆盖等于让 init 顺序决定生效者")
	}
	// 大小写与空白不影响查找：配置里写 testcoin-a 也要能命中标成 TESTCOIN-A 的规则。
	if _, ok := ChainRuleFor(strings.ToLower(regCoinA)); !ok {
		t.Error("按小写币种查不到规则")
	}
	if _, ok := ChainRuleFor("   "); ok {
		t.Error("空白币种不该命中任何规则")
	}
	if err := RegisterChainRule(regCoinB, fakeRule{}); err != nil {
		t.Fatal(err)
	}
	got := CurrenciesWithRules()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("已登记币种未排序: %v", got)
	}
	for _, want := range []string{regCoinA, regCoinB} {
		if !strings.Contains(strings.Join(got, ","), want) {
			t.Errorf("已登记币种缺 %s，实得 %v", want, got)
		}
	}
}

// TestChainRuleRejectsBeforeSending 规则拒掉的单必须一次请求都不发出。
// 没登记规则的币种继续只管非空：宿主没链接那条通道包时，pay 没有那条链的知识。
func TestChainRuleRejectsBeforeSending(t *testing.T) {
	if err := RegisterChainRule(ruleCoin, fakeRule{}); err != nil {
		t.Fatal(err)
	}
	r := newSDKServer(t)
	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "t", BaseURL: r.srv.URL, SecretKey: "s", Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		order *PayOrder
		want  string
	}{
		{"坏地址", &PayOrder{OrderNo: "O1", Amount: 10, Currency: ruleCoin, Address: "x"}, "地址不合法"},
		{"带备注", &PayOrder{OrderNo: "O2", Amount: 10, Currency: ruleCoin, Address: goodAddr, Memo: "m"}, "该链不支持备注"},
	}
	for _, cs := range cases {
		if _, err := ch.Withdraw(context.Background(), cs.order); err == nil ||
			!strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 未被规则拦下，实得 %v", cs.name, err)
		}
	}
	if n := r.calls.Load(); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
	// 未登记规则的币种不受格式校验影响（地址非空那道由 CheckOrder 负责）。
	if _, err := ch.Withdraw(context.Background(), &PayOrder{
		OrderNo: "O4", Amount: 10, Currency: "NO_RULE_COIN", Address: "whatever",
	}); err != nil {
		t.Fatalf("未登记规则的币种被过度校验: %v", err)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("请求次数=%d，期望 1", n)
	}
	// 提现缺地址时先被 CheckOrder 挡下，规则连跑都不该跑。
	if _, err := ch.Withdraw(context.Background(), &PayOrder{OrderNo: "O5", Amount: 10, Currency: ruleCoin}); err == nil ||
		strings.Contains(err.Error(), "地址不合法") {
		t.Errorf("缺地址应报「收款地址为空」而不是格式错，实得 %v", err)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("请求次数=%d，期望仍是 1", n)
	}
	// 充值侧不带收款地址是合法形态：格式规则只管「有值时值对不对」。
	if _, err := ch.Recharge(context.Background(), &PayOrder{OrderNo: "O6", Amount: 10, Currency: ruleCoin}); err != nil {
		t.Fatalf("充值侧空地址被规则误伤: %v", err)
	}
	if n := r.calls.Load(); n != 2 {
		t.Fatalf("请求次数=%d，期望 2", n)
	}
}

// TestSignDomainCoversChainAndCallbackFields 网络标识、回调地址与备注都必须在签名域里。
//
// 这三个字段各管一件不能出错的事：network 防跨链误投、notify_url 决定回执送给谁、
// memo 决定交易所归集账户认不认得出这一笔。它们此前只进报文不进签名，
// 中间改一个字节而签名照旧，等于把钱送给改出来的人。
func TestSignDomainCoversChainAndCallbackFields(t *testing.T) {
	r := newSDKServer(t)
	r.resp.Store(`{"code":"0","data":{"order_no":"O1","status":"success","amount":1000}}`)
	const callback = "https://cb.example/notify"
	cases := []struct {
		name    string
		network string
		address string
		memo    string
		notify  string
	}{
		{"基线", NetworkMainnet, "TAddr", "", callback},
		{"换网络", NetworkShasta, "TAddr", "", callback},
		{"换地址", NetworkMainnet, "TOther", "", callback},
		{"加备注", NetworkMainnet, "TAddr", "8890", callback},
		{"换回调", NetworkMainnet, "TAddr", "", "https://evil.example/x"},
	}
	var seen []string
	for _, cs := range cases {
		ch, err := Open(DriverGeneric, ProviderOptions{
			Name: "t", BaseURL: r.srv.URL, SecretKey: "TOPSECRET", AppID: "app1",
			MerchantID: "MCH-1", NotifyURL: cs.notify, Endpoint: endpoints(), Timeout: 2 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ch.Withdraw(context.Background(), &PayOrder{
			OrderNo: "O1", Amount: 1000, Currency: CurrencyUSDT,
			Network: cs.network, Address: cs.address, Memo: cs.memo,
		}); err != nil {
			t.Fatalf("%s: %v", cs.name, err)
		}
		want := SignMD5("TOPSECRET",
			"app1", "MCH-1", "O1", "1000", CurrencyUSDT,
			cs.network, cs.address, "", cs.memo, cs.notify)
		if got := r.signs.Load().(string); got != want {
			t.Errorf("%s: 签名=%s，按十个定长标量算出的期望=%s", cs.name, got, want)
		}
		seen = append(seen, r.signs.Load().(string))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[0] {
			t.Errorf("%s 的签名与基线相同，说明该字段没进签名域", cases[i].name)
		}
	}
}

// TestOrderRequestSignValuesAreFixedPosition 钉住签名域的长度与每一位的落点。
//
// SignMD5 是无分隔符直连拼接，位置就是签名域的全部结构：字段增减、顺序调换、
// 改成按「有值才签」动态追加，都会让后面每一位偏移，而本地永远算得过。
func TestOrderRequestSignValuesAreFixedPosition(t *testing.T) {
	req := OrderRequest{
		AppID: "app1", MerchantID: "MCH-1", OrderNo: "O1", Amount: 1000,
		Currency: CurrencyUSDT, Network: NetworkMainnet, Address: "TAddr",
		Phone: "138", Memo: "8890", NotifyURL: "https://cb",
	}
	want := []string{"app1", "MCH-1", "O1", "1000", CurrencyUSDT, NetworkMainnet, "TAddr", "138", "8890", "https://cb"}
	got := req.SignValues()
	if len(got) != len(want) {
		t.Fatalf("签名域长度=%d，期望 %d（十个定长标量）", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 位=%q，期望 %q", i+1, got[i], want[i])
		}
	}
	// 空值同样占位：缺省报文的签名域也必须是十位，金额那位的缺省是 "0"。
	var zero OrderRequest
	empty := zero.SignValues()
	if len(empty) != 10 {
		t.Fatalf("空报文的签名域长度=%d，期望 10", len(empty))
	}
	for i, v := range empty {
		if i == 3 {
			if v != "0" {
				t.Errorf("金额位缺省应为 \"0\"，实得 %q", v)
			}
			continue
		}
		if v != "" {
			t.Errorf("第 %d 位应为空串占位，实得 %q", i+1, v)
		}
	}
}

// TestPayloadKeyNamesTrackOrderRequest 保留字名单由反射取自 OrderRequest，
// 这里钉住它确实覆盖了十个已签名量，且不含 Extra 自身。
func TestPayloadKeyNamesTrackOrderRequest(t *testing.T) {
	for _, name := range []string{
		"app_id", "merchant_id", "order_no", "amount", "currency",
		"network", "address", "phone", "memo", "notify_url",
	} {
		if _, ok := payloadKeyNames[name]; !ok {
			t.Errorf("保留字名单缺 %s，Extra 就能覆盖这个已签名量", name)
		}
	}
	if _, ok := payloadKeyNames["extra"]; ok {
		t.Error(`Extra 是摊平进报文的，json:"-" 不该被算成保留字`)
	}
}

// TestOrderExtraCannotClobberSignedFields 单内特有字段撞上已签名量必须拒单，
// 而且是在发出请求之前拒。
func TestOrderExtraCannotClobberSignedFields(t *testing.T) {
	for _, key := range []string{"amount", "address", "memo", "network", "notify_url"} {
		if _, _, err := mergeExtras(nil, map[string]string{key: "1"}); err == nil ||
			!strings.Contains(err.Error(), key) {
			t.Errorf("Extra 键 %s 未被挡下，实得 %v", key, err)
		}
	}
	// 追加顺序：通道特有字段按登记顺序在前，单内字段按 key 字典序在后。
	// 字典序不是洁癖——Go 的 map 遍历是随机化的，随签名域一起随机就会让同一单算出两个签名。
	if _, signs, err := mergeExtras(
		[]ExtraField{{Name: "contract", Value: "c"}},
		map[string]string{"ticket": "t", "operator": "o"},
	); err != nil {
		t.Fatal(err)
	} else if len(signs) != 3 || signs[0] != "c" || signs[1] != "o" || signs[2] != "t" {
		t.Fatalf("追加顺序应为「通道字段登记顺序 → 单内字段字典序」，实得 %v", signs)
	}
	r := newSDKServer(t)
	ch, err := Open(DriverGeneric, ProviderOptions{
		Name: "t", BaseURL: r.srv.URL, SecretKey: "s", Endpoint: endpoints(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Withdraw(context.Background(), &PayOrder{
		OrderNo: "O1", Amount: 1000, Address: "TAddr", Extra: map[string]string{"amount": "1"},
	}); err == nil {
		t.Fatal("覆盖金额的 Extra 应当被拒")
	}
	if n := r.calls.Load(); n != 0 {
		t.Fatalf("被拒的单发出了 %d 次请求，期望 0", n)
	}
	// 合法的特有字段既进报文也进签名域，两侧同一次产出。
	if _, err := ch.Withdraw(context.Background(), &PayOrder{
		OrderNo: "O2", Amount: 1000, Address: "TAddr", Extra: map[string]string{"remark": "r1"},
	}); err != nil {
		t.Fatal(err)
	}
	if body := r.bodies.Load().(string); !strings.Contains(body, `"remark":"r1"`) {
		t.Fatalf("特有字段没进报文: %s", body)
	}
	want := SignMD5("s", "", "", "O2", "1000", "", "", "TAddr", "", "", "", "r1")
	if got := r.signs.Load().(string); got != want {
		t.Fatalf("签名=%s，期望 %s（标量之后追加单内字段）", got, want)
	}
}

// TestResultCarriesChainReceipt 回执要能表达「已提交但还没落地」：哈希、单笔 gas、
// 确认数三者各自区分「没回」与「回了一个零值」。
func TestResultCarriesChainReceipt(t *testing.T) {
	p, err := NewProvider(ProviderOptions{Name: "t", BaseURL: "https://x.example", SecretKey: "s", Endpoint: endpoints()})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(OrderData{
		OrderNo: "O1", Status: "success", TxHash: "0xabc", Fee: 3200, FeeCurrency: CurrencyTRX,
		Confirmations: ptr64(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Result(data, "O1")
	if res.TxHash != "0xabc" || res.Fee != 3200 || res.FeeCurrency != CurrencyTRX {
		t.Fatalf("链上回执字段未透出: %+v", res)
	}
	if res.Confirmations == nil || *res.Confirmations != 0 {
		t.Fatalf("确认数 0 被当成没回: %+v", res.Confirmations)
	}
	// 供应商没回这些字段时保持零值与 nil，不编出「0 确认、0 手续费」的假事实。
	plain := p.Result([]byte(`{"order_no":"O1","status":"pending"}`), "O1")
	if plain.Confirmations != nil || plain.Fee != 0 || plain.TxHash != "" {
		t.Fatalf("缺字段被填成了有值: %+v", plain)
	}
	if got := p.Result([]byte(`{"order_no":"O9","status":"success","tx_hash":"h"}`), "O1"); got.OrderNo != "O9" {
		t.Errorf("回执里的订单号应优先于入参，实得 %s", got.OrderNo)
	}
}

func ptr64(v int64) *int64 { return &v }
