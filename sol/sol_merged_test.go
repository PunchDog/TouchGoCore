package sol

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/rpc"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件钉「收款地址与备注」这道闸：它既在包内函数上直接断言，
// 也从 Withdraw 的真入口再走一遍——只测函数的话，把调用点删掉测试仍然全绿。

// TestCheckOrderAddressAcceptsWallet 合法地址放行。
func TestCheckOrderAddressAcceptsWallet(t *testing.T) {
	for _, addr := range []string{solBytes1To32, solAllFFAcct, solPatternAcct, "  " + solBytes1To32 + "  "} {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: addr}}); err != nil {
			t.Errorf("合法地址 %q 被拒: %v", addr, err)
		}
	}
}

// TestCheckOrderAddressRejectsBadAddress 长度不足、含字母表外字符、跨链地址一律拒。
func TestCheckOrderAddressRejectsBadAddress(t *testing.T) {
	for _, cs := range []struct {
		name string
		addr string
	}{
		{"少一位", solBytes1To32[:42]},
		{"多一位", solAllFFAcct + "1"},
		{"含 0", "0wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"},
		{"EVM 地址", "0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A"},
		{"TRON 地址", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
	} {
		err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: cs.addr}})
		if err == nil {
			t.Errorf("%s: 非法地址未被拦下", cs.name)
			continue
		}
		if !strings.Contains(err.Error(), "不合法") {
			t.Errorf("%s: 错误文案未点明地址不合法: %v", cs.name, err)
		}
	}
}

// TestCheckOrderAddressRejectsMemo Solana 没有 TON comment 那种认款机制：填了就拒。
func TestCheckOrderAddressRejectsMemo(t *testing.T) {
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Memo: "   "}}); err != nil {
		t.Errorf("纯空白备注被当成有值: %v", err)
	}
	for _, memo := range []string{"123", "复转到交易所", " " + solBytes1To32} {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32, Memo: memo}}); err == nil {
			t.Errorf("备注 %q 未被拦下", memo)
		}
	}
}

// TestCheckOrderAddressToleratesEmptyOrder 空订单/空收款方交给 pay.CheckOrder 拒，
// 这里不越位重复判：两处判同一件事，漂移的那一次通常是「新增一类收款方只改了一处」。
func TestCheckOrderAddressToleratesEmptyOrder(t *testing.T) {
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单在地址规则上不应报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("缺 To 在地址规则上不应报错: %v", err)
	}
}

// TestWithdrawRunsAddressGateBeforeRPC 从真入口证明这道闸在出款路径上。
//
// 通道的 rpc 字段留 nil：如果地址闸没拦住，下一步就会解引用它并 panic。
// 于是「返回地址错误而不是 panic」同时证明了两件事——闸在路径上，且它排在动钱之前。
func TestWithdrawRunsAddressGateBeforeRPC(t *testing.T) {
	c := solTestChannel(t)
	ctx := context.Background()

	bad := &pay.PayOrder{
		OrderNo:  "SOL-GATE-1",
		Amount:   1_000_000,
		Currency: pay.CurrencyUSDT,
		To:       &pay.Party{Kind: pay.PartyWallet, Address: solBytes1To32[:42]},
	}
	_, err := c.Withdraw(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法地址应被出款前置拒掉，实得 %v", err)
	}

	withMemo := *bad
	withMemo.To = &pay.Party{Kind: pay.PartyWallet, Address: solBytes1To32, Memo: "123"}
	_, err = c.Withdraw(ctx, &withMemo)
	if err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("带备注的订单应被出款前置拒掉，实得 %v", err)
	}
}

// 本文件钉出款机器里那些「不连节点也能判」的闸：装配期拒单、金额/小数位/网络、
// 出款方比对、手续费上限、余额换算。连节点才有的行为（预检、广播、查证）
// 由 query_test.go 的纯函数判据与真实链测试各自负责，这里不假装覆盖。

// TestNewSolOnchainChannelRejectsIncomplete 装配期只要缺一项就拒绝启动。
//
// 每一条都对应一种「带着未知上线」的形态：不知道发哪张 mint、不知道小数位、
// 不知道用哪个钱包付钱。它们的共同点是——第一笔出款才会暴露，而那时钱已经动了。
func TestNewSolOnchainChannelRejectsIncomplete(t *testing.T) {
	key := solTestKey(t)
	wallet := key.PublicKey().String()
	cases := []struct {
		name string
		opt  channelOptions
		want string
	}{
		{
			name: "缺 chain 段",
			opt:  channelOptions{Name: "usdt", Mint: solBytes1To32, Decimals: 6},
			want: "chain",
		},
		{
			name: "mint 为空",
			opt:  channelOptions{Name: "usdt", Decimals: 6, Chain: &chainConfig{WalletAddress: wallet}},
			want: "mint",
		},
		{
			name: "mint 非法",
			opt:  channelOptions{Name: "usdt", Mint: "not-an-address", Decimals: 6, Chain: &chainConfig{WalletAddress: wallet}},
			want: "不合法",
		},
		{
			name: "钱包地址为空",
			opt:  channelOptions{Name: "usdt", Mint: solBytes1To32, Decimals: 6, Chain: &chainConfig{}},
			want: "wallet_address",
		},
		{
			name: "小数位不是 6",
			opt:  channelOptions{Name: "usdt", Mint: solBytes1To32, Decimals: 18, Chain: &chainConfig{WalletAddress: wallet}},
			want: "decimals",
		},
	}
	for _, cs := range cases {
		ch, err := newSolOnchainChannel(cs.opt)
		if err == nil {
			t.Errorf("%s: 装配成功了（应当拒绝）", cs.name)
			if ch != nil {
				_ = ch.Close()
			}
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 错误文案未提到 %q: %v", cs.name, cs.want, err)
		}
	}
}

// TestFeeCeiling 手续费上限必须与「预检余额」和「台账 FeeLimit」用同一个数。
// 优先费单价是 micro-lamports/CU，换算到 lamports 要除回 1e6——这组用例把这条
// 换算钉死：曾经漏掉除数会让上限虚高 10^6 倍，配了优先费的出款几乎必被预检误拒。
func TestFeeCeiling(t *testing.T) {
	c := solTestChannel(t)
	c.chain.PriorityFeeMicroLamports = 0
	if got := c.feeCeiling(); got != signatureFeeLamports {
		t.Errorf("不配优先费时上限=%d，期望 %d", got, signatureFeeLamports)
	}
	// 5 micro-lamports/CU × 200000 CU = 1e6 micro-lamports = 1 lamport。
	c.chain.PriorityFeeMicroLamports = 5
	want := int64(signatureFeeLamports + 1)
	if got := c.feeCeiling(); got != want {
		t.Errorf("优先费=5 时上限=%d，期望 %d（换算漏除 1e6 会虚高）", got, want)
	}
	// 1 lamport/CU（=1e6 micro-lamports/CU）× 200000 CU = 200000 lamports。
	c.chain.PriorityFeeMicroLamports = 1_000_000
	want = int64(signatureFeeLamports + computeUnitLimit)
	if got := c.feeCeiling(); got != want {
		t.Errorf("优先费=1e6 时上限=%d，期望 %d", got, want)
	}
	c.chain.PriorityFeeMicroLamports = math.MaxUint64
	if got := c.feeCeiling(); got != math.MaxInt64 {
		t.Errorf("优先费溢出时必须饱和到 MaxInt64，实得 %d", got)
	}
}

// TestCheckDecimals 填了就必须等于配置；留 0 表示不声明。
func TestCheckDecimals(t *testing.T) {
	c := solTestChannel(t)
	if err := c.checkDecimals(&pay.PayOrder{Decimals: 0}); err != nil {
		t.Errorf("未声明小数位被拒: %v", err)
	}
	if err := c.checkDecimals(&pay.PayOrder{Decimals: 6}); err != nil {
		t.Errorf("小数位一致被拒: %v", err)
	}
	err := c.checkDecimals(&pay.PayOrder{Decimals: 18})
	if err == nil || !strings.Contains(err.Error(), "10^n") {
		t.Errorf("小数位不一致未被拦下或文案未点明量级风险: %v", err)
	}
}

// TestCheckAmount 进签名机之前，金额必须为正。
func TestCheckAmount(t *testing.T) {
	c := solTestChannel(t)
	for _, amount := range []int64{0, -1, math.MinInt64} {
		if err := c.checkAmount(&pay.PayOrder{Amount: amount}); err == nil {
			t.Errorf("金额 %d 未被拦下", amount)
		}
	}
	if err := c.checkAmount(&pay.PayOrder{Amount: 1}); err != nil {
		t.Errorf("正金额被拒: %v", err)
	}
	if err := c.checkAmount(nil); err == nil {
		t.Error("nil 订单未被拦下")
	}
}

// TestCheckAsset 网络与币种都不能被订单改掉：换网就是换币。
func TestCheckAsset(t *testing.T) {
	c := solTestChannel(t)
	cases := []struct {
		name string
		net  string
		cur  string
		ok   bool
	}{
		{"都不填按通道配置", "", "", true},
		{"网络一致", "mainnet-beta", pay.CurrencyUSDT, true},
		{"网络大小写不敏感", "MAINNET-BETA", "usdt", true},
		{"网络不一致", "devnet", "", false},
		{"币种不一致", "", pay.CurrencyTRX, false},
	}
	for _, cs := range cases {
		err := c.checkAsset(&pay.PayOrder{Currency: cs.cur, To: &pay.Party{Network: cs.net}})
		if cs.ok && err != nil {
			t.Errorf("%s: 被拒: %v", cs.name, err)
		}
		if !cs.ok && err == nil {
			t.Errorf("%s: 未被拦下", cs.name)
		}
	}
}

// TestAssetOfPinsHotWallet 出款方默认是配置热钱包；按单私钥必须与 From.Address 同一账户。
func TestAssetOfPinsHotWallet(t *testing.T) {
	c := solTestChannel(t)
	if _, err := c.assetOf(&pay.PayOrder{}); err != nil {
		t.Errorf("缺 From 应表示「用配置默认出款方」: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: "  "}}); err != nil {
		t.Errorf("空 From.Address 应表示「用配置默认出款方」: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: c.sign.Address().String()}}); err != nil {
		t.Errorf("等于热钱包的 From 被拒: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: "not-an-address"}}); err == nil {
		t.Error("非法 From 地址未被拦下")
	}
	_, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: solBytes1To32}})
	if err == nil {
		t.Fatal("指定了非热钱包地址的 From 未被拦下")
	}
	if !strings.Contains(err.Error(), c.sign.Address().String()) {
		t.Errorf("错误文案未点明实际热钱包地址: %v", err)
	}

	// 按单私钥：派生地址与 From.Address 一致才放行；留空地址则用派生地址。
	keyRaw, addr := qaSOLPerOrderKey(t)
	a, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: addr.String(), PrivateKey: pay.Secret(keyRaw)}})
	if err != nil {
		t.Fatalf("按单私钥被拒: %v", err)
	}
	if !a.wallet.Equals(addr) || a.sign == nil || a.secret != keyRaw {
		t.Fatalf("按单资产解析错: %+v", a)
	}
	a, err = c.assetOf(&pay.PayOrder{From: &pay.Party{PrivateKey: pay.Secret(keyRaw)}})
	if err != nil || !a.wallet.Equals(addr) {
		t.Fatalf("按单私钥留空地址应回落到派生地址: asset=%+v err=%v", a, err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: c.sign.Address().String(), PrivateKey: pay.Secret(keyRaw)}}); err == nil {
		t.Fatal("私钥与 From.Address 不匹配未被拒")
	} else if !strings.Contains(err.Error(), "不是同一个账户") {
		t.Errorf("不匹配拒因文案不符: %v", err)
	}

	// mint 与网络仍钉配置：本轮不放开按单换币。
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Contract: solPatternAcct}}); err == nil {
		t.Error("按单换 mint 未被拒")
	} else if !strings.Contains(err.Error(), "不得按单换币") {
		t.Errorf("换币拒因文案不符: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Network: "devnet"}}); err == nil {
		t.Error("按单换网未被拒")
	} else if !strings.Contains(err.Error(), "配的是 mainnet-beta 网络") {
		t.Errorf("换网拒因文案不符: %v", err)
	}
}

// qaSOLPerOrderKey 造一把与配置热钱包不同的 ed25519 私钥（确定性 seed，便于复现）。
func qaSOLPerOrderKey(t *testing.T) (string, solana.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(200 + i)
	}
	key := solana.PrivateKey(ed25519.NewKeyFromSeed(seed))
	return key.String(), key.PublicKey()
}

// TestSameRecipient 同一单号二次提交时按解码后的字节比收端。
func TestSameRecipient(t *testing.T) {
	pk, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	if !sameRecipient("", pk) {
		t.Error("台账未记收端时应视为一致")
	}
	if !sameRecipient("  "+pk.String()+"  ", pk) {
		t.Error("台账里的收端带空白且与订单相同，应视为一致")
	}
	if sameRecipient(solPatternAcct, pk) {
		t.Error("不同收端被读成同一地址")
	}
	if sameRecipient("garbage-not-an-address", pk) {
		t.Error("台账里的收端不可解析时被读成了同一地址")
	}
}

// TestParseTokenAmount 余额按字符串解析，不走浮点。
func TestParseTokenAmount(t *testing.T) {
	if v, err := parseTokenAmount(" 123456 "); err != nil || v != 123456 {
		t.Errorf("正常余额解析=%d err=%v", v, err)
	}
	if v, err := parseTokenAmount("0"); err != nil || v != 0 {
		t.Errorf("零余额解析=%d err=%v", v, err)
	}
	if _, err := parseTokenAmount(""); err == nil {
		t.Error("空串未被拦下")
	}
	if _, err := parseTokenAmount("1.5"); err == nil {
		t.Error("带小数点的最小单位字符串未被拦下")
	}
	if _, err := parseTokenAmount("18446744073709551616"); err == nil {
		t.Error("超出 u64 的余额未被拦下")
	}
}

// TestSaturateUint64 超出 int64 的 u64 余额饱和而不是回绕成负数。
func TestSaturateUint64(t *testing.T) {
	if got := saturateUint64(0); got != 0 {
		t.Errorf("0 饱和后=%d", got)
	}
	if got := saturateUint64(math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("MaxInt64 饱和后=%d", got)
	}
	if got := saturateUint64(math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("MaxUint64 饱和后=%d，期望 %d（绝不能回绕成负数）", got, math.MaxInt64)
	}
}

// TestQueryAccountRejectsForeignCurrency 查账户的第一个动作是币种判定，
// 不是读链：非 USDT 的查询必须在碰 RPC 之前就拒掉（这里 rpc 为 nil 即证明）。
func TestQueryAccountRejectsForeignCurrency(t *testing.T) {
	c := solTestChannel(t)
	if _, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: pay.CurrencyTRX}); err == nil {
		t.Error("非 USDT 币种的账户查询未被拦下")
	}
}

// TestWithdrawRejectsBankTarget 银行代付不该走到地址解析：先给「走错了通道」。
//
// 与地址闸同一条理由：rpc 为 nil 时若这一步放行，下一步就会 panic。
func TestWithdrawRejectsBankTarget(t *testing.T) {
	c := solTestChannel(t)
	o := &pay.PayOrder{
		OrderNo: "SOL-BANK-1",
		Amount:  1_000_000,
		To: &pay.Party{
			Kind: pay.PartyBank,
			Bank: &pay.BankAccount{AccountNo: "6222020000000000000", Holder: "张三"},
		},
	}
	_, err := c.Withdraw(context.Background(), o)
	if err == nil {
		t.Fatal("银行收款方未被拦下")
	}
	if !strings.Contains(err.Error(), "银行") && !strings.Contains(err.Error(), "bank") {
		t.Errorf("错误文案未点明银行通道不支持: %v", err)
	}
}

// TestSolTestChannelFixtures 把纯函数夹具的缺省值钉住。
//
// 这些用例的闸都要求「不连节点也能判」，因此夹具本身不能带接入点参数：
// 一旦将来有人给夹具补上超时或重试，那说明有闸开始依赖 RPC 了。
func TestSolTestChannelFixtures(t *testing.T) {
	c := solTestChannel(t)
	if c.nowFn == nil || c.nowFn().IsZero() {
		t.Error("测试通道缺时间源")
	}
	if c.ledger == nil {
		t.Error("测试通道缺台账")
	}
	if c.opt.Decimals != defaultSOLDecimals {
		t.Errorf("测试通道小数位=%d，期望 %d", c.opt.Decimals, defaultSOLDecimals)
	}
	if c.chain.Timeout != 0 || c.chain.MaxRetry != 0 {
		t.Errorf("测试通道不应带接入点参数: timeout=%v retry=%d", c.chain.Timeout, c.chain.MaxRetry)
	}
}

// TestReverseCandidateNeverSettles F2：反查命中同址同额候选时只禁止广播，
// 候选不落账、不查证，结果必须是 UNKNOWN + 候选 txid。
func TestReverseCandidateNeverSettles(t *testing.T) {
	c := solTestChannel(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	o := &pay.PayOrder{
		OrderNo: "SOL-F2", Amount: 600, Currency: pay.CurrencyUSDT,
		To: &pay.Party{Address: to.String()},
	}
	txid := solana.Signature{1, 2, 3}.String()
	c.reverseLookup = func(context.Context, *pay.PayOrder, orderAsset, time.Time) (*ledger.Entry, error) {
		return &ledger.Entry{
			OrderNo: o.OrderNo, TxID: txid, From: c.sign.Address().String(),
			To: to.String(), Contract: c.opt.Mint, Amount: o.Amount,
		}, nil
	}
	res, err := c.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatalf("候选路径不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash != txid {
		t.Fatalf("候选必须回 UNKNOWN 并带 txid: %+v", res)
	}
	if !strings.Contains(res.RawNote, "无法证明") {
		t.Fatalf("回执没有说明候选不能归属本单: %q", res.RawNote)
	}
	if _, found := c.ledger.Get(o.OrderNo); found {
		t.Fatal("反查候选被写进了台账，下一次 confirm 会结算成 SUCCESS")
	}
}

func TestWithdrawLockedPinsPerOrderWallet(t *testing.T) {
	c := solTestChannel(t)
	keyRaw, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	o := &pay.PayOrder{
		OrderNo: "SOL-PER-1", Amount: 600, Currency: pay.CurrencyUSDT,
		To:   &pay.Party{Address: to.String()},
		From: &pay.Party{Address: owner.String(), PrivateKey: pay.Secret(keyRaw)},
	}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "sig", From: c.sign.Address().String(),
		To: to.String(), Contract: c.opt.Mint, Amount: o.Amount, State: ledger.StateSettled,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同单号换钱包未被拒: %v", err)
	}
}

func TestWithdrawLockedPinsMint(t *testing.T) {
	c := solTestChannel(t)
	keyRaw, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	o := &pay.PayOrder{
		OrderNo: "SOL-PER-2", Amount: 600, Currency: pay.CurrencyUSDT,
		To:   &pay.Party{Address: to.String()},
		From: &pay.Party{Address: owner.String(), PrivateKey: pay.Secret(keyRaw)},
	}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	otherMint, err := solana.PublicKeyFromBase58(solPatternAcct)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "sig", From: owner.String(),
		To: to.String(), Contract: otherMint.String(), Amount: o.Amount, State: ledger.StateSettled,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同单号换 mint 未被拒: %v", err)
	}
}

func TestTokenDeltaUsesEntryOwnerAndMint(t *testing.T) {
	c := solTestChannel(t)
	_, owner := qaSOLPerOrderKey(t)
	entry := &ledger.Entry{From: owner.String(), Contract: c.opt.Mint}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "400")},
	}
	delta, err := c.tokenDelta(meta, entry)
	if err != nil {
		t.Fatalf("按单 owner 的净变化计算失败: %v", err)
	}
	if delta.String() != "-600" {
		t.Fatalf("净变化=%s，期望 -600", delta)
	}
	// 台账缺 From/Contract 时才回落配置热钱包；这里换成配置钱包后，上面那组
	// 按单 owner 的余额不能被算进净变化。
	legacyDelta, err := c.tokenDelta(meta, &ledger.Entry{})
	if err != nil {
		t.Fatalf("旧台账回落计算失败: %v", err)
	}
	if legacyDelta.Sign() != 0 {
		t.Fatalf("旧台账回落配置钱包时不应命中按单 owner 的余额，实得 %s", legacyDelta)
	}
}

func TestPaidToUsesEntryOwnerAndMint(t *testing.T) {
	c := solTestChannel(t)
	_, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	entry := &ledger.Entry{From: owner.String(), Contract: c.opt.Mint, To: to.String(), Amount: 600}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "400"), solTokenBal(2, c.mint, to, "600")},
	}
	if !c.paidTo(meta, entry) {
		t.Fatal("按单钱包的正常转出未被识别")
	}
	// 换成配置热钱包：同一份 meta 不能被认成这一单。
	entry.From = c.sign.Address().String()
	if c.paidTo(meta, entry) {
		t.Fatal("meta 里的 owner 与台账不是同一账户却被认成已付")
	}
}

// 本文件钉查单侧那几条「纯本地就能判」的判据：台账原文还原、余额净变化、
// 反查命中条件、失败终局、以及「说不清就给 UNKNOWN」。
//
// 连不上节点时，confirm 的「查不到 → blockhash 判死」与 judge 的确认数分支
// 无法在这里覆盖（它们需要节点回答）；但它们的判据本身是纯函数，
// 而那正是「钱动没动」这句话真正的依据。

// solTokenBal 构造一条 token 余额记录。Owner 是指针：链上返回里它可以缺失，
// 缺失的记录必须被排除而不是当成零地址。
func solTokenBal(accountIndex uint16, mint, owner solana.PublicKey, amount string) rpc.TokenBalance {
	o := owner
	return rpc.TokenBalance{
		AccountIndex:  accountIndex,
		Mint:          mint,
		Owner:         &o,
		UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: 6},
	}
}

// TestDecodeRawTx 台账里的已签名字节必须能还原成同一笔交易。
func TestDecodeRawTx(t *testing.T) {
	if _, err := decodeRawTx(nil); err == nil {
		t.Error("nil 条目未被拦下")
	}
	if _, err := decodeRawTx(&ledger.Entry{}); err == nil {
		t.Error("缺 RawTx 的条目未被拦下")
	}
	if _, err := decodeRawTx(&ledger.Entry{RawTx: []byte{1, 2, 3}}); err == nil {
		t.Error("不可解析的字节未被拦下")
	}

	sgn := solTestSigner(t)
	hash := solana.Hash{7}
	tx, err := solana.NewTransactionBuilder().
		SetRecentBlockHash(hash).
		SetFeePayer(sgn.Address()).
		AddInstruction(computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()).
		Build()
	if err != nil {
		t.Fatalf("组包失败: %v", err)
	}
	if err := sgn.Sign(tx); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := decodeRawTx(&ledger.Entry{TxID: txSignature(tx), RawTx: raw})
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if back.Message.RecentBlockhash != hash {
		t.Errorf("还原后 blockhash=%s，期望 %s", back.Message.RecentBlockhash, hash)
	}
	if len(back.Signatures) == 0 || !back.Signatures[0].Equals(tx.Signatures[0]) {
		t.Error("还原后签名与原文不一致")
	}
}

// TestTokenDelta 净变化只统计「本 mint + 热钱包持有」的那些账户。
func TestTokenDelta(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	mint := c.mint
	otherMint, err := solana.PublicKeyFromBase58(solPatternAcct) // 与 c.mint 不同的另一把公钥
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	otherOwner, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}

	cases := []struct {
		name string
		pre  []rpc.TokenBalance
		post []rpc.TokenBalance
		want string
	}{
		{
			name: "正常转出",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400")},
			want: "-600",
		},
		{
			name: "收款方 ATA 新建（只出现在 post）",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, mint, otherOwner, "600")},
			want: "0",
		},
		{
			name: "别的 mint 不算",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, otherMint, owner, "500")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400"), solTokenBal(2, otherMint, owner, "0")},
			want: "-600",
		},
		{
			name: "别人的账户不算",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, mint, otherOwner, "900")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400"), solTokenBal(2, mint, otherOwner, "1500")},
			want: "-600",
		},
		{
			name: "账户关闭（只出现在 pre）",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: nil,
			want: "-1000",
		},
	}
	for _, cs := range cases {
		delta, err := c.tokenDelta(&rpc.TransactionMeta{PreTokenBalances: cs.pre, PostTokenBalances: cs.post}, &ledger.Entry{})
		if err != nil {
			t.Errorf("%s: 计算失败: %v", cs.name, err)
			continue
		}
		if delta.String() != cs.want {
			t.Errorf("%s: 净变化=%s，期望 %s", cs.name, delta, cs.want)
		}
	}
}

// TestTokenDeltaRejectsBadAmount 读不出余额时不能当成 0：那会把「读失败」读成「没动钱」。
func TestTokenDeltaRejectsBadAmount(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	bad := solTokenBal(1, c.mint, owner, "not-a-number")
	if _, err := c.tokenDelta(&rpc.TransactionMeta{PreTokenBalances: []rpc.TokenBalance{bad}}, &ledger.Entry{}); err == nil {
		t.Error("不可解析的余额未被拦下")
	}
}

// TestPaidTo 反查命中必须同时满足「本钱包净减少本单金额」与「收端在交易后持有本 mint」。
func TestPaidTo(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	entry := &ledger.Entry{To: to.String(), Amount: 600}
	mk := func(pre, post []rpc.TokenBalance) *rpc.TransactionMeta {
		return &rpc.TransactionMeta{PreTokenBalances: pre, PostTokenBalances: post}
	}
	ours := func(amount string) rpc.TokenBalance { return solTokenBal(1, c.mint, owner, amount) }

	if !c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("400"), solTokenBal(2, c.mint, to, "600")}), entry) {
		t.Error("金额与收端都相符的交易未被识别为已付")
	}
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("400")}), entry) {
		t.Error("收端不在交易后余额表里却判成了已付")
	}
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("900"), solTokenBal(2, c.mint, to, "100")}), entry) {
		t.Error("金额不符却判成了已付")
	}
	if c.paidTo(nil, entry) || c.paidTo(mk(nil, nil), nil) {
		t.Error("nil meta / nil 条目应判为未命中")
	}
	// 台账里的收端不可解析时不能 panic，只能判未命中。
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")}, []rpc.TokenBalance{ours("400")}),
		&ledger.Entry{To: "garbage", Amount: 600}) {
		t.Error("收端不可解析时判成了已付")
	}
}

// TestJudgeFailedIsTerminal 进块但执行失败是终局：手续费照烧，钱没动，不重建。
func TestJudgeFailedIsTerminal(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-J1", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.judge(context.Background(), entry,
		&rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{Err: errors.New("custom program error: 0x1"), Fee: 5000}}, c.nowFn())
	if err != nil {
		t.Fatalf("判失败时不应报错: %v", err)
	}
	if res.Status != pay.StatusFailed {
		t.Errorf("状态=%s，期望 failed", res.Status)
	}
	if res.Fee != int64(signatureFeeLamports) || res.FeeCurrency != pay.CurrencySOL {
		t.Errorf("手续费留痕=%d %s，期望 %d SOL", res.Fee, res.FeeCurrency, signatureFeeLamports)
	}
	if res.RawNote == "" {
		t.Error("失败回执必须带可读原因")
	}
	got, ok := c.ledger.Get("SOL-J1")
	if !ok || !got.Terminal() {
		t.Errorf("失败结论未落成终局: ok=%v state=%v", ok, got)
	}
}

// TestJudgeMismatchedDeltaIsUnknown 无错但余额变化对不上：说不清，绝不给成功或失败。
func TestJudgeMismatchedDeltaIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	entry := &ledger.Entry{OrderNo: "SOL-J2", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
	}
	res, err := c.judge(context.Background(), entry, &rpc.GetTransactionResult{Meta: meta}, c.nowFn())
	if err != nil {
		t.Fatalf("判 UNKNOWN 时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
	if !strings.Contains(res.RawNote, "净变化") || !strings.Contains(res.RawNote, "本单应付") {
		t.Errorf("UNKNOWN 文案未点明判据与对不上的金额: %q", res.RawNote)
	}
}

// TestJudgeMissingMetaIsUnknown 落了块却没有 meta：读不出余额变化，只能 UNKNOWN。
func TestJudgeMissingMetaIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-J3", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.judge(context.Background(), entry, &rpc.GetTransactionResult{}, c.nowFn())
	if err != nil {
		t.Fatalf("判 UNKNOWN 时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
}

// TestConfirmBadSignatureIsUnknown 台账里的签名不可解析时给 UNKNOWN，不 panic 也不判死。
func TestConfirmBadSignatureIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-C1", TxID: "0OIl-not-base58", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.confirm(context.Background(), entry, c.nowFn())
	if err != nil {
		t.Fatalf("签名不可解析时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
	if res.TxHash != entry.TxID {
		t.Errorf("UNKNOWN 回执必须带上台账里的签名供对账: %q", res.TxHash)
	}
}

// TestQueryOrderWithoutEntry 台账里没有这一单号时给 UNKNOWN，不去链上瞎猜。
func TestQueryOrderWithoutEntry(t *testing.T) {
	c := solTestChannel(t)
	if _, err := c.QueryOrder(context.Background(), "   "); err == nil {
		t.Error("空单号未被拦下")
	}
	res, err := c.QueryOrder(context.Background(), "SOL-NOT-FOUND")
	if err != nil {
		t.Fatalf("查无此单时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown（把查不到说成失败会诱使上游重下单）", res.Status)
	}
}

// TestSettleRequiresWritableJournal 终局落不进台账时必须报错，而不是照样返回结论。
func TestSettleRequiresWritableJournal(t *testing.T) {
	c := solTestChannel(t)
	c.ledger.SetJournal(failingJournal{})
	entry := &ledger.Entry{OrderNo: "SOL-S1", TxID: "sig", Amount: 600}
	err := c.settle(entry, "链上终局")
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Errorf("台账写不进去时应报 ErrJournalUnavailable，实得 %v", err)
	}
}

// failingJournal 是一个永远写不进去的台账存储，用来验证「记不上就不下结论」。
type failingJournal struct{}

func (failingJournal) Put(*ledger.Entry) error                  { return errors.New("disk full") }
func (failingJournal) Get(string, string) (*ledger.Entry, bool) { return nil, false }

// TestTokenDeltaSignsAreBig 金额比较必须走大整数：u64 余额在 int64 边界附近会回绕。
func TestTokenDeltaSignsAreBig(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	// 余额接近 u64 上限时，post-pre 用 int64 算会回绕成正数。
	big1 := "18446744073709551615"
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, big1)},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "0")},
	}
	delta, err := c.tokenDelta(meta, &ledger.Entry{})
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if delta.Sign() >= 0 {
		t.Errorf("净变化=%s，应为一个负数（大整数不能回绕）", delta)
	}
	if delta.Cmp(new(big.Int).Neg(new(big.Int).SetUint64(^uint64(0)))) != 0 {
		t.Errorf("净变化=%s，期望 -%s", delta, big1)
	}
}

// 本文件只钉 SOL 侧的「接线正确性」：装配期「配置不全就静默不启动」，以及注入键名
// 与分派种类同源。门面机器本身（分派登记、重启关旧、订单补全、未启动拒绝）的行为
// 基线在 chainroute/route_test.go 只写一份，四链共用。

// TestNewSolUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
func TestNewSolUSDTClientRejectsIncomplete(t *testing.T) {
	wallet := solTestKey(t).PublicKey().String()
	good := func() *config.SolUsdtConfig {
		return &config.SolUsdtConfig{
			Enable:   "on",
			Driver:   driverSolRPC,
			Network:  "mainnet-beta",
			Contract: solBytes1To32,
			Decimals: 6,
			Chain:    &config.SolChainConfig{RPC: "https://example.invalid", WalletAddress: wallet},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 sol 段", &config.Cfg{}},
		{"缺 sol.usdt 段", &config.Cfg{Sol: &config.SolConfig{}}},
		{"enable 不是 on", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"mint 为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"mint 非法", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Contract = "not-an-address"
			return u
		}()}}},
		{"网络为空", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Sol: &config.SolConfig{Usdt: func() *config.SolUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
	}
	for _, cs := range cases {
		c, ok := newSolUSDTClient(cs.cfg)
		if ok {
			t.Errorf("%s: 装配成功了（应当拒绝）", cs.name)
			if c != nil {
				_ = c.Ch.Close()
			}
		}
		if c != nil {
			t.Errorf("%s: 拒绝时仍返回了非 nil 客户端", cs.name)
		}
	}
}

// TestNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestNamespaceKeys(t *testing.T) {
	if solUSDTKey != string(pay.KindSolUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", solUSDTKey, pay.KindSolUSDT)
	}
	if !strings.Contains(string(pay.KindSolUSDT), "sol") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindSolUSDT)
	}
}

// 本文件是离线签名器的断言：私钥三种形态解析、派生地址比对、交易签名与身份。
//
// 全部是纯本地用例：签名器不该为「证明自己能用」去连一次节点，而链上行为
// 已经在 query/onchain 的判据里各自钉过。

// solTestSeed 是测试用的确定性 seed（1..32）。用固定字节而不是随机密钥：
// 失败时的地址每次都一样，能直接拿去核对，不必先复现一次随机。
func solTestSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return seed
}

// solTestKey 返回 seed 对应的 64 字节 ed25519 私钥。
func solTestKey(t *testing.T) solana.PrivateKey {
	t.Helper()
	return solana.PrivateKey(ed25519.NewKeyFromSeed(solTestSeed()))
}

// solTestSigner 返回一个已通过派生地址核对的签名器。
func solTestSigner(t *testing.T) *signer {
	t.Helper()
	key := solTestKey(t)
	s, err := newSigner(key.String(), key.PublicKey().String())
	if err != nil {
		t.Fatalf("用合法私钥构造签名器失败: %v", err)
	}
	return s
}

// solTestMint 返回一个确定的 mint 地址（与热钱包不同的另一把公钥）。
func solTestMint(t *testing.T) solana.PublicKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(101 + i)
	}
	return solana.PrivateKey(ed25519.NewKeyFromSeed(seed)).PublicKey()
}

// solTestChannel 构造一条不含接入点的通道，只供纯函数判据使用。
//
// rpc 字段留 nil 是刻意的：这些用例一旦开始依赖节点，就会在离线环境里静默跳过，
// 而「跳过的资金闸」与「没有这道闸」在验收上看起来一样。
func solTestChannel(t *testing.T) *onchainChannel {
	t.Helper()
	sgn := solTestSigner(t)
	mint := solTestMint(t)
	return &onchainChannel{
		sign: sgn,
		opt: channelOptions{
			Name:     "usdt",
			Network:  "mainnet-beta",
			Mint:     mint.String(),
			Decimals: defaultSOLDecimals,
		},
		chain: &chainConfig{
			WalletAddress: sgn.Address().String(),
			PrivateKey:    solTestKey(t).String(),
			RPC:           "https://example.invalid/rpc",
			APIKey:        "test-api-key",
		},
		ledger: ledger.New("sol-test"),
		mint:   mint,
		nowFn:  time.Now,
	}
}

// TestParsePrivateKeyForms 三种私钥形态必须解出同一把密钥。
//
// 这条断言的意义在运维侧：助记词派生给的是 32 字节 seed，钱包导出给的是 64 字节
// 完整私钥，base58 是钱包常见的文本形态。三种都能用时，「换算脚本多写一个字节」
// 这条路径才会消失。
func TestParsePrivateKeyForms(t *testing.T) {
	key := solTestKey(t)
	want := key.PublicKey().String()
	cases := []struct {
		name string
		raw  string
	}{
		{"base58 64 字节", key.String()},
		{"hex 64 字节", hex.EncodeToString(key)},
		{"hex 32 字节 seed", hex.EncodeToString(solTestSeed())},
		{"0x 前缀 hex seed", "0x" + hex.EncodeToString(solTestSeed())},
		{"两侧空白", "  " + key.String() + "\n"},
	}
	for _, cs := range cases {
		got, err := parsePrivateKey(cs.raw)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", cs.name, err)
			continue
		}
		if got.PublicKey().String() != want {
			t.Errorf("%s: 派生地址 %s，期望 %s", cs.name, got.PublicKey(), want)
		}
	}
}

// TestParsePrivateKeyRejects 认不出来的形态必须报错，且错误里不得出现原文。
func TestParsePrivateKeyRejects(t *testing.T) {
	key := solTestKey(t)
	short := hex.EncodeToString(key[:31])
	overlong := hex.EncodeToString(append(append([]byte(nil), key...), 0x00))
	for _, cs := range []struct {
		name string
		raw  string
	}{
		{"空串", ""},
		{"只有空白", "   "},
		{"hex 31 字节", short},
		{"hex 65 字节", overlong},
		{"非 base58 的串", "not-a-key-0OIl"},
	} {
		got, err := parsePrivateKey(cs.raw)
		if err == nil {
			t.Errorf("%s: 应当解析失败，实得 %d 字节", cs.name, len(got))
			continue
		}
		if cs.raw != "" && strings.Contains(err.Error(), cs.raw) {
			t.Errorf("%s: 错误文案把私钥原文带了出去: %v", cs.name, err)
		}
	}
}

// TestNewSignerRequiresMatchingWallet 派生地址与配置地址不等时必须拒绝装配。
//
// 这是本通道唯一能拦住「按 A 的地址记账、用 B 的私钥出款」的地方：那种错在链上
// 不留痕迹（交易由 B 付钱，台账写着 A），只能在装配期判。
func TestNewSignerRequiresMatchingWallet(t *testing.T) {
	key := solTestKey(t)
	if _, err := newSigner(key.String(), key.PublicKey().String()); err != nil {
		t.Fatalf("匹配的钱包地址被拒: %v", err)
	}
	if _, err := newSigner(key.String(), solBytes1To32); err == nil {
		t.Error("派生地址与配置地址不等时仍然装配成功")
	}
	if _, err := newSigner(key.String(), "not-an-address"); err == nil {
		t.Error("非法 wallet_address 未被拦下")
	}
	if _, err := newSigner("", key.PublicKey().String()); err == nil {
		t.Error("空私钥未被拦下")
	}
}

// TestSignProducesStableIdentity 同一份交易字节签两次必须得到同一个签名。
//
// Solana 的交易身份就是第一个签名，ed25519 是确定性签名：同一份 message 签两次
// 结果逐字节相同。这正是「同一单号重建后如果拿到同一个签名，它仍然只有一笔」
// 的依据，也让台账里的 TxID 可以被复算核对。
func TestSignProducesStableIdentity(t *testing.T) {
	sgn := solTestSigner(t)
	inst := computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()
	build := func() *solana.Transaction {
		t.Helper()
		tx, err := solana.NewTransactionBuilder().
			SetRecentBlockHash(solana.Hash{1}).
			SetFeePayer(sgn.Address()).
			AddInstruction(inst).
			Build()
		if err != nil {
			t.Fatalf("组包失败: %v", err)
		}
		return tx
	}
	tx1, tx2 := build(), build()
	if err := sgn.Sign(tx1); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if err := sgn.Sign(tx2); err != nil {
		t.Fatalf("第二次签名失败: %v", err)
	}
	if len(tx1.Signatures) == 0 || tx1.Signatures[0].IsZero() {
		t.Fatal("签名后交易没有第一个签名")
	}
	if !tx1.Signatures[0].Equals(tx2.Signatures[0]) {
		t.Error("同一份交易签两次得到了不同签名：交易身份不再稳定")
	}
	if got, want := txSignature(tx1), tx1.Signatures[0].String(); got != want {
		t.Errorf("txSignature=%s，期望 %s", got, want)
	}
	// 签名必须能被公钥验过：这一步才真正证明它是这把私钥产的，而不是一段随机字节。
	msg, err := tx1.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("交易 message 不可序列化: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(sgn.Address().Bytes()), msg, tx1.Signatures[0][:]) {
		t.Error("签名无法用热钱包公钥验过")
	}
}

// TestSignRejectsForeignPayer 交易的第一签名人不是本钱包时必须拒绝签名。
//
// 拒的理由是资金面而不是格式：那种交易由别的账户付钱，本包的余额预检与台账
// 都会记到本钱包头上，签出来的结果在链上却指向另一个主体。
func TestSignRejectsForeignPayer(t *testing.T) {
	sgn := solTestSigner(t)
	foreign, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	tx, err := solana.NewTransactionBuilder().
		SetRecentBlockHash(solana.Hash{2}).
		SetFeePayer(foreign).
		AddInstruction(computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()).
		Build()
	if err != nil {
		t.Fatalf("组包失败: %v", err)
	}
	if err := sgn.Sign(tx); err == nil {
		t.Error("付款人不是本钱包的交易仍然签名成功")
	}
	if err := sgn.Sign(nil); err == nil {
		t.Error("nil 交易未被拦下")
	}
	if got := txSignature(nil); got != "" {
		t.Errorf("nil 交易的身份应为空串，实得 %q", got)
	}
}

// TestTxSignatureEmpty 空交易的身份是空串而不是 panic。
func TestTxSignatureEmpty(t *testing.T) {
	if got := txSignature(&solana.Transaction{}); got != "" {
		t.Errorf("无签名交易的身份应为空串，实得 %q", got)
	}
}

// Solana 地址夹具：base58 解码后恰为 32 字节的 ed25519 公钥形态。
// 三个向量都是先定字节（1..32 递增、全 FF、按公式生成）再现编码并 roundtrip 核过的，
// 不是抄来的——抄来的串一旦本身解不出 32 字节，测试就会把长度判断的错误永远藏住。
const (
	solBytes1To32  = "4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw"
	solAllFFAcct   = "JEKNVnkbo3jma5nREBBJCDoXFVeKkD56V3xKrvRmWxFG"
	solPatternAcct = "5PzNBPqQ1c4QcMg9zC2G5eF1P3nZRbZNWf471X2mYtBh"
)

// TestBase58DecodeRoundtrip 夹具的来历单独钉一次：解码后必须恰好 32 字节，
// 且内容与造向量时的原始字节一致——Solana 没有内嵌校验和，长度是本地唯一能核的性质。
func TestBase58DecodeRoundtrip(t *testing.T) {
	raw, err := base58Decode(solBytes1To32)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solBytes1To32, len(raw), err)
	}
	for i, b := range raw {
		if b != byte(i+1) {
			t.Fatalf("第 %d 字节=%02x，期望递增序列", i, b)
		}
	}
	raw, err = base58Decode(solAllFFAcct)
	if err != nil || len(raw) != solAddressLen {
		t.Fatalf("夹具 %s 应解出 32 字节: len=%d err=%v", solAllFFAcct, len(raw), err)
	}
	for i, b := range raw {
		if b != 0xFF {
			t.Fatalf("第 %d 字节=%02x，期望 FF", i, b)
		}
	}
}

// TestIsValidSolanaAddress 32 字节 base58 的正负用例。
func TestIsValidSolanaAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{solBytes1To32, true, "1..32 递增字节"},
		{solAllFFAcct, true, "全 FF 字节"},
		{solPatternAcct, true, "公式生成字节"},
		{"  " + solBytes1To32 + "  ", true, "两侧空白应被 trim 后过关"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{solBytes1To32[:42], false, "少一位（解码不足 32 字节）"},
		{solAllFFAcct + "1", false, "尾部多一个前导零字符（解码 33 字节）"},
		{"1" + solBytes1To32, false, "首部多一个前导零字符"},
		// 0OIl 不在 Base58 字母表里，混进来必须当场拒。
		{"0wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 0"},
		{"OwBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigw", false, "含非法字符 O"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4VigI", false, "含非法字符 I"},
		{"4wBqpZM9xaSheZzJSMawUKKwhdpChKbZ5eu5ky4Vigl", false, "含非法字符 l"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3A", false, "EVM 地址不是 Solana 地址"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 Solana 地址"},
	}
	// 逐位篡改：Solana 没有校验和，改一位仍然可能是「另一个格式合法的地址」——
	// 这里钉的是格式判定不会被篡改绕过，字母表外的替换必须拒。
	tampered := []rune(solPatternAcct)
	for i := range tampered {
		c := []rune{'0', 'O'}[i%2]
		cases = append(cases, struct {
			addr string
			ok   bool
			desc string
		}{string(tampered[:i]) + string(c) + string(tampered[i+1:]), false, "逐位替换为字母表外字符"})
	}
	for _, cs := range cases {
		if got := IsValidSolanaAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidSolanaAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestSOLRuleChecksAddress 本包的规则直接以 solRule 存在，不再经 pay 的注册表。
//
// 注册表已在 P5 随 pay 瘦身删除：规则只服务「本包自己那条链」，改成包内函数调用后
// 就不存在「忘了登记 ⇒ 未登记即放行」这条静默通道了。
func TestSOLRuleChecksAddress(t *testing.T) {
	rule := solRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32}}); err != nil {
		t.Errorf("合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: solBytes1To32[:42]}}); err == nil {
		t.Error("长度不足的地址未被拦下")
	}
}

// TestSOLRuleRejectsMemo Solana 原生与 SPL 转账都没有 TON comment 那种认款机制：
// 填了就拒而不是忽略。
func TestSOLRuleRejectsMemo(t *testing.T) {
	rule := solRule{}
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
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = SolWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = SolQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = SolAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 1000000000, To: &pay.Party{Address: solBytes1To32}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := SolWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := SolQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := SolAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := SolWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}
