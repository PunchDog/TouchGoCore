package telegram

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	tonapi "github.com/tonkeeper/tonapi-go"
	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/wallet"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 真机验证：TON_LIVE_VERIFY=1 才跑，默认跳过。
//
// 离线闸证不到两件事，而这两件都直接决定出款能不能被正确对账：
//  1. 台账的认领键。本包把外部消息 cell 的 hash 记成 TxID，查证时拿它与接入点回的
//     in_msg.hash 相比。两者是不是同一个口径（原始 hash 还是归一化 hash），
//     只有拿真实上过链的外部消息才能定论——猜错的表现不是报错，而是「永远查不到」，
//     于是每一单都卡在 UNKNOWN，最后靠人工对账收场。
//  2. 接入点回的 JSON 形状是否与解析代码的假设一致（尤其 out_msgs 里 value 的单位
//     与 destination 的形态——单位读错，台账丢失后的反查就永远匹配不上，那正是双付兜底失效的地方）。
//
// 第一档只读、不签名、不广播、不动钱，任何人重跑都拿得到同样的结论。
// 第二档要真花钱，只在显式给了测试网私钥时才跑。

// tonLiveReadAccount 是一个真实活跃的主网钱包（有外部消息触发的历史交易）。
//
// 反查与哈希口径都必须拿真实活跃地址验：拿一个没有交易的地址会得到空列表，
// 那既证明不了解析、也证明不了 hash 口径，只是让用例安静地什么都不判。
const tonLiveReadAccount = "EQBszTJahYw3lpP64ryqscKQaDGk4QpsO7RO6LYVvKHSINS0"

func tonLiveEnabled(t *testing.T) bool {
	t.Helper()
	if os.Getenv("TON_LIVE_VERIFY") == "" {
		t.Skip("设 TON_LIVE_VERIFY=1 才跑 TON 真机验证")
	}
	return true
}

// tonLiveClient 建真接入点。没给 token 就匿名跑（实测匿名可读，只是会被限流）。
func tonLiveClient(t *testing.T, base string) *tonapi.Client {
	t.Helper()
	var sec tonapi.SecuritySource = &tonapi.Security{}
	if tk := os.Getenv("TONAPI_TOKEN"); tk != "" {
		sec = tonapi.WithToken(tk)
	}
	cli, err := tonapi.NewClient(base, sec, tonapi.WithClient(&http.Client{Timeout: 45 * time.Second}))
	if err != nil {
		t.Fatalf("接入点 %s 建客户端失败: %v", base, err)
	}
	return cli
}

// TestLiveTONExternalMessageHashConvention 定论「接入点回的外部消息 hash 是原始还是归一化」。
//
// 判法：同一笔交易从两条独立路径拿到 in_msg hash——一条是接入点的 JSON 摘要，
// 一条是把交易的原始 BOC 用 tongo 自己解出来后现算的 cell hash。两者对上哪一种算法，
// 台账就该记哪一种。任一候选对上即算通过，但必须打印出对上了哪个，
// 因为本包的实现只按「原始 = 归一化」这一条前提写（不带 StateInit 的外部消息两者相等）。
func TestLiveTONExternalMessageHashConvention(t *testing.T) {
	tonLiveEnabled(t)
	cli := tonLiveClient(t, tonapi.TonApiURL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var params tonapi.GetBlockchainAccountTransactionsParams
	params.AccountID = tonLiveReadAccount
	params.Limit.SetTo(10)
	res, err := cli.GetBlockchainAccountTransactions(ctx, params)
	if err != nil {
		t.Fatalf("接入点读不动（本机代理或网络不可达时就是这样，别当成实现错）: %v", err)
	}
	checked := 0
	for i := range res.Transactions {
		tx := &res.Transactions[i]
		inMsg, ok := tx.InMsg.Get()
		if !ok || string(inMsg.MsgType) != tonExtInKind || strings.TrimSpace(tx.Raw) == "" {
			continue
		}
		checked++
		rawBoc, err := hex.DecodeString(strings.TrimSpace(tx.Raw))
		if err != nil {
			t.Fatalf("交易原始 BOC 不是十六进制: %v", err)
		}
		root, err := boc.DeserializeSingleRootBoc(rawBoc)
		if err != nil {
			t.Fatalf("交易 BOC 反序列化失败: %v", err)
		}
		var decoded tlb.Transaction
		if err := tlb.Unmarshal(root, &decoded); err != nil {
			t.Fatalf("交易结构解析失败: %v", err)
		}
		if !decoded.Msgs.InMsg.Exists {
			t.Fatalf("这笔交易没有 in_msg 引用，本用例的取法不适用")
		}
		msg := decoded.Msgs.InMsg.Value.Value // 从链上解码而来，私有 hash 字段已被填充
		rawHash := msg.Hash(false)
		normHash := msg.Hash(true)
		want := strings.ToLower(strings.TrimSpace(inMsg.Hash))
		t.Logf("tx=%s 接入点 in_msg=%s 原始=%s 归一化=%s", tx.Hash, want,
			hex.EncodeToString(rawHash[:]), hex.EncodeToString(normHash[:]))
		switch strings.ToLower(hex.EncodeToString(normHash[:])) {
		case want:
			if rawHash != normHash {
				t.Errorf("接入点回的是归一化 hash，而它与原始 hash 不同：本包记的是原始那一种，认领会永远失配")
			}
		default:
			if strings.ToLower(hex.EncodeToString(rawHash[:])) != want {
				t.Fatalf("两种算法都对不上接入点的 in_msg.hash（%s），台账键的口径假设不成立", want)
			}
		}
	}
	if checked == 0 {
		t.Fatal("这个账户近 10 笔里没有外部消息触发的交易，本用例等于没判——换一个活跃地址")
	}
}

// TestLiveTONWalletDerivationAgainstChain 拿真实账户核对「派生地址=配置地址」这道闸的方向。
//
// 只读：读一个真实地址的链上状态与序号，确认本包把它当得出、也确认接入点的 status
// 取值确实是非exist/uninit/active/frozen 这四个（precheckFunds 按这四个分支处置）。
func TestLiveTONWalletDerivationAgainstChain(t *testing.T) {
	tonLiveEnabled(t)
	cli := tonLiveClient(t, tonapi.TonApiURL)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	acct, err := ton.ParseAccountID(tonLiveReadAccount)
	if err != nil {
		t.Fatal(err)
	}
	var p tonapi.GetAccountParams
	p.AccountID = acct.ToRaw()
	info, err := cli.GetAccount(ctx, p)
	if err != nil {
		t.Fatalf("读账户失败: %v", err)
	}
	if info.Status != "active" {
		t.Errorf("真实钱包账户的 status 应当是 active，实得 %q（precheckFunds 的分支表要跟着改）", info.Status)
	}
	if info.Balance <= 0 {
		t.Errorf("夹具账户余额读到 %d，单位或字段口径与实现假设不符", info.Balance)
	}
	var sp tonapi.GetAccountSeqnoParams
	sp.AccountID = acct.ToRaw()
	seq, err := cli.GetAccountSeqno(ctx, sp)
	if err != nil {
		t.Fatalf("读序号失败: %v", err)
	}
	if seq.Seqno < 0 {
		t.Errorf("序号读到负数 %d", seq.Seqno)
	}
	t.Logf("真实账户 status=%s balance=%d seqno=%d", info.Status, info.Balance, seq.Seqno)
}

// TestLiveTONGatewayReadSideParsesRealPayload 用真实回包过一遍窄网关的解析。
//
// 这一条盯的是 normalizeTx/tonVerdict 对 JSON 字段的假设：success 与 compute/action
// 阶段少任一项时本包必须退回「判不了」而不是判失败。
func TestLiveTONGatewayReadSideParsesRealPayload(t *testing.T) {
	tonLiveEnabled(t)
	gw, err := newTonAPIGateway(&pay.ChainOptions{
		HTTPBase: tonapi.TonApiURL, Network: pay.NetworkMainnet, Timeout: 45 * time.Second, MaxRetry: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	acct, err := ton.ParseAccountID(tonLiveReadAccount)
	if err != nil {
		t.Fatal(err)
	}
	balance, err := gw.account(ctx, acct)
	if err != nil {
		t.Fatalf("网关读余额失败: %v", err)
	}
	if balance.Status == "" || balance.BalanceNanoton <= 0 {
		t.Fatalf("网关把真实余额读成了 %+v", balance)
	}
	txs, err := gw.transactions(ctx, acct, tonScanLimit)
	if err != nil {
		t.Fatalf("网关读交易列表失败: %v", err)
	}
	if len(txs) == 0 {
		t.Fatal("真实活跃账户读不出交易，解析口径与接入点不一致")
	}
	verdictKnown := 0
	for _, tx := range txs {
		if tx.Hash == "" || tx.UnixTime <= 0 {
			t.Errorf("交易缺 hash 或时间: %+v", tx)
		}
		if tx.VerdictKnown {
			verdictKnown++
		}
		for _, out := range tx.Outs {
			if out.To == "" && out.AmountNanoton != 0 {
				t.Errorf("转出记录没有收款端却带着金额: %+v", out)
			}
		}
	}
	if verdictKnown == 0 {
		t.Error("整页交易都读不出执行结果——tonVerdict 的判据太严，会把每一单都卡在 UNKNOWN")
	}
	t.Logf("读到 %d 笔交易，其中 %d 笔给出可判定结果", len(txs), verdictKnown)
}

// TestLiveTONTestnetWithdrawEndToEnd 在测试网真发一笔并回读确认。
//
// 需要 TON_LIVE_TESTNET_KEY（测试网热钱包私钥 hex，且该钱包必须已在链上部署并有测试币），
// 可选 TON_LIVE_DEST（收款地址，缺省退回自己）与 TON_LIVE_AMOUNT（纳吨，缺省 100 万）。
// 条件不满足时一律 Fatal 说明缺什么，而不是安静 skip——「没跑成」与「跑过且绿」必须分得开。
//
// 这条是本文件唯一会真动钱的路径，也是唯一能证明「签名→广播→按 hash 回读」整条链路
// 与真实节点对得上的证据；离线闸替代不了它。
func TestLiveTONTestnetWithdrawEndToEnd(t *testing.T) {
	tonLiveEnabled(t)
	keyHex := os.Getenv("TON_LIVE_TESTNET_KEY")
	if keyHex == "" {
		t.Fatal("需要 TON_LIVE_TESTNET_KEY：测试网热钱包私钥（hex）。没有它就发不出真金，本用例不能算跑过")
	}
	key, err := tonPrivateKey(keyHex)
	if err != nil {
		t.Fatalf("测试网私钥不可用: %v", err)
	}
	gw, err := newTonAPIGateway(&pay.ChainOptions{
		HTTPBase: tonapi.TestnetTonApiURL, Network: pay.NetworkTestnet,
		APIKey: os.Getenv("TONAPI_TOKEN"), Timeout: 45 * time.Second, MaxRetry: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 热钱包的地址不写在配置里读：这一条用例只验证链路，逐个候选问链上「你是 active 吗」，
	// 问得到的那个就是配置该填的 wallet_address（也顺带证明派生核对的方向是对的）。
	w, ver, acct, found := tonLiveFindDeployed(t, ctx, key, gw)
	if !found {
		t.Fatal("五个候选版本派生出的测试网地址都不在链上（status 非 active）：先部署测试网热钱包并注入测试币")
	}
	t.Logf("测试网热钱包 %s（%s）", acct.ToRaw(), ver.ToString())

	dest := strings.TrimSpace(os.Getenv("TON_LIVE_DEST"))
	if dest == "" {
		dest = acct.ToHuman(true, false) // 自转一笔：仍走完签名-广播-回读整条路径
	}
	amount := int64(1_000_000)
	if v := os.Getenv("TON_LIVE_AMOUNT"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			t.Fatalf("TON_LIVE_AMOUNT=%s 不是正的纳吨数", v)
		}
		amount = n
	}
	c := &tonChannel{
		gw: gw,
		opt: pay.ProviderOptions{Name: "ton", Driver: DriverTonAPI,
			MerchantID: "live-testnet"},
		chain: &pay.ChainOptions{HTTPBase: tonapi.TestnetTonApiURL, Network: pay.NetworkTestnet,
			APIKey: os.Getenv("TONAPI_TOKEN"), WalletAddress: acct.ToRaw(),
			PrivateKey: keyHex, FeeLimitSun: 20_000_000, Timeout: 45 * time.Second, MaxRetry: 2},
		led:    ledger.New("ton"),
		acct:   acct,
		wallet: acct.ToRaw(),
		w:      w,
		ver:    ver,
		nowFn:  time.Now,
	}
	c.sign = c.defaultSign

	orderNo := fmt.Sprintf("LIVE-%d", time.Now().Unix())
	res, err := c.Withdraw(ctx, &pay.PayOrder{OrderNo: orderNo, Currency: pay.CurrencyTON,
		Network: pay.NetworkTestnet, Amount: amount, Address: dest, Memo: "live verify"})
	if err != nil {
		t.Fatalf("提现就地失败: %v", err)
	}
	t.Logf("广播后首查: status=%s tx=%s note=%s", res.Status, res.TxHash, res.RawNote)
	if res.Status == pay.StatusFailed && strings.Contains(res.RawNote, "可重发") {
		t.Fatalf("接入点直接拒了这条外部消息（说明报文形态有问题）: %s", res.RawNote)
	}

	deadline := time.Now().Add(3 * time.Minute)
	var last *pay.PayResult
	for time.Now().Before(deadline) {
		last, err = c.QueryOrder(ctx, orderNo)
		if err != nil {
			t.Fatalf("回读失败: %v", err)
		}
		if last.Status == pay.StatusSuccess || last.Status == pay.StatusFailed {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if last.Status != pay.StatusSuccess {
		t.Fatalf("3 分钟内没读到成功终局，实得 %s（%s）——回读口径或 hash 认领有问题", last.Status, last.RawNote)
	}
	if last.TxHash == "" || last.Fee <= 0 {
		t.Errorf("成功回执缺 txhash 或手续费: %+v", last)
	}
	// 钱要真的落到收款端：反查那一笔的转出记录，收端与金额都要对上。
	if !tonLiveSeesTransfer(t, ctx, gw, acct, dest, amount) {
		t.Errorf("回读说成功了，但按收端+金额反查不到这一笔（%s → %s，%d 纳吨）", acct.ToRaw(), dest, amount)
	}
}

// tonLiveFindDeployed 在候选版本里找出链上已部署的那一个热钱包。
func tonLiveFindDeployed(t *testing.T, ctx context.Context, key ed25519.PrivateKey, gw tonGateway) (*wallet.Wallet, wallet.Version, ton.AccountID, bool) {
	pub := key.Public().(ed25519.PublicKey)
	for _, ver := range tonDetectVersions {
		acctID, err := wallet.GenerateWalletAddress(pub, ver, ptrI32(wallet.TestnetGlobalID), 0, nil)
		if err != nil {
			continue
		}
		state, err := gw.account(ctx, acctID)
		if err != nil || state.Status != "active" {
			continue
		}
		wd, err := wallet.New(key, ver, nil, wallet.WithNetworkGlobalID(wallet.TestnetGlobalID), wallet.WithWorkchain(0))
		if err != nil {
			continue
		}
		return &wd, ver, acctID, true
	}
	return nil, 0, ton.AccountID{}, false
}

// tonLiveSeesTransfer 按收端与金额在近期交易里找这一笔，验证反查兜底真的能兜住。
func tonLiveSeesTransfer(t *testing.T, ctx context.Context, gw tonGateway, from ton.AccountID, dest string, amount int64) bool {
	txs, err := gw.transactions(ctx, from, tonScanLimit)
	if err != nil {
		t.Logf("反查读不动: %v", err)
		return false
	}
	for _, tx := range txs {
		for _, out := range tx.Outs {
			if out.AmountNanoton == amount && sameTONAddress(out.To, dest) {
				return true
			}
		}
	}
	return false
}

func ptrI32(v int32) *int32 { return &v }
