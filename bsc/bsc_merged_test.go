package bsc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/ledger"
	"touchgocore/pay"
)

// EVM 地址夹具：两个 EIP-55 校验和形态按官方规则现算（并与 go-ethereum 的
// Address.Hex() 逐字对拍过），单一大小写的历史形态不带校验和语义，格式过关即放行。
const (
	evmChecksumMixed  = "0xfb6916095cA1d6Ab8C1E3E5E1C0e5a2C2b4d0F3A"
	evmChecksumMixed2 = "0xDeaDbeefdEAdbeefdEadbEEFdeadbeEFdEaDbeeF"
	evmAllLower       = "0xfb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a"
	evmAllUpper       = "0xFB6916095CA1D6AB8C1E3E5E1C0E5A2C2B4D0F3A"
	evmAllDigits      = "0x1234567890123456789012345678901234567890"
	// evmBogusChecksum 与 evmChecksumMixed 只差一位字母的大小写，
	// 长度、前缀、字符集全对，只有校验和不符。
	evmBogusChecksum = "0xfb6916095ca1d6Ab8C1E3E5E1C0e5a2C2b4d0F3A"
)

// TestIsValidEVMAddress EIP-55 的正负用例：混合大小写必须等于校验和形态；
// 全小写、全大写、纯数字是历史形态，不携带校验和信息，格式过关即放行。
func TestIsValidEVMAddress(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
		desc string
	}{
		{evmChecksumMixed, true, "EIP-55 校验和形态"},
		{evmChecksumMixed2, true, "另一枚现算的校验和形态"},
		{evmAllLower, true, "全小写（无校验和语义的历史形态）"},
		{evmAllUpper, true, "全大写（同上）"},
		{evmAllDigits, true, "纯数字（无字母，大小写规则不适用）"},
		{evmBogusChecksum, false, "混合大小写但校验和不符"},
		{"", false, "空串"},
		{"   ", false, "只有空白"},
		{"fb6916095ca1d6ab8c1e3e5e1c0e5a2c2b4d0f3a", false, "缺 0x 前缀"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3", false, "少一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3AA", false, "多一位"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2B4D0F3Z", false, "含非十六进制字符"},
		{"0xfB6916095CA1d6AB8C1E3E5E1C0E5A2C2b4D0F3a", false, "校验和形态上翻转两个字母的大小写"},
		{"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", false, "TRON 地址不是 EVM 地址"},
	}
	// 逐位替换成 '0' / 'f' 的三向断言：结果若是混合大小写，除非恰好落在
	// EIP-55 形态上，否则必须被校验和拦下；单一大小写与纯数字则必须放行。
	body := []byte(evmChecksumMixed)
	for i := 2; i < len(body); i++ {
		orig := body[i]
		for _, cand := range []byte{'0', 'f'} {
			if cand == orig {
				continue
			}
			body[i] = cand
			addr := string(body)
			hex40 := addr[2:]
			// 替换后恰好落在校验和形态上的属巧合，不在被拒之列。
			if hex40 == toEIP55(hex40) {
				continue
			}
			mixed := hex40 != strings.ToLower(hex40) && hex40 != strings.ToUpper(hex40)
			if got := IsValidEVMAddress(addr); got == mixed {
				t.Errorf("替换后的地址 %q 判定不符：混合大小写=%v，实得 ok=%v", addr, mixed, got)
			}
		}
		body[i] = orig
	}
	// 逐位替换跑完后原夹具必须完好：循环里的写回如果漏了一位，这里会先红。
	if string(body) != evmChecksumMixed || !IsValidEVMAddress(evmChecksumMixed) {
		t.Fatalf("夹具 %s 在替换循环里被改坏了", evmChecksumMixed)
	}
	for _, cs := range cases {
		if got := IsValidEVMAddress(cs.addr); got != cs.ok {
			t.Errorf("%s: IsValidEVMAddress(%q)=%v，期望 %v", cs.desc, cs.addr, got, cs.ok)
		}
	}
}

// TestToEIP55OfficialVectors 用 EIP-55 的官方测试向量钉住推导本身。
//
// 这些向量是本包唯一的「外部真源」：只用自算的夹具时，推导与校验一旦同错同对
// （比如把摘要的整个字节拿去比 >= 8，而不是按奇偶取高低半字节），用例会全绿，
// 而真实地址全被判成非法。下面八个向量覆盖了全大写、全小写与真正带校验和的形态。
func TestToEIP55OfficialVectors(t *testing.T) {
	vectors := []string{
		"0x52908400098527886E0F7030069857D2E4169EE7",
		"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
		"0xde709f2102306220921060314715629080e2fb77",
		"0x27b1fdb04752bbc536007a920d24acb045561c26",
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
	}
	for _, v := range vectors {
		body := v[2:]
		if got := "0x" + toEIP55(strings.ToLower(body)); got != v {
			t.Errorf("EIP-55 推导不符:\n got=%s\nwant=%s", got, v)
		}
		if !IsValidEVMAddress(v) {
			t.Errorf("官方校验和形态被拒: %s", v)
		}
	}

	// 逐位翻转字母大小写：只要翻转后仍是混合大小写，就必须被校验和拦下。
	// 这是比「固定几个坏串」更硬的判据——五分之四种单点手抖都能被它覆盖。
	body := []byte(evmChecksumMixed)
	for i := 2; i < len(body); i++ {
		orig := body[i]
		var flipped byte
		switch {
		case orig >= 'a' && orig <= 'f':
			flipped = orig - 32
		case orig >= 'A' && orig <= 'F':
			flipped = orig + 32
		default:
			continue
		}
		body[i] = flipped
		addr := string(body)
		hex40 := addr[2:]
		mixed := hex40 != strings.ToLower(hex40) && hex40 != strings.ToUpper(hex40)
		if got := IsValidEVMAddress(addr); got == mixed {
			t.Errorf("翻转第 %d 位大小写后的 %q 判定不符：混合大小写=%v，实得 ok=%v", i, addr, mixed, got)
		}
		body[i] = orig
	}
	if string(body) != evmChecksumMixed {
		t.Fatal("翻转循环把夹具改坏了")
	}
}

// TestBNBRuleChecksAddress 本包的规则直接以 bnbRule 存在，不再经 pay 的注册表。
//
// 注册表已在 P5 随 pay 瘦身删除：规则只服务「本包自己那条链」，改成包内函数调用后
// 就不存在「忘了登记 ⇒ 未登记即放行」这条静默通道了。
func TestBNBRuleChecksAddress(t *testing.T) {
	rule := bnbRule{}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: evmAllLower}}); err != nil {
		t.Errorf("全小写合法地址被拒: %v", err)
	}
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: evmBogusChecksum}}); err == nil {
		t.Error("校验和不过的地址未被拦下")
	}
}

// TestBNBRuleRejectsMemo EVM 转账没有 memo 概念：填了就拒而不是忽略。
func TestBNBRuleRejectsMemo(t *testing.T) {
	rule := bnbRule{}
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
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = BscWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = BscQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = BscAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: evmAllLower}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := BscWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := BscQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := BscAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); !errors.Is(err, ErrPayoutNotWired) {
			t.Errorf("%s 应返回未接入哨兵，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := BscWithdraw(nil, nil); !errors.Is(err, ErrPayoutNotWired) {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}

// TestCheckOrderAddress 包内地址规则的四个口径。
//
// 这条闸代替了原来登记在 pay 注册表里的 ChainRule：那时「未登记即放行」，
// 币种名拼错一个字母就静默失效；现在出款路径只有一个调用点，规则就在那个点上。
func TestCheckOrderAddress(t *testing.T) {
	ok := []struct {
		desc string
		to   *pay.Party
	}{
		{"EIP-55 校验和形态", &pay.Party{Address: evmChecksumMixed}},
		{"全小写（无校验和语义）", &pay.Party{Address: evmAllLower}},
		{"全大写（无校验和语义）", &pay.Party{Address: evmAllUpper}},
		{"纯数字", &pay.Party{Address: evmAllDigits}},
		{"首尾空白", &pay.Party{Address: "  " + evmAllLower + "\n"}},
		{"空地址（非空检查在 pay.CheckOrder）", &pay.Party{}},
	}
	for _, cs := range ok {
		if err := checkOrderAddress(&pay.PayOrder{To: cs.to}); err != nil {
			t.Errorf("%s: 被拒: %v", cs.desc, err)
		}
	}

	bad := []struct {
		desc string
		to   *pay.Party
		want string
	}{
		{"校验和不过", &pay.Party{Address: evmBogusChecksum}, "不合法"},
		{"少一位", &pay.Party{Address: evmAllLower[:len(evmAllLower)-1]}, "不合法"},
		{"缺 0x 前缀", &pay.Party{Address: evmAllLower[2:]}, "不合法"},
		{"混入非法字符", &pay.Party{Address: "0xzz34567890123456789012345678901234567890"}, "不合法"},
		{"TRON 地址", &pay.Party{Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}, "不合法"},
		{"带 memo", &pay.Party{Address: evmAllLower, Memo: "123456"}, "不支持备注"},
		{"memo 只有空白", &pay.Party{Address: evmAllLower, Memo: "  "}, ""},
	}
	for _, cs := range bad {
		err := checkOrderAddress(&pay.PayOrder{To: cs.to})
		if cs.want == "" {
			if err != nil {
				t.Errorf("%s: 被拒: %v", cs.desc, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: 未被拒", cs.desc)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 拒因文案不符（缺 %q）: %v", cs.desc, cs.want, err)
		}
	}

	// nil 订单/收款方放行：本函数只回答「格式对不对」，
	// 「有没有收款方」是 pay.CheckOrder 的职责，两处都判会让判据漂移。
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单被拒: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("nil 收款方被拒: %v", err)
	}
}

// newTestChannel 造一条已过装配闸、但**没有接入点**的出款通道。
//
// rpc 留 nil 是有意的：下面这些用例验的都是「在动链之前就该拒」的闸，
// 一旦某条闸失效，用例会在 nil 上 panic 而不是安静通过——这比断言一个错误文案更硬。
// 任何需要真节点的路径（广播、查回执）都不在这里测。
func newTestChannel(t *testing.T) *onchainChannel {
	t.Helper()
	sgn := mustTestSigner(t)
	return &onchainChannel{
		sign: sgn,
		opt: channelOptions{
			Name:     "usdt",
			Driver:   driverBSCRPC,
			Network:  "mainnet",
			Contract: evmAllLower,
			Decimals: defaultBSCDecimals,
			Chain:    &chainConfig{ChainID: testChainID, WalletAddress: testWallet, PrivateKey: testPrivKeyHex},
		},
		chain:  &chainConfig{ChainID: testChainID, WalletAddress: testWallet, PrivateKey: testPrivKeyHex},
		ledger: ledger.New("bsc-test"),
		token:  common.HexToAddress(evmAllLower),
		nowFn:  time.Now,
	}
}

// walletOrder 造一条能通过 pay.CheckOrder 的钱包出款单。
func walletOrder(addr string) *pay.PayOrder {
	return &pay.PayOrder{
		OrderNo:  "O-1",
		Amount:   1_000_000,
		Currency: pay.CurrencyUSDT,
		To:       &pay.Party{Address: addr},
	}
}

// TestWithdrawRejectsNonWalletTarget 收款方性质不对时必须在地址解析之前拒。
//
// 银行代付与商户号收款本通道发不了。若让它们落到地址解析里，报出来的会是
// 「BSC 收款地址 6222… 不合法」——读的人会以为卡号填错了，而不是走错了通道。
func TestWithdrawRejectsNonWalletTarget(t *testing.T) {
	c := newTestChannel(t)
	cases := []struct {
		desc string
		to   *pay.Party
		want string
	}{
		{"银行卡", &pay.Party{Kind: pay.PartyBank, Bank: &pay.BankAccount{AccountNo: "6222333344445555"}}, "银行转账"},
		{"商户号", &pay.Party{Kind: pay.PartyMerchant, Account: "M-1"}, "商户号"},
	}
	for _, cs := range cases {
		o := &pay.PayOrder{OrderNo: "O-1", Amount: 100, To: cs.to}
		_, err := c.Withdraw(context.Background(), o)
		if err == nil {
			t.Errorf("%s: 未被拒", cs.desc)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 拒因文案不符（缺 %q）: %v", cs.desc, cs.want, err)
		}
		if !strings.Contains(err.Error(), "不支持") {
			t.Errorf("%s: 文案没说清是通道不支持: %v", cs.desc, err)
		}
	}
}

// TestWithdrawRejectsBadOrder 前置契约仍走 pay.CheckOrder：缺单号/零金额/缺收款方。
func TestWithdrawRejectsBadOrder(t *testing.T) {
	c := newTestChannel(t)
	cases := []struct {
		desc string
		o    *pay.PayOrder
		want string
	}{
		{"nil 订单", nil, "订单为 nil"},
		{"缺单号", &pay.PayOrder{Amount: 1, To: &pay.Party{Address: evmAllLower}}, "订单号"},
		{"零金额", &pay.PayOrder{OrderNo: "O", Amount: 0, To: &pay.Party{Address: evmAllLower}}, "金额"},
		{"负金额", &pay.PayOrder{OrderNo: "O", Amount: -1, To: &pay.Party{Address: evmAllLower}}, "金额"},
		{"缺收款方", &pay.PayOrder{OrderNo: "O", Amount: 1}, "收款方"},
		{"空收款方", &pay.PayOrder{OrderNo: "O", Amount: 1, To: &pay.Party{}}, "既没有地址"},
	}
	for _, cs := range cases {
		_, err := c.Withdraw(context.Background(), cs.o)
		if err == nil {
			t.Errorf("%s: 未被拒", cs.desc)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 拒因文案不符（缺 %q）: %v", cs.desc, cs.want, err)
		}
	}
}

// TestWithdrawRejectsDecimalsMismatch 金额位数声明与配置不符即拒。
//
// USDT 在 TRON 是 6 位、在 BSC 是 18 位。把 6 填进 BSC 的单里，Amount 会小 10^12 倍；
// 反过来则多付 10^12 倍。这笔钱一旦上链没有任何回滚手段，只能在组包之前拒。
func TestWithdrawRejectsDecimalsMismatch(t *testing.T) {
	c := newTestChannel(t)
	for _, d := range []int{6, 17, 19} {
		o := walletOrder(evmAllLower)
		o.Decimals = d
		_, err := c.Withdraw(context.Background(), o)
		if err == nil {
			t.Errorf("decimals=%d 未被拒", d)
			continue
		}
		if !strings.Contains(err.Error(), "小数位") {
			t.Errorf("decimals=%d 的拒因文案不符: %v", d, err)
		}
	}
	// 留 0 表示不声明，按配置解释；等于配置则放行（放到 ledger 分支前，不触链）。
	if err := c.checkDecimals(walletOrder(evmAllLower)); err != nil {
		t.Errorf("decimals 留空被拒: %v", err)
	}
	o := walletOrder(evmAllLower)
	o.Decimals = defaultBSCDecimals
	if err := c.checkDecimals(o); err != nil {
		t.Errorf("decimals 等于配置被拒: %v", err)
	}
}

// TestWithdrawRejectsForeignAsset 网络与币种都必须与本通道配置一致。
func TestWithdrawRejectsForeignAsset(t *testing.T) {
	c := newTestChannel(t)

	o := walletOrder(evmAllLower)
	o.To.Network = "chapel"
	if _, err := c.Withdraw(context.Background(), o); err == nil {
		t.Error("换网的单未被拒")
	} else if !strings.Contains(err.Error(), "换网就是换币") {
		t.Errorf("换网拒因文案不符: %v", err)
	}

	o = walletOrder(evmAllLower)
	o.To.Network = "MainNet" // 大小写不敏感：网络名是配置里的字面量，不是地址
	if err := c.checkAsset(o); err != nil {
		t.Errorf("网络名大小写不同被拒: %v", err)
	}
	o.To.Network = ""
	if err := c.checkAsset(o); err != nil {
		t.Errorf("网络留空（由门面补默认值）被拒: %v", err)
	}

	o = walletOrder(evmAllLower)
	o.Currency = "TRX"
	if err := c.checkAsset(o); err == nil {
		t.Error("非 USDT 币种未被拒")
	}
	o.Currency = "usdt"
	if err := c.checkAsset(o); err != nil {
		t.Errorf("小写币种被拒: %v", err)
	}
}

// TestAssetOfPinsHotWallet 出款方默认是配置热钱包；按单私钥必须与 From.Address 同一账户。
func TestAssetOfPinsHotWallet(t *testing.T) {
	c := newTestChannel(t)
	if _, err := c.assetOf(walletOrder(evmAllLower)); err != nil {
		t.Errorf("From 留空被拒: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{}}); err != nil {
		t.Errorf("From 空地址被拒: %v", err)
	}
	// 同一地址的另一种合法写法：按字节比，不能被当成另一个账户。
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: strings.ToLower(testWallet)}}); err != nil {
		t.Errorf("同一地址的小写形态被拒: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: testWallet}}); err != nil {
		t.Errorf("热钱包地址被拒: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: evmChecksumMixed2}}); err == nil {
		t.Error("非热钱包地址未被拒")
	} else if !strings.Contains(err.Error(), "无法签名") {
		t.Errorf("拒因文案不符: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: "0xzz"}}); err == nil {
		t.Error("非法 From 地址未被拒")
	} else if !strings.Contains(err.Error(), "不是合法的 EVM 地址") {
		t.Errorf("非法 From 的拒因文案不符: %v", err)
	}

	// 按单私钥：派生地址与 From.Address 一致才放行；留空地址则用派生地址。
	keyHex, addr := qaBSCPerOrderKey(t)
	a, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: addr.Hex(), PrivateKey: pay.Secret(keyHex)}})
	if err != nil {
		t.Fatalf("按单私钥被拒: %v", err)
	}
	if a.wallet != addr || a.sign == nil || a.secret != keyHex {
		t.Fatalf("按单资产解析错: %+v", a)
	}
	a, err = c.assetOf(&pay.PayOrder{From: &pay.Party{PrivateKey: pay.Secret(keyHex)}})
	if err != nil || a.wallet != addr {
		t.Fatalf("按单私钥留空地址应回落到派生地址: asset=%+v err=%v", a, err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Address: testWallet, PrivateKey: pay.Secret(keyHex)}}); err == nil {
		t.Fatal("私钥与 From.Address 不匹配未被拒")
	} else if !strings.Contains(err.Error(), "不是同一个账户") {
		t.Errorf("不匹配拒因文案不符: %v", err)
	}

	// 合约与网络仍钉配置：本轮不放开按单换币。
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Contract: evmChecksumMixed2}}); err == nil {
		t.Error("按单换合约未被拒")
	} else if !strings.Contains(err.Error(), "不得按单换币") {
		t.Errorf("换币拒因文案不符: %v", err)
	}
	if _, err := c.assetOf(&pay.PayOrder{From: &pay.Party{Network: "chapel"}}); err == nil {
		t.Error("按单换网未被拒")
	} else if !strings.Contains(err.Error(), "配的是 mainnet 网络") {
		t.Errorf("换网拒因文案不符: %v", err)
	}
}

// qaBSCPerOrderKey 造一把随机私钥及其 EVM 地址（用例内自洽，不依赖固定向量）。
func qaBSCPerOrderKey(t *testing.T) (string, common.Address) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(crypto.FromECDSA(key)), crypto.PubkeyToAddress(key.PublicKey)
}

// TestSameRecipient EVM 地址的同一性按字节判，不按字符串判。
func TestSameRecipient(t *testing.T) {
	to := common.HexToAddress(evmAllLower)
	if !sameRecipient("", to) {
		t.Error("台账没记收端时应放行")
	}
	if !sameRecipient("   ", to) {
		t.Error("台账收端只有空白时应放行")
	}
	if !sameRecipient(evmAllLower, to) {
		t.Error("同一地址的全小写形态被判成不同")
	}
	if !sameRecipient(strings.ToUpper("0x"+evmAllLower[2:]), to) {
		t.Error("同一地址的全大写形态被判成不同")
	}
	if sameRecipient(evmChecksumMixed2, to) {
		t.Error("不同地址被判成同一")
	}
}

// TestWithdrawRejectsSettledOrder 已终局的单号再动一次就是双付。
func TestWithdrawRejectsSettledOrder(t *testing.T) {
	c := newTestChannel(t)
	entry := &ledger.Entry{
		OrderNo: "O-1", TxID: "0xdead", To: evmAllLower,
		Contract: evmAllLower, Amount: 1_000_000,
		State: ledger.StateSettled, UpdatedAtMS: ledger.NowMS(time.Now()),
	}
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	_, err := c.Withdraw(context.Background(), walletOrder(evmAllLower))
	if err == nil {
		t.Fatal("已终局的单号未被拒")
	}
	if !errors.Is(err, ErrOrderAlreadySettled) {
		t.Errorf("拒因不是「已终局」哨兵: %v", err)
	}
	if !strings.Contains(err.Error(), "0xdead") {
		t.Errorf("拒因未带出既有交易的哈希: %v", err)
	}
}

// TestWithdrawRejectsChangedRecipient 同一幂等键不得改收款方。
func TestWithdrawRejectsChangedRecipient(t *testing.T) {
	c := newTestChannel(t)
	entry := &ledger.Entry{
		OrderNo: "O-1", TxID: "0xbeef", To: evmChecksumMixed2,
		Contract: evmAllLower, Amount: 1_000_000,
		State: ledger.StateBroadcast, UpdatedAtMS: ledger.NowMS(time.Now()),
	}
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	_, err := c.Withdraw(context.Background(), walletOrder(evmAllLower))
	if err == nil {
		t.Fatal("改收款方的重试未被拒")
	}
	if !strings.Contains(err.Error(), "不得改收款方") {
		t.Errorf("拒因文案不符: %v", err)
	}
}

// TestPrepareRecordsBeforeBroadcast 先记账后广播：prepare 结束时台账里必须已有这一笔，
// 且红黑两个地址/金额/哈希都能从台账反推回同一份已签名交易。
//
// 顺序反过来（先广播再记账）会留下「链上有一笔、本地查不到」的状态，那正是双付的温床。
func TestPrepareRecordsBeforeBroadcast(t *testing.T) {
	c := newTestChannel(t)
	to := common.HexToAddress(evmAllLower)
	o := walletOrder(evmAllLower)
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    9,
		GasPrice: gweiToWei(1),
		Gas:      defaultTransferGasLimit,
		To:       &c.token,
		Value:    big.NewInt(0),
		Data:     erc20TransferData(to, big.NewInt(o.Amount)),
	})
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析资产失败: %v", err)
	}
	entry, err := c.prepare(o, tx, to, a, big.NewInt(o.Amount), time.Now())
	if err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	if entry.From != common.HexToAddress(testWallet).Hex() {
		t.Fatalf("台账 From = %q，期望配置热钱包 %s", entry.From, common.HexToAddress(testWallet).Hex())
	}
	if entry.State != ledger.StatePrepared {
		t.Errorf("记账状态 = %s，期望 prepared", entry.State)
	}
	if entry.ExpiresAtMS != 0 {
		t.Errorf("EVM 交易不该有链上有效期，ExpiresAtMS=%d", entry.ExpiresAtMS)
	}
	if entry.Contract != evmAllLower || entry.Amount != o.Amount || common.HexToAddress(entry.To) != to {
		t.Errorf("台账定位字段不符: %+v", entry)
	}

	stored, found := c.ledger.Get(o.OrderNo)
	if !found {
		t.Fatal("prepare 之后台账里查不到这一单")
	}
	if stored.State != ledger.StatePrepared || stored.TxID != entry.TxID {
		t.Errorf("台账落的不是同一笔: %+v", stored)
	}

	signed, err := decodeRawTx(stored)
	if err != nil {
		t.Fatalf("台账里的原始交易不可还原: %v", err)
	}
	if signed.Nonce() != 9 || signed.Gas() != defaultTransferGasLimit {
		t.Errorf("还原出的交易参数不符: nonce=%d gas=%d", signed.Nonce(), signed.Gas())
	}
	if signed.To() == nil || *signed.To() != c.token {
		t.Errorf("交易的 to 不是代币合约: %v", signed.To())
	}
	if txHash(signed) != stored.TxID {
		t.Errorf("哈希与台账不符: %s vs %s", txHash(signed), stored.TxID)
	}
	from, err := types.Sender(types.NewEIP155Signer(big.NewInt(testChainID)), signed)
	if err != nil || from != common.HexToAddress(testWallet) {
		t.Errorf("发送方不是热钱包: from=%v err=%v", from.Hex(), err)
	}
	if !strings.EqualFold(strings.TrimPrefix(entry.Contract, "0x"), evmAllLower[2:]) {
		t.Errorf("台账合约地址不符: %s", entry.Contract)
	}
}

// TestDecodeRawTxRejectsGarbage 台账里没有原始交易/原文损坏都必须报错。
func TestDecodeRawTxRejectsGarbage(t *testing.T) {
	if _, err := decodeRawTx(nil); err == nil {
		t.Error("nil 条目未被拒")
	}
	if _, err := decodeRawTx(&ledger.Entry{OrderNo: "O"}); err == nil {
		t.Error("缺 RawTx 未被拒")
	}
	if _, err := decodeRawTx(&ledger.Entry{OrderNo: "O", RawTx: []byte{0xff, 0xff, 0xff}}); err == nil {
		t.Error("损坏的 RawTx 未被拒")
	}
}

// TestNewBscOnchainChannelGates 装配期五道闸：chain、合约、钱包地址、私钥、链 ID。
//
// 这些用例都不会拨号（rpc 为空），因此能证明闸在「连节点之前」就已判死。
func TestNewBscOnchainChannelGates(t *testing.T) {
	base := func() channelOptions {
		return channelOptions{
			Name:     "usdt",
			Network:  "mainnet",
			Contract: evmAllLower,
			Decimals: defaultBSCDecimals,
			Chain:    &chainConfig{ChainID: testChainID, WalletAddress: testWallet, PrivateKey: testPrivKeyHex},
		}
	}
	cases := []struct {
		desc string
		mod  func(*channelOptions)
		want string
	}{
		{"缺 chain", func(o *channelOptions) { o.Chain = nil }, "缺少"},
		{"空合约", func(o *channelOptions) { o.Contract = "  " }, "未配置代币合约"},
		{"非法合约", func(o *channelOptions) { o.Contract = "0xzz" }, "不合法"},
		{"空钱包", func(o *channelOptions) { o.Chain.WalletAddress = "" }, "wallet_address 为空"},
		{"空私钥", func(o *channelOptions) { o.Chain.PrivateKey = "" }, "私钥为空"},
		{"链 ID 为 0", func(o *channelOptions) { o.Chain.ChainID = 0 }, "chain_id"},
		{"钱包与私钥不匹配", func(o *channelOptions) { o.Chain.WalletAddress = evmChecksumMixed2 }, "不是同一个账户"},
		{"RPC 为空", func(o *channelOptions) { o.Chain.RPC = "" }, "chain.rpc 为空"},
		{"RPC 形态不符", func(o *channelOptions) { o.Chain.RPC = "127.0.0.1:8545" }, "不是 http"},
	}
	for _, cs := range cases {
		opt := base()
		cs.mod(&opt)
		if _, err := newBscOnchainChannel(opt); err == nil {
			t.Errorf("%s: 未被拒", cs.desc)
		} else if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 拒因文案不符（缺 %q）: %v", cs.desc, cs.want, err)
		}
	}
}

// TestQueryOrderUnknownOrder 台账里没有这一单号时给 UNKNOWN，绝不给 FAILED。
//
// 「查不到」说成失败，上游就会重下一单——而那正是双付。
func TestQueryOrderUnknownOrder(t *testing.T) {
	c := newTestChannel(t)
	res, err := c.QueryOrder(context.Background(), "  O-unknown  ")
	if err != nil {
		t.Fatalf("查无此单报了错误: %v", err)
	}
	if res == nil || res.Status != pay.StatusUnknown {
		t.Fatalf("查无此单的状态 = %v，期望 UNKNOWN", res)
	}
	if res.OrderNo != "O-unknown" {
		t.Errorf("回执单号未去空白: %q", res.OrderNo)
	}
	if res.RawNote == "" {
		t.Error("回执没有说明为什么查不到")
	}
	if _, err := c.QueryOrder(context.Background(), "  "); err == nil {
		t.Error("空单号未被拒")
	}
}

// TestQueryAccountRejectsForeignCurrency 本通道只有 USDT 一个币种。
func TestQueryAccountRejectsForeignCurrency(t *testing.T) {
	c := newTestChannel(t)
	if _, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: "TRX"}); err == nil {
		t.Error("非 USDT 的账户查询未被拒")
	} else if !strings.Contains(err.Error(), "只有 USDT 一个币种") {
		t.Errorf("拒因文案不符: %v", err)
	}
}

// TestRedactRemovesSecrets 对外的摘要不得带出私钥与 RPC 基址。
func TestRedactRemovesSecrets(t *testing.T) {
	c := newTestChannel(t)
	c.chain.RPC = "https://bsc.example/v1/SECRETKEY"
	msg := "dial " + c.chain.RPC + " failed; key=" + c.chain.PrivateKey
	got := c.redact(msg)
	if strings.Contains(got, "SECRETKEY") || strings.Contains(got, testPrivKeyHex) {
		t.Errorf("摘要仍带凭证: %s", got)
	}
	if c.redact("") != "" {
		t.Error("空串不该被改动")
	}
}

// TestPrepareRecordsLargeAmountUnits 大额 BSC 出款：台账必须保存精确十进制串，
// int64 Amount 只是饱和后的兼容值。
func TestPrepareRecordsLargeAmountUnits(t *testing.T) {
	c := newTestChannel(t)
	to := common.HexToAddress(evmAllLower)
	o := walletOrder(evmAllLower)
	o.Amount = 0
	o.AmountUnits = "100000000000000000000" // 100 USDT
	amount, err := o.AmountBig()
	if err != nil {
		t.Fatalf("AmountBig 失败: %v", err)
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce: 1, GasPrice: gweiToWei(1), Gas: defaultTransferGasLimit,
		To: &c.token, Value: big.NewInt(0), Data: erc20TransferData(to, amount),
	})
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析资产失败: %v", err)
	}
	entry, err := c.prepare(o, tx, to, a, amount, time.Now())
	if err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	if entry.AmountUnits != o.AmountUnits {
		t.Fatalf("台账精确金额=%q，期望 %q", entry.AmountUnits, o.AmountUnits)
	}
	if entry.Amount != int64(^uint64(0)>>1) {
		t.Fatalf("int64 兼容金额应饱和，实得 %d", entry.Amount)
	}
}

// TestReverseCandidateNeverSettles F2：反查命中同址同额候选时只禁止广播，
// 候选不落账、不查证，结果必须是 UNKNOWN + 候选 txid。
func TestReverseCandidateNeverSettles(t *testing.T) {
	c := newTestChannel(t)
	o := walletOrder(evmAllLower)
	txid := "0x" + strings.Repeat("ab", 32)
	c.reverseLookup = func(context.Context, *pay.PayOrder, orderAsset, time.Time) (*ledger.Entry, error) {
		return &ledger.Entry{
			OrderNo: o.OrderNo, TxID: txid, From: testWallet, To: evmAllLower,
			Contract: evmAllLower, Amount: o.Amount, AmountUnits: "1000000",
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

func bscPerOrderOrder(t *testing.T) (*onchainChannel, *pay.PayOrder, orderAsset) {
	t.Helper()
	c := newTestChannel(t)
	keyHex, addr := qaBSCPerOrderKey(t)
	o := walletOrder(evmAllLower)
	o.OrderNo = "PO-BSC-PER-1"
	o.From = &pay.Party{Kind: pay.PartyWallet, Address: addr.Hex(), PrivateKey: pay.Secret(keyHex)}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	return c, o, a
}

func TestWithdrawLockedPinsPerOrderWallet(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "0xdead", From: evmChecksumMixed2,
		To: o.To.Address, Contract: c.opt.Contract, Amount: o.Amount,
		State: ledger.StateSettled, UpdatedAtMS: ledger.NowMS(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同单号换钱包未被拒: %v", err)
	}
}

func TestWithdrawLockedPinsContract(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "0xdead", From: a.wallet.Hex(),
		To: o.To.Address, Contract: evmChecksumMixed2, Amount: o.Amount,
		State: ledger.StateSettled, UpdatedAtMS: ledger.NowMS(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同单号换合约未被拒: %v", err)
	}
}

func TestPrepareRecordsPerOrderFrom(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	to := common.HexToAddress(strings.TrimSpace(o.To.Address))
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    1,
		GasPrice: gweiToWei(1),
		Gas:      defaultTransferGasLimit,
		To:       &c.token,
		Value:    big.NewInt(0),
		Data:     erc20TransferData(to, big.NewInt(o.Amount)),
	})
	entry, err := c.prepare(o, tx, to, a, big.NewInt(o.Amount), time.Now())
	if err != nil {
		t.Fatalf("按单资产记账失败: %v", err)
	}
	if entry.From != a.wallet.Hex() {
		t.Fatalf("台账 From=%q，期望按单钱包 %s", entry.From, a.wallet.Hex())
	}
	if entry.Contract != c.opt.Contract {
		t.Fatalf("台账合约=%q，期望配置合约 %s", entry.Contract, c.opt.Contract)
	}
}

func TestMatchesTransferUsesEntryWallet(t *testing.T) {
	c := newTestChannel(t)
	_, addr := qaBSCPerOrderKey(t)
	to := common.HexToAddress(evmAllLower)
	entry := &ledger.Entry{
		From: addr.Hex(), To: to.Hex(), Contract: c.opt.Contract, Amount: 1_000_000,
	}
	rec := &types.Receipt{Logs: []*types.Log{
		transferLog(c.token, addr, to, big.NewInt(1_000_000)),
	}}
	if !c.matchesTransfer(rec, entry) {
		t.Fatal("按单钱包发出的 Transfer 未被识别")
	}
	// 换成配置热钱包后，同一份回执不能被认成这一单（旧台账没有 From 才回落配置）。
	entry.From = common.HexToAddress(testWallet).Hex()
	if c.matchesTransfer(rec, entry) {
		t.Fatal("回执里的 from 与台账不是同一账户却被认成已付")
	}
}

func TestNonceCacheIsPerAddress(t *testing.T) {
	c := newTestChannel(t)
	_, a1 := qaBSCPerOrderKey(t)
	_, a2 := qaBSCPerOrderKey(t)
	c.adoptNonce(a1, 5)
	c.adoptNonce(a2, 9)
	if got := c.nonces[a1]; got != 6 {
		t.Fatalf("地址 A 的 next nonce=%d，期望 6", got)
	}
	if got := c.nonces[a2]; got != 10 {
		t.Fatalf("地址 B 的 next nonce=%d，期望 10（不能被 A 的缓存覆盖）", got)
	}
}

// TestAdoptNonceConcurrentSafe 复现审查里的严重并发路径：prepared 态重广播不持 sendMu
// 就经 adoptNonce 写 nonces map，与其他 goroutine 并发读写同一张 map。无锁实现会被
// Go 运行时判为 "concurrent map writes" 直接 fatal，-race 下本用例即回归护栏。
// 同时验证单调推进：乱序并发写不会把某个地址的缓存回退。
func TestAdoptNonceConcurrentSafe(t *testing.T) {
	c := newTestChannel(t)
	_, a1 := qaBSCPerOrderKey(t)
	_, a2 := qaBSCPerOrderKey(t)
	const goroutines = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			// 两个地址混合写：同桶覆盖与跨桶新增都要在锁内完成。
			c.adoptNonce(a1, uint64(i))
			c.adoptNonce(a2, uint64(i*2))
		}(i)
	}
	wg.Wait()
	c.nonceMu.Lock()
	got1, got2 := c.nonces[a1], c.nonces[a2]
	c.nonceMu.Unlock()
	if want := uint64(goroutines); got1 != want {
		t.Errorf("a1 的 next nonce=%d，期望 %d（应单调推进到 max+1）", got1, want)
	}
	if want := uint64((goroutines-1)*2 + 1); got2 != want {
		t.Errorf("a2 的 next nonce=%d，期望 %d", got2, want)
	}
}

// transferLog 造一条本合约发出的 Transfer(address,address,uint256) 事件。
func transferLog(token, from, to common.Address, amount *big.Int) *types.Log {
	return &types.Log{
		Address: token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to)},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
	}
}

// broadcastEntry 造一条「已交给节点、等回执」的台账条目。
func broadcastEntry(to string, amount int64) *ledger.Entry {
	return &ledger.Entry{
		OrderNo: "O-1", TxID: "0xabc0000000000000000000000000000000000000000000000000000000000001",
		To: to, Contract: evmAllLower, Amount: amount,
		State: ledger.StateBroadcast, UpdatedAtMS: ledger.NowMS(time.Now()),
	}
}

// TestJudgeFailedReceipt 进块但执行失败：gas 照烧、钱没动，是终局。
//
// 同一份 raw tx 重广播结论一样，换一笔就是第二笔钱——所以判 FAILED 并落终局。
func TestJudgeFailedReceipt(t *testing.T) {
	c := newTestChannel(t)
	entry := broadcastEntry(evmAllLower, 1_000_000)
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	rec := &types.Receipt{
		Status:            types.ReceiptStatusFailed,
		GasUsed:           51_000,
		EffectiveGasPrice: big.NewInt(1_000_000_000),
	}
	res, err := c.judge(context.Background(), entry, rec, time.Now())
	if err != nil {
		t.Fatalf("judge 报错: %v", err)
	}
	if res == nil || res.Status != pay.StatusFailed {
		t.Fatalf("状态 = %v，期望 FAILED", res)
	}
	if res.TxHash != entry.TxID || res.OrderNo != entry.OrderNo {
		t.Errorf("回执没带上定位信息: %+v", res)
	}
	if want := int64(51_000) * 1_000_000_000; res.Fee != want {
		t.Errorf("手续费 = %d，期望 %d", res.Fee, want)
	}
	if res.FeeCurrency != pay.CurrencyBNB {
		t.Errorf("手续费币种 = %q，期望 BNB", res.FeeCurrency)
	}
	stored, _ := c.ledger.Get(entry.OrderNo)
	if stored == nil || stored.State != ledger.StateSettled {
		t.Errorf("失败结论没有落成终局: %+v", stored)
	}
}

// TestJudgeSuccessNeedsTransferEvent 成功的双判：status=1 且本单的 Transfer 事件在场。
func TestJudgeSuccessNeedsTransferEvent(t *testing.T) {
	c := newTestChannel(t)
	entry := broadcastEntry(evmAllLower, 1_000_000)
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	rec := &types.Receipt{
		Status:            types.ReceiptStatusSuccessful,
		GasUsed:           51_000,
		EffectiveGasPrice: big.NewInt(1_000_000_000),
		// BlockNumber 留 nil：本用例不接节点，确认数走「读不到链头则留空」的分支。
		Logs: []*types.Log{
			transferLog(c.token, c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(1_000_000)),
		},
	}
	res, err := c.judge(context.Background(), entry, rec, time.Now())
	if err != nil {
		t.Fatalf("judge 报错: %v", err)
	}
	if res == nil || res.Status != pay.StatusSuccess {
		t.Fatalf("状态 = %v，期望 SUCCESS", res)
	}
	if res.Confirmations != nil {
		t.Errorf("读不到链头时不该编出确认数: %d", *res.Confirmations)
	}
	stored, _ := c.ledger.Get(entry.OrderNo)
	if stored == nil || stored.State != ledger.StateSettled {
		t.Errorf("成功结论没有落成终局: %+v", stored)
	}
}

// TestJudgeSuccessWithoutTransferIsUnknown status=1 但找不到本单事件：说不清，不落终局。
//
// 有些老代币在余额不足时返回 false 而不 revert：回执是成功的，钱却没动。
// 把它读成成功是账实不符，读成失败又可能已经动过钱——只能交给对账按哈希去核。
func TestJudgeSuccessWithoutTransferIsUnknown(t *testing.T) {
	c := newTestChannel(t)
	cases := []struct {
		desc string
		logs []*types.Log
	}{
		{"没有任何日志", nil},
		{"金额不符", []*types.Log{transferLog(c.token, c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(999_999))}},
		{"收款方不符", []*types.Log{transferLog(c.token, c.sign.Address(), common.HexToAddress(evmChecksumMixed2), big.NewInt(1_000_000))}},
		{"别的合约发的", []*types.Log{transferLog(common.HexToAddress(evmChecksumMixed2), c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(1_000_000))}},
	}
	for _, cs := range cases {
		entry := broadcastEntry(evmAllLower, 1_000_000)
		if err := c.ledger.Put(entry); err != nil {
			t.Fatalf("%s: 台账写入失败: %v", cs.desc, err)
		}
		res, err := c.judge(context.Background(), entry, &types.Receipt{
			Status: types.ReceiptStatusSuccessful, Logs: cs.logs,
		}, time.Now())
		if err != nil {
			t.Errorf("%s: judge 报错: %v", cs.desc, err)
			continue
		}
		if res == nil || res.Status != pay.StatusUnknown {
			t.Errorf("%s: 状态 = %v，期望 UNKNOWN", cs.desc, res)
			continue
		}
		if !strings.Contains(res.RawNote, "Transfer") {
			t.Errorf("%s: 回执没说明为何认不出: %q", cs.desc, res.RawNote)
		}
		stored, _ := c.ledger.Get(entry.OrderNo)
		if stored == nil || stored.State != ledger.StateBroadcast {
			t.Errorf("%s: UNKNOWN 不该落终局: %+v", cs.desc, stored)
		}
	}
}

// TestMatchesTransfer 四个条件逐一比，任何一条不符都不算本单。
func TestMatchesTransfer(t *testing.T) {
	c := newTestChannel(t)
	from := c.sign.Address()
	to := common.HexToAddress(evmAllLower)
	amount := big.NewInt(7_777)

	base := broadcastEntry(evmAllLower, amount.Int64())
	ok := &types.Receipt{Logs: []*types.Log{transferLog(c.token, from, to, amount)}}
	if !c.matchesTransfer(ok, base) {
		t.Fatal("完全匹配的日志未被认出来")
	}

	// 少数代币把 value 也标成 indexed：金额落在第四个 topic 上。
	indexed := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to), common.BytesToHash(common.LeftPadBytes(amount.Bytes(), 32))},
	}}}
	if !c.matchesTransfer(indexed, base) {
		t.Error("value 被 indexed 的日志未被认出来")
	}

	if c.matchesTransfer(nil, base) {
		t.Error("nil 回执被判成匹配")
	}
	short := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from)},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
	}}}
	if c.matchesTransfer(short, base) {
		t.Error("只有两个 topic 的日志被判成匹配")
	}
	noData := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to)},
		Data:    []byte{1, 2, 3},
	}}}
	if c.matchesTransfer(noData, base) {
		t.Error("数据段不足以读出金额的日志被判成匹配")
	}
	// 同一回执里既有别人的事件、也有本单的事件：只要有一条对上就算成功。
	mixed := &types.Receipt{Logs: []*types.Log{
		transferLog(common.HexToAddress(evmChecksumMixed2), from, to, amount),
		transferLog(c.token, from, to, amount),
	}}
	if !c.matchesTransfer(mixed, base) {
		t.Error("混有多条日志时未认出本单那一条")
	}
}

// TestPickTransfer 反查只比金额：收端已由 topic 过滤掉。
func TestPickTransfer(t *testing.T) {
	amount := big.NewInt(4_242)
	logs := []types.Log{
		{Data: common.LeftPadBytes(big.NewInt(1).Bytes(), 32)},
		{Data: common.LeftPadBytes(amount.Bytes(), 32), TxHash: common.HexToHash("0xaa")},
	}
	hit := pickTransfer(logs, amount)
	if hit == nil || hit.TxHash != common.HexToHash("0xaa") {
		t.Fatalf("未命中的金额: %+v", hit)
	}
	four := []types.Log{{
		Topics: []common.Hash{transferEventTopic, {}, {}, common.BytesToHash(common.LeftPadBytes(amount.Bytes(), 32))},
		TxHash: common.HexToHash("0xbb"),
	}}
	if hit := pickTransfer(four, amount); hit == nil || hit.TxHash != common.HexToHash("0xbb") {
		t.Errorf("四 topic 形态未命中: %+v", hit)
	}
	if hit := pickTransfer(logs, big.NewInt(999_999)); hit != nil {
		t.Errorf("不该命中金额 999999: %+v", hit)
	}
	if hit := pickTransfer(nil, amount); hit != nil {
		t.Error("空日志列表命中了")
	}
	if hit := pickTransfer([]types.Log{{Data: []byte{1}}}, big.NewInt(1)); hit != nil {
		t.Error("数据段过短的日志被命中")
	}
}

// TestFeeOf 手续费留痕：缺 EffectiveGasPrice 时给 0，不编数字。
func TestFeeOf(t *testing.T) {
	c := newTestChannel(t)
	if got := c.feeOf(nil); got != 0 {
		t.Errorf("nil 回执的手续费 = %d，期望 0", got)
	}
	if got := c.feeOf(&types.Receipt{GasUsed: 21000}); got != 0 {
		t.Errorf("缺 gas 价的手续费 = %d，期望 0", got)
	}
	if got := c.feeOf(&types.Receipt{GasUsed: 21000, EffectiveGasPrice: big.NewInt(2)}); got != 42000 {
		t.Errorf("手续费 = %d，期望 42000", got)
	}
	// 超出 int64 时饱和而不是溢出成负数。
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	if got := c.feeOf(&types.Receipt{GasUsed: 21000, EffectiveGasPrice: huge}); got <= 0 {
		t.Errorf("超大手续费溢出成 %d", got)
	}
}

// 本文件只钉 BSC 侧的「接线正确性」：装配期「配置不全就静默不启动」，以及注入键名
// 与分派种类同源。门面机器本身（分派登记、重启关旧、订单补全、未启动拒绝）的行为
// 基线在 chainroute/route_test.go 只写一份，四链共用。

// TestNewBscUSDTClientRejectsIncomplete 缺任何一项都返回 ok=false，且不碰网络。
//
// 每一条都对应一种「带着未知上线」的形态；它们共同的特征是第一笔出款才暴露，
// 而那时钱已经开始动。装配期拒掉是唯一来得及的位置。
//
// 最后一例是资金闸而不是缺项：配置写齐了但私钥没注入，同样拒绝。
// 签名者的存在与否不该由「配置看起来完整」来推断。
func TestNewBscUSDTClientRejectsIncomplete(t *testing.T) {
	good := func() *config.BscUsdtConfig {
		return &config.BscUsdtConfig{
			Enable:   "on",
			Driver:   driverBSCRPC,
			Network:  "mainnet",
			Contract: evmChecksumMixed,
			Decimals: 18,
			Chain: &config.BscChainConfig{
				RPC:           "https://example.invalid",
				WalletAddress: evmAllLower,
			},
		}
	}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"nil 配置", nil},
		{"缺 bsc 段", &config.Cfg{}},
		{"缺 bsc.usdt 段", &config.Cfg{Bsc: &config.BscConfig{}}},
		{"enable 不是 on", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Enable = "off"
			return u
		}()}}},
		{"驱动名不符", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Driver = "some_other_driver"
			return u
		}()}}},
		{"合约为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = "  "
			return u
		}()}}},
		{"合约非法", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Contract = evmBogusChecksum
			return u
		}()}}},
		{"网络为空", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Network = ""
			return u
		}()}}},
		{"缺 chain 段", &config.Cfg{Bsc: &config.BscConfig{Usdt: func() *config.BscUsdtConfig {
			u := good()
			u.Chain = nil
			return u
		}()}}},
		{"配置齐备但私钥未注入", &config.Cfg{Bsc: &config.BscConfig{Usdt: good()}}},
	}
	for _, cs := range cases {
		c, ok := newBscUSDTClient(cs.cfg)
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

// TestBscNamespaceKeys 注入键名与分派种类同源：改一处不会漏掉另一处。
func TestBscNamespaceKeys(t *testing.T) {
	if bscUSDTKey != string(pay.KindBscUSDT) {
		t.Errorf("注入键 %q 与分派种类 %q 不同源", bscUSDTKey, pay.KindBscUSDT)
	}
	if !strings.Contains(string(pay.KindBscUSDT), "bsc") {
		t.Errorf("分派种类 %q 里读不出链名", pay.KindBscUSDT)
	}
}

// TestERC20TransferData 手工拼的 ABI 编码逐字节对拍。
//
// 不引 ABI 编码器是刻意的（只调两个函数，引工具链反而多一份私货），代价是
// 选择器或左补零写错时不会有编译器报错——所以这里按字节钉死：
// 前 4 字节选择器、随后 32 字节左补零地址、再 32 字节大端金额。
func TestERC20TransferData(t *testing.T) {
	to := common.HexToAddress(evmChecksumMixed2)
	if got := addressTopic(to); got != common.BytesToHash(common.LeftPadBytes(to.Bytes(), 32)) {
		t.Fatalf("addressTopic 与 32 字节左补零不一致: %s", got.Hex())
	}

	for _, amount := range []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		big.NewInt(1_000_000),
		new(big.Int).SetUint64(^uint64(0)),   // 2^64-1
		new(big.Int).Lsh(big.NewInt(1), 255), // 最高位为 1，左补零不能退化成符号扩展
	} {
		data := erc20TransferData(to, amount)
		if len(data) != 4+32+32 {
			t.Fatalf("金额 %s: 调用数据长度 %d，期望 68", amount, len(data))
		}
		if !bytes.Equal(data[:4], erc20TransferSelector) {
			t.Errorf("金额 %s: 选择器 %x，期望 %x", amount, data[:4], erc20TransferSelector)
		}
		if want := common.LeftPadBytes(to.Bytes(), 32); !bytes.Equal(data[4:36], want) {
			t.Errorf("金额 %s: 地址段 %x，期望 %x", amount, data[4:36], want)
		}
		if want := common.LeftPadBytes(amount.Bytes(), 32); !bytes.Equal(data[36:68], want) {
			t.Errorf("金额 %s: 金额段 %x，期望 %x", amount, data[36:68], want)
		}
	}

	// nil 金额按 0 处理而不是 panic：零金额在 pay.CheckOrder 已经拦过一次，
	// 组包这一层不该因为上游没填就把进程打挂。
	if data := erc20TransferData(to, nil); !bytes.Equal(data[36:], make([]byte, 32)) {
		t.Errorf("nil 金额未按 0 编码: %x", data[36:])
	}

	bal := erc20BalanceOfData(to)
	if len(bal) != 4+32 {
		t.Fatalf("balanceOf 调用数据长度 %d，期望 36", len(bal))
	}
	if !bytes.Equal(bal[:4], erc20BalanceOfSelector) {
		t.Errorf("balanceOf 选择器 %x，期望 %x", bal[:4], erc20BalanceOfSelector)
	}
	if want := common.LeftPadBytes(to.Bytes(), 32); !bytes.Equal(bal[4:], want) {
		t.Errorf("balanceOf 地址段 %x，期望 %x", bal[4:], want)
	}
}

// TestTransferEventTopic 事件 topic0 的黄金值。
//
// 它是查单与反查的锚：算错一个字节，所有「本单的 Transfer 事件」都认不出来，
// 而认不出来的表现是「回执成功但未见 Transfer」——一笔已经成功的钱被报成 UNKNOWN。
func TestTransferEventTopic(t *testing.T) {
	const want = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got := transferEventTopic.Hex(); got != want {
		t.Fatalf("Transfer 事件 topic0 = %s，期望 %s", got, want)
	}
	if !bytes.Equal(erc20DecimalsSelector, []byte{0x31, 0x3c, 0xe5, 0x67}) {
		t.Errorf("decimals() 选择器不符: %x", erc20DecimalsSelector)
	}
}

// TestDecodeUint256 返回值必须恰好是一个 uint256。
//
// 空返回是「这个地址没有合约」的典型表现（节点返回 0x），当成 0 余额会把配置错误
// 报成「余额不足」；超长返回说明那不是标准 ERC-20，读前 32 字节等于拿一段
// 不知道含义的字节当余额。
func TestDecodeUint256(t *testing.T) {
	raw := common.LeftPadBytes(big.NewInt(123456789).Bytes(), 32)
	got, err := decodeUint256(raw)
	if err != nil {
		t.Fatalf("合法返回被拒: %v", err)
	}
	if got.Cmp(big.NewInt(123456789)) != 0 {
		t.Errorf("解出的值 %s，期望 123456789", got)
	}

	if _, err := decodeUint256(nil); err == nil {
		t.Error("空返回未被拒")
	}
	if _, err := decodeUint256([]byte{}); err == nil {
		t.Error("零长返回未被拒")
	}
	for _, n := range []int{1, 31, 33, 64} {
		if _, err := decodeUint256(make([]byte, n)); err == nil {
			t.Errorf("%d 字节返回未被拒（不是单个 uint256）", n)
		}
	}
}

// TestClassifySendError 广播错误的分档。
//
// 分错的方向代价不对称：把「已进池」误判成「没进池」，上游会重建一单（双付）；
// 把「没进池」误判成「说不清」，只是停在 UNKNOWN 等对账。所以只有节点在入池前
// 就拒的那几句进 rejectBeforePool，其余一律 rejectUnknown。
func TestClassifySendError(t *testing.T) {
	cases := []struct {
		err  string
		want rejectClass
	}{
		{"", rejectUnknown},
		{"i/o timeout", rejectUnknown},
		{"connection reset by peer", rejectUnknown},
		{"already known", rejectAlreadyKnown},
		{"known transaction: 0xabc", rejectAlreadyKnown},
		{"transaction already exists", rejectAlreadyKnown},
		{"nonce too low", rejectNonceUsed},
		{"nonce too low: next nonce 8, tx nonce 7", rejectNonceUsed},
		{"insufficient funds for gas * price + value", rejectBeforePool},
		{"intrinsic gas too low", rejectBeforePool},
		{"exceeds block gas limit", rejectBeforePool},
		{"invalid sender", rejectBeforePool},
		{"invalid chain id", rejectBeforePool},
		{"transaction underpriced", rejectBeforePool},
		{"replacement transaction underpriced", rejectBeforePool},
		{"gas price too low", rejectBeforePool},
		{"rlp: value size exceeds available input length", rejectBeforePool},
		{"oversized data", rejectBeforePool},
	}
	for _, cs := range cases {
		var err error
		if cs.err != "" {
			err = errString(cs.err)
		}
		if got := classifySendError(err); got != cs.want {
			t.Errorf("classifySendError(%q)=%v，期望 %v", cs.err, got, cs.want)
		}
	}
	// 大小写不敏感：各家节点的文案大小写不一。
	if got := classifySendError(errString("NONCE TOO LOW")); got != rejectNonceUsed {
		t.Errorf("大写文案未被识别: %v", got)
	}
}

// errString 是最小的 error 实现，避免为了造一个带文案的 error 去引 fmt。
type errString string

func (e errString) Error() string { return string(e) }

// TestIsReadRetryable 读侧只重试「接入点没答上来」，业务性失败不重试。
func TestIsReadRetryable(t *testing.T) {
	retryable := []error{
		errString("i/o timeout"),
		errString("connection refused"),
		errString("unexpected EOF"),
		errString("429 Too Many Requests"),
		context.DeadlineExceeded,
	}
	for _, err := range retryable {
		if !isReadRetryable(err) {
			t.Errorf("%v 应可重试", err)
		}
	}
	no := []error{
		nil,
		errString("execution reverted"),
		errString("invalid argument 0"),
		context.Canceled,
	}
	for _, err := range no {
		if isReadRetryable(err) {
			t.Errorf("%v 不该重试", err)
		}
	}
}

// TestGweiToWei 配置里的 gwei 价换 wei。
func TestGweiToWei(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{-1, "0"},
		{1, "1000000000"},
		{1.5, "1500000000"},
		{3, "3000000000"},
		{0.000000001, "1"},
		{0.1, "100000000"},
	}
	for _, cs := range cases {
		if got := gweiToWei(cs.in).String(); got != cs.want {
			t.Errorf("gweiToWei(%v)=%s，期望 %s", cs.in, got, cs.want)
		}
	}
}

// TestSaturateInt64 收窄大整数：超出即饱和，负数归 0。
func TestSaturateInt64(t *testing.T) {
	max := int64(^uint64(0) >> 1)
	cases := []struct {
		in   *big.Int
		want int64
	}{
		{nil, 0},
		{big.NewInt(0), 0},
		{big.NewInt(-5), 0},
		{big.NewInt(123), 123},
		{big.NewInt(max), max},
		{new(big.Int).Add(big.NewInt(max), big.NewInt(1)), max},
		{new(big.Int).Lsh(big.NewInt(1), 200), max},
	}
	for _, cs := range cases {
		if got := saturateInt64(cs.in); got != cs.want {
			t.Errorf("saturateInt64(%v)=%d，期望 %d", cs.in, got, cs.want)
		}
	}
}

// TestLooksLikeRPCURL 只判形态：写错协议前缀的接入点在装配期就该被拒。
func TestLooksLikeRPCURL(t *testing.T) {
	ok := []string{"http://127.0.0.1:8545", "https://bsc-dataseed.binance.org", "ws://n:1", "wss://n:1", "HTTP://X.Y"}
	for _, s := range ok {
		if !looksLikeRPCURL(s) {
			t.Errorf("%q 应被认作接入点", s)
		}
	}
	no := []string{"", "127.0.0.1:8545", "tcp://x:1", "file:///tmp/x", "bsc-dataseed"}
	for _, s := range no {
		if looksLikeRPCURL(s) {
			t.Errorf("%q 不该被认作接入点", s)
		}
	}
}

// TestBigString nil 大整数给 "0"，免得日志里出现 "<nil>"。
// （摘要截断 clip 已上移为 util.Clip，其用例见 util/string_test.go。）
func TestBigString(t *testing.T) {
	if bigString(nil) != "0" {
		t.Errorf("nil 大整数应给 0，实得 %q", bigString(nil))
	}
	if bigString(big.NewInt(42)) != "42" {
		t.Error("大整数字面量不符")
	}
}

// 固定私钥 → 派生地址的黄金向量。
//
// 私钥取公开测试账户（Hardhat/Anvil 的 #0）：公开、可复算、与任何真实资金无关。
// 地址先由 crypto.PubkeyToAddress 现推一次后逐字钉在这里——用例要验证的是
// 「签名器算出来的地址等于配置地址」，不能让它自己算一个地址再给自己盖章。
const (
	testPrivKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testWallet     = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	// testChainID 是 BSC 主网的 chain id。
	testChainID = 56
)

// mustTestSigner 造一个已通过全部装配闸的签名器。
func mustTestSigner(t *testing.T) *signer {
	t.Helper()
	sgn, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), testWallet)
	if err != nil {
		t.Fatalf("夹具签名器构造失败: %v", err)
	}
	return sgn
}

// TestParsePrivateKey 私钥解析：容忍 0x 前缀与首尾空白，错形态一律拒且不回显原文。
func TestParsePrivateKey(t *testing.T) {
	want := common.HexToAddress(testWallet)
	ok := []struct {
		desc string
		in   string
	}{
		{"裸 hex", testPrivKeyHex},
		{"0x 前缀", "0x" + testPrivKeyHex},
		{"0X 前缀", "0X" + testPrivKeyHex},
		{"大写 hex", strings.ToUpper(testPrivKeyHex)},
		{"首尾空白", "  " + testPrivKeyHex + "\n"},
	}
	for _, cs := range ok {
		key, err := parsePrivateKey(cs.in)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", cs.desc, err)
			continue
		}
		if got := crypto.PubkeyToAddress(key.PublicKey); got != want {
			t.Errorf("%s: 派生地址 %s，期望 %s", cs.desc, got.Hex(), want.Hex())
		}
	}

	bad := []struct {
		desc string
		in   string
	}{
		{"空串", ""},
		{"只有空白", "   "},
		{"只有前缀", "0x"},
		{"少一字节", testPrivKeyHex[:62]},
		{"多一字节", testPrivKeyHex + "00"},
		{"含非十六进制字符", "0x" + testPrivKeyHex[:60] + "zz"},
		{"全零", strings.Repeat("0", 64)},
		{"超出曲线阶", strings.Repeat("ff", 32)},
	}
	for _, cs := range bad {
		if _, err := parsePrivateKey(cs.in); err == nil {
			t.Errorf("%s: 应被拒却解析成功", cs.desc)
		}
	}

	// 报错不得带出私钥原文：私钥不许以任何形式出现在返回值里（它会进日志）。
	_, err := parsePrivateKey("0x" + testPrivKeyHex[:58] + "zz")
	if err == nil {
		t.Fatal("非法私钥未被拒")
	}
	if strings.Contains(err.Error(), testPrivKeyHex[:16]) {
		t.Errorf("错误文案回显了私钥片段: %v", err)
	}
}

// TestNewSignerRequiresMatchingWallet 装配期三道闸：链 ID、私钥、派生地址。
//
// 三者都必须在装配期判死：任何一条留到第一笔出款才暴露，代价都是「钱已经动了一部分」。
func TestNewSignerRequiresMatchingWallet(t *testing.T) {
	if _, err := newSigner(testPrivKeyHex, nil, testWallet); err == nil {
		t.Error("chain_id 为 nil 未被拒")
	}
	if _, err := newSigner(testPrivKeyHex, big.NewInt(0), testWallet); err == nil {
		t.Error("chain_id 为 0 未被拒")
	}
	if _, err := newSigner(testPrivKeyHex, big.NewInt(-56), testWallet); err == nil {
		t.Error("chain_id 为负未被拒")
	}
	if _, err := newSigner("", big.NewInt(testChainID), testWallet); err == nil {
		t.Error("空私钥未被拒")
	}

	// 私钥 A 配地址 B：必须拒，且错误里两个地址都要出现（否则运维不知道该改哪边）。
	_, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), evmChecksumMixed2)
	if err == nil {
		t.Fatal("派生地址与配置地址不等时未被拒")
	}
	if !strings.Contains(err.Error(), "不是同一个账户") {
		t.Errorf("拒因文案不符: %v", err)
	}

	// 地址形态不合法要在「比派生地址」之前就拒：common.HexToAddress 对解不出来的串
	// 会安静地返回零地址，直接比会把它读成「派生地址和它不等」。
	if _, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"); err == nil {
		t.Error("非法 EVM 地址未被拒")
	} else if !strings.Contains(err.Error(), "不是合法的 EVM 地址") {
		t.Errorf("非法地址的拒因文案不符: %v", err)
	}

	sgn := mustTestSigner(t)
	if sgn.Address() != common.HexToAddress(testWallet) {
		t.Errorf("成功构造的签名器地址 %s，期望 %s", sgn.Address().Hex(), testWallet)
	}
	if sgn.chainID.Int64() != testChainID {
		t.Errorf("签名器链 ID = %s，期望 %d", sgn.chainID, testChainID)
	}
}

// TestSignerEIP155 签名必须与链 ID 绑定：本链 ID 可恢复出发送方，换链 ID 恢复不出来。
//
// 只断言「签名没报错」是不够的：EIP-155 的 v 由链 ID 算出，链 ID 写错时本地签名
// 照样成功，而交易会被节点拒掉——或者更糟，在另一条 EVM 链上被接受。
func TestSignerEIP155(t *testing.T) {
	sgn := mustTestSigner(t)
	to := common.HexToAddress(evmAllLower)
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    7,
		GasPrice: big.NewInt(1_000_000_000),
		Gas:      defaultTransferGasLimit,
		To:       &to,
		Value:    big.NewInt(0),
		Data:     erc20TransferData(to, big.NewInt(1_234_567)),
	})
	signed, err := sgn.Sign(tx)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if got := signed.ChainId(); got == nil || got.Int64() != testChainID {
		t.Fatalf("签名后的链 ID = %v，期望 %d", got, testChainID)
	}
	from, err := types.Sender(types.NewEIP155Signer(big.NewInt(testChainID)), signed)
	if err != nil {
		t.Fatalf("按本链 ID 恢复发送方失败: %v", err)
	}
	if from != sgn.Address() {
		t.Errorf("恢复出的发送方 %s，期望 %s", from.Hex(), sgn.Address().Hex())
	}
	// 换一条同构链（Chapel，chain id 97）恢复：v 对不上，必须失败或落在别的地址上。
	if other, err := types.Sender(types.NewEIP155Signer(big.NewInt(97)), signed); err == nil && other == sgn.Address() {
		t.Error("换链 ID 后仍恢复出同一发送方：签名没有真正绑定链 ID")
	}

	// 台账按哈希查证的前提是「同一份 raw tx 重广播得到同一个哈希」：
	// 序列化往返之后哈希必须逐字不变，否则同单号第二次广播会变成另一笔。
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back := new(types.Transaction)
	if err := back.UnmarshalBinary(raw); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if txHash(back) != txHash(signed) {
		t.Errorf("序列化往返后哈希变了: %s → %s", txHash(signed), txHash(back))
	}
	h := txHash(signed)
	if h != strings.ToLower(h) || !strings.HasPrefix(h, "0x") || len(h) != 66 {
		t.Errorf("txHash 形态不符（应为全小写 0x + 64 位）: %s", h)
	}
	if txHash(nil) != "" {
		t.Error("nil 交易的哈希应为空串")
	}
}
