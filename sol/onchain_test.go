package sol

import (
	"context"
	"crypto/ed25519"
	"math"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"

	"touchgocore/pay"
)

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
func TestFeeCeiling(t *testing.T) {
	c := solTestChannel(t)
	c.chain.PriorityFeeMicroLamports = 0
	if got := c.feeCeiling(); got != signatureFeeLamports {
		t.Errorf("不配优先费时上限=%d，期望 %d", got, signatureFeeLamports)
	}
	c.chain.PriorityFeeMicroLamports = 1
	want := int64(signatureFeeLamports + computeUnitLimit)
	if got := c.feeCeiling(); got != want {
		t.Errorf("优先费=1 时上限=%d，期望 %d", got, want)
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
