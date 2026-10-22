package tron

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/address"
	sdkkeys "github.com/fbsobreira/gotron-sdk/pkg/keys"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件钉住「按单出款参数」与「链上余额查询」两条新口径。
//
// whatapp 的多线路热钱包不在配置里：每一单的 From 携带 地址+私钥+合约+网络。
// 这里要证明三件事：解析出的资产确实是 From 那一份；同一单号不得换钱包/换币；
// 余额查询读的是查询指定的地址/合约，而不是配置默认值。

const perOrderPrivKey = "2222222222222222222222222222222222222222222222222222222222222222"

func perOrderWallet(t *testing.T) string {
	t.Helper()
	key, err := sdkkeys.GetPrivateKeyFromHex(perOrderPrivKey)
	if err != nil {
		t.Fatalf("测试私钥不可解析: %v", err)
	}
	return address.BTCECPrivkeyToAddress(key).String()
}

func perOrderChannel(gw chainGateway) *onchainChannel {
	if f, ok := gw.(*fakeGateway); ok && f.tokenBal == 0 && f.tokenBalErr == nil {
		f.tokenBal = 1 << 50
	}
	return &onchainChannel{
		gw:     gw,
		sign:   (&counters{}).signer(),
		opt:    channelOptions{Name: "usdt", Driver: DriverTronGrid},
		chain:  &chainConfig{Node: "grpc.example:50051", WalletAddress: tronRangeAcct, Contract: tronOfficialUSDT, Network: pay.NetworkMainnet, APIKey: fakeAPIKey, PrivateKey: fakePrivKey},
		ledger: ledger.New("tron"),
		wallet: tronRangeAcct,
		nowFn:  func() time.Time { return testClock },
	}
}

func TestAssetOfPrefersPerOrderParams(t *testing.T) {
	ch := perOrderChannel(&fakeGateway{})
	// 按单网络是这一组的主变量；配置网络留空，避免交叉校验挡住覆盖行为。
	ch.chain.Network = ""
	wallet := perOrderWallet(t)

	asset, err := ch.assetOf(&pay.PayOrder{
		OrderNo: "O1", Amount: 1,
		To: &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{
			Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT,
			Network: "shasta", PrivateKey: pay.Secret(perOrderPrivKey),
		},
	})
	if err != nil {
		t.Fatalf("按单参数解析失败: %v", err)
	}
	if asset.wallet != wallet || asset.contract != tronOfficialUSDT || asset.network != "shasta" || asset.sign == nil {
		t.Fatalf("按单参数没有覆盖配置缺省值: %+v", asset)
	}
	if asset.secret != perOrderPrivKey {
		t.Fatal("用于脱敏的按单私钥没有带进 asset")
	}

	// 私钥与 From.Address 不匹配：必须在签名前拒。
	_, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O2", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: tronRangeAcct, PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err == nil || !strings.Contains(err.Error(), "不是同一个账户") {
		t.Fatalf("私钥与地址不匹配应在签名前拒，实得 %v", err)
	}

	// 只给地址、不给私钥：只有配置热钱包能签。
	asset, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O3", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: tronRangeAcct},
	})
	if err != nil || asset.wallet != tronRangeAcct || asset.sign == nil {
		t.Fatalf("From 只给配置热钱包地址时应沿用配置私钥: asset=%+v err=%v", asset, err)
	}
	_, err = ch.assetOf(&pay.PayOrder{
		OrderNo: "O4", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet},
	})
	if err == nil || !strings.Contains(err.Error(), "无法签名") {
		t.Fatalf("From 指定非配置钱包却无按单私钥时应拒，实得 %v", err)
	}

	// 旧库把 trc20 填在 chain_network 里：按空处理，回落配置网络。
	mainnetCh := perOrderChannel(&fakeGateway{})
	asset, err = mainnetCh.assetOf(&pay.PayOrder{
		OrderNo: "O5", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT, Network: "TRC20", PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err != nil || asset.network != pay.NetworkMainnet {
		t.Fatalf("代币标准名不应被当成网络: asset=%+v err=%v", asset, err)
	}

	// 配置节点属于主网、订单写着 shasta：必须在签名前拒，不能把测试网合约发进主网节点。
	_, err = mainnetCh.assetOf(&pay.PayOrder{
		OrderNo: "O6", Amount: 1,
		To:   &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT, Network: "shasta", PrivateKey: pay.Secret(perOrderPrivKey)},
	})
	if err == nil || !strings.Contains(err.Error(), "换网就是换币") {
		t.Fatalf("配置网络与按单网络不一致应拒，实得 %v", err)
	}
}

func TestWithdrawPinsFromAndContract(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: true, BlockNumber: 100}, head: 106}
	ch := perOrderChannel(gw)
	wallet := perOrderWallet(t)
	base := &pay.PayOrder{
		OrderNo: "WD-PIN-1", Amount: 1, Currency: pay.CurrencyUSDT,
		To: &pay.Party{Kind: pay.PartyWallet, Address: tronAllZeroAcct},
		From: &pay.Party{
			Kind: pay.PartyWallet, Address: wallet, Contract: tronOfficialUSDT,
			PrivateKey: pay.Secret(perOrderPrivKey),
		},
	}
	if _, err := ch.Withdraw(context.Background(), base); err != nil {
		t.Fatalf("按单私钥出款失败: %v", err)
	}
	entry, found := ch.ledger.Get(base.OrderNo)
	if !found || entry.From != wallet || entry.Contract != tronOfficialUSDT {
		t.Fatalf("台账没有钉住按单资产: %+v found=%v", entry, found)
	}

	// 同一单号换钱包：即使第一个参数都合法，也必须在签第二笔之前拒。
	other := *base
	other.From = &pay.Party{
		Kind: pay.PartyWallet, Address: usdtTestWallet, Contract: tronOfficialUSDT,
		PrivateKey: pay.Secret(fakePrivKey),
	}
	if _, err := ch.Withdraw(context.Background(), &other); err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同一单号换钱包必须拒，实得 %v", err)
	}

	// 同一单号换合约：拒。
	other = *base
	other.From = &pay.Party{
		Kind: pay.PartyWallet, Address: wallet, Contract: tronAllZeroAcct,
		PrivateKey: pay.Secret(perOrderPrivKey),
	}
	if _, err := ch.Withdraw(context.Background(), &other); err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同一单号换合约必须拒，实得 %v", err)
	}
}

func TestQueryOnchainAccountReadsRequestedAsset(t *testing.T) {
	gw := &fakeGateway{tokenBal: 123, acct: &accountState{BalanceSun: 456}}
	ch := perOrderChannel(gw)
	info, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{
		Address: tronAllZeroAcct, Contract: tronOfficialUSDT, Currency: pay.CurrencyUSDT,
	})
	if err != nil {
		t.Fatalf("链上余额查询失败: %v", err)
	}
	if info.Available != 123 || info.Balance != 123 || info.Status != pay.AcctActive {
		t.Fatalf("链上余额不符: %+v", info)
	}
	if !strings.Contains(info.RawNote, "456") {
		t.Fatalf("gas 余额没有进摘要: %q", info.RawNote)
	}

	if _, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: "not-a-tron-address"}); err == nil {
		t.Fatal("非法地址应被拒")
	}
	if _, err := ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Currency: pay.CurrencyTON}); err == nil {
		t.Fatal("非 USDT 币种应被拒")
	}

	gw.tokenBalErr = context.DeadlineExceeded
	_, err = ch.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: tronAllZeroAcct, Contract: tronOfficialUSDT})
	pe, ok := pay.ProviderErrorOf(err)
	if !ok || pe.Code != "balance_unread" || !pe.Retryable {
		t.Fatalf("余额读失败应为可重试 ProviderError，实得 %v", err)
	}
}
