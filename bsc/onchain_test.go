package bsc

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"touchgocore/ledger"
	"touchgocore/pay"
)

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
