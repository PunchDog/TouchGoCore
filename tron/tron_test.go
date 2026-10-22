package tron

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"touchgocore/pay"
	"touchgocore/util"
)

// TRON 地址夹具：官方 USDT-TRC20 合约（版本字节 0x41、校验和已核），
// 以及用 20 字节账户哈希现算校验和造出的几个。造向量而不是抄网上的：
// 抄来的地址一旦本身是编的，测试就会把「校验和不过」永远藏住。
const (
	tronOfficialUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	tronAllZeroAcct  = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"
	tronRangeAcct    = "T9yED5xMV5ARV98BexN97aLZ1UUq7eKSxm"
	tronFFAcct       = "TZJozAg1ruapycCicgz31GxvYJ1FraLjZa"
	// tronBogusChecksum 长度与版本字节全对，只有末尾 4 字节校验和不过。
	tronBogusChecksum = "TR7NharAcTPb3DHchqPguF36bXqWRRCbqR"
)

// TestIsValidTRONAddress 地址校验的正负用例：合法向量全过，
// 长度、版本字节、字母表、校验和任一不合都当场拒。
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
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6O", false, "含非法字符 O"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6l", false, "含非法字符 l"},
		{"0x76d5820a24a2089091721d40a4d20680804b6229", false, "EVM 地址不是 TRON 地址"},
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

// TestTRXRuleLivesInThisPackage 地址规则归属于链包，且不再经 pay 的注册表：
// checkOrderAddress 的调用点就是接线，漏接是编译错误而不是「未登记即放行」。
func TestTRXRuleLivesInThisPackage(t *testing.T) {
	rule := TRONRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronRangeAcct}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tronBogusChecksum}}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestTRXRuleRejectsMemo TRON 原生转账没有 memo 概念：填了就拒而不是忽略，
// 上游把 TON 的认款机制套到这条链上是错配，静默吞掉等于放它去别处发作。
func TestTRXRuleRejectsMemo(t *testing.T) {
	rule := TRONRule{}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"123", " 12 ", "复转到交易所"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
	// 纯空白按「没有备注」处理：真正不发出去的那一步在 NewOrderRequest 的 trim。
	if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
}

// TestPayoutInterfaceStaysWired TRX 侧门面仍是包外可见调用面，且一律返回未接入哨兵。
//
// 与 usdt 侧的存续闸同型：签名不许被顺手改掉（下游按它编译），
// 而「没接入」绝不允许被读成「已受理」——结果必须是 nil 加 errors.Is 可判的哨兵。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = TronWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = TronQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = TronAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: tronAllZeroAcct}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := TronWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := TronQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := TronAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 单号为空也一样先拒：不能把参数错误伪装成一次「查询未果」的 UNKNOWN。
	if _, err := TronQuery(nil, ""); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空单号的拒绝口径不符: %v", err)
	}
}

// TestPayoutNotWiredDoesNotBroadcast 未接入的门面不得广播任何回执。
//
// 广播键（CallTronMsg+Xxx）在下游挂着落库回调，误广播一次就给账本记一笔不存在的动作。
func TestPayoutNotWiredDoesNotBroadcast(t *testing.T) {
	var (
		mu   sync.Mutex
		got  []*pay.PayResult
		kind []string
	)
	for _, k := range []string{"Withdraw", "Query"} {
		id := util.DefaultCallFunc.Register(util.CallTronMsg+k, func(res *pay.PayResult) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, res)
			kind = append(kind, k)
		})
		defer util.DefaultCallFunc.Unregister(util.CallTronMsg+k, id)
	}
	o := &pay.PayOrder{OrderNo: "O9", Amount: 1, To: &pay.Party{Address: tronAllZeroAcct}}
	_, _ = TronWithdraw(nil, o)
	_, _ = TronQuery(nil, "O9")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("未接入的链路广播了回执: kinds=%v %+v", kind, got)
	}
}
