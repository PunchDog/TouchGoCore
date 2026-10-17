package tron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"touchgocore/pay"
)

// 本文件只测一件事：钱会不会被动两次。
//
// 每一个用例都断言「签名次数」和「广播次数」，而不只是返回值：链上出款的正确性
// 不看接口返回了什么状态码，看的是链上最终有几笔以本地址、本单号署名的转账。

const (
	fakeAPIKey  = "FAKE_TRONGRID_API_KEY"
	fakePrivKey = "1111111111111111111111111111111111111111111111111111111111111111"
)

// protoMarshalTx 把交易序列化成台账里那份字节，供「重广播的是不是同一份」比对。
func protoMarshalTx(tx *core.Transaction) ([]byte, error) { return proto.Marshal(tx) }

var testClock = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// fakeGateway 是可编程的链上网关替身，并记录每一类动作的次数。
type fakeGateway struct {
	buildTx    *core.Transaction
	buildErr   error
	buildCalls int

	broadcastRes  *broadcastResult
	broadcastErr  error
	broadcastRaw  [][]byte
	broadcastTxns []*core.Transaction

	receipt     *txReceipt
	receiptErr  error
	lookupCalls int

	tokenBal    int64
	tokenBalErr error
	acct        *accountState
	acctErr     error
	head        int64
	headErr     error

	outbound    []outboundTransfer
	outboundErr error
	closeCalls  int
}

func (f *fakeGateway) buildTransfer(context.Context, string, string, string, int64, int64) (*core.Transaction, error) {
	f.buildCalls++
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	// 每次回一份「新的」交易：真实节点会带上新的 refBlock 与 expiration，
	// 返回同一个指针会让「重建产出的是另一笔」这条断言变成假绿。
	raw := *f.buildTx.GetRawData()
	raw.RefBlockNum += int64(f.buildCalls)
	raw.Expiration += int64(f.buildCalls) * int64(time.Minute/time.Millisecond)
	return &core.Transaction{RawData: &raw}, nil
}

func (f *fakeGateway) broadcast(_ context.Context, tx *core.Transaction) (*broadcastResult, error) {
	raw, err := protoMarshalTx(tx)
	if err != nil {
		return nil, err
	}
	f.broadcastRaw = append(f.broadcastRaw, raw)
	f.broadcastTxns = append(f.broadcastTxns, tx)
	if f.broadcastErr != nil {
		return nil, f.broadcastErr
	}
	if f.broadcastRes != nil {
		return f.broadcastRes, nil
	}
	return &broadcastResult{Code: "SUCCESS", Accepted: true}, nil
}

func (f *fakeGateway) lookupTx(context.Context, string) (*txReceipt, error) {
	f.lookupCalls++
	if f.receiptErr != nil {
		return nil, f.receiptErr
	}
	if f.receipt != nil {
		return f.receipt, nil
	}
	return &txReceipt{}, nil
}

func (f *fakeGateway) tokenBalance(context.Context, string, string) (int64, error) {
	return f.tokenBal, f.tokenBalErr
}

func (f *fakeGateway) account(context.Context, string) (*accountState, error) {
	if f.acctErr != nil {
		return nil, f.acctErr
	}
	if f.acct != nil {
		return f.acct, nil
	}
	return &accountState{BalanceSun: defaultTRC20FeeLimitSun}, nil
}

func (f *fakeGateway) headBlock(context.Context) (int64, error) { return f.head, f.headErr }

func (f *fakeGateway) outboundTransfers(context.Context, string, string, int64) ([]outboundTransfer, error) {
	if f.outboundErr != nil {
		return nil, f.outboundErr
	}
	return f.outbound, nil
}

func (f *fakeGateway) close() error {
	f.closeCalls++
	return nil
}

// counters 记签名次数，是这批用例的核心断言对象。
type counters struct {
	mu    sync.Mutex
	calls int
}

func (c *counters) signer() signFunc {
	return func(tx *core.Transaction) (*core.Transaction, error) {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		// 复刻真实签名的形态：只往 signature 上追加，raw_data 一字不动
		//（txID 因此稳定，「签名后即可确定交易号」这条前提才测得到）。
		out := &core.Transaction{RawData: tx.GetRawData(), Signature: [][]byte{{0xAA}}}
		return out, nil
	}
}

func newTestChannel(gw chainGateway, j Journal, sign signFunc) *onchainChannel {
	// 默认让热钱包「钱够」：本文件测的是出资次数，不是余额门槛——后者单独有用例。
	if f, ok := gw.(*fakeGateway); ok && f.tokenBal == 0 && f.tokenBalErr == nil {
		f.tokenBal = 1 << 50
	}
	led := newLedger()
	if j != nil {
		led.set(j)
	}
	return &onchainChannel{
		gw:     gw,
		sign:   sign,
		opt:    pay.ProviderOptions{Name: "usdt", Driver: DriverTronGrid},
		chain:  &pay.ChainOptions{Node: "grpc.example:50051", WalletAddress: tronRangeAcct, Contract: tronOfficialUSDT, Network: pay.NetworkMainnet, APIKey: fakeAPIKey, PrivateKey: fakePrivKey},
		ledger: led,
		wallet: tronRangeAcct,
		nowFn:  func() time.Time { return testClock },
	}
}

func buildableTx() *core.Transaction {
	return &core.Transaction{RawData: &core.TransactionRaw{
		RefBlockNum: 12345,
		Expiration:  testClock.Add(time.Minute).UnixMilli(),
		Timestamp:   testClock.UnixMilli(),
		FeeLimit:    defaultTRC20FeeLimitSun,
	}}
}

func order() *pay.PayOrder {
	return &pay.PayOrder{OrderNo: "WD-001", Amount: 3_000_000_000, Currency: pay.CurrencyUSDT, Address: tronAllZeroAcct}
}

func sameRaw(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUncertainBroadcastNeverResigns 广播结果未知时，重入同一单号只能查证，不得再签一笔。
func TestUncertainBroadcastNeverResigns(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), broadcastErr: errors.New("rpc error: deadline exceeded")}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("不确定必须给 UNKNOWN 并交出 txid，实得 %+v", res)
	}
	firstTxid := res.TxHash

	for i := 0; i < 3; i++ {
		gw.broadcastErr = errors.New("transport: connection reset")
		again, err := ch.Withdraw(context.Background(), order())
		if err != nil {
			t.Fatal(err)
		}
		if again.TxHash != firstTxid {
			t.Fatalf("第 %d 次重入换了交易号：%s vs %s", i+1, again.TxHash, firstTxid)
		}
	}
	if c.calls != 1 {
		t.Fatalf("同一单号签了 %d 笔，只能 1 笔", c.calls)
	}
	if gw.buildCalls != 1 {
		t.Fatalf("同一单号构造了 %d 次，只能 1 次", gw.buildCalls)
	}
	if len(gw.broadcastRaw) != 1 {
		t.Fatalf("广播了 %d 次，不确定态只能等查证", len(gw.broadcastRaw))
	}
}

// TestPreparedEntryRebroadcastsIdenticalBytes 台账里 prepared（签名完成但没交出去）的条目
// 重入时广播的是逐字相同的那一份，因此节点侧天然去重、不会产生第二笔。
func TestPreparedEntryRebroadcastsIdenticalBytes(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, nil, c.signer())

	tx, err := gw.buildTransfer(context.Background(), ch.wallet, order().Address, ch.chain.Contract, order().Amount, ch.feeLimit())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := ch.prepare(order(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != statePrepared {
		t.Fatalf("新条目应为 prepared，实得 %s", entry.State)
	}
	if _, err := ch.broadcastEntry(context.Background(), entry, testClock); err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 1 || !sameRaw(gw.broadcastRaw[0], entry.RawTx) {
		t.Fatalf("广播的不是台账里那一份: %x / %x", gw.broadcastRaw[0], entry.RawTx)
	}
	// 重入同一单号：已进 stateBroadcast，只能查证，不再广播。
	if _, err := ch.Withdraw(context.Background(), order()); err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("重入后广播 %d 次、签名 %d 次", len(gw.broadcastRaw), c.calls)
	}
}

// TestSettledOrderRefusesSecondAttempt 链上已有终局的单号再被调用时必须拒绝，而不是再出资。
func TestSettledOrderRefusesSecondAttempt(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: true, BlockNumber: 100}, head: 106}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess {
		t.Fatalf("应当成功，实得 %+v", res)
	}
	if res.Confirmations == nil || *res.Confirmations != 6 {
		t.Fatalf("确认数应为 6，实得 %+v", res.Confirmations)
	}
	if res.FeeCurrency != pay.CurrencyTRX {
		t.Fatalf("gas 计价币种应为 TRX，实得 %s", res.FeeCurrency)
	}
	again, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrOrderAlreadySettled) || again != nil {
		t.Fatalf("终局单号再出资必须被拒，实得 res=%v err=%v", again, err)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("终局后又动了钱: 广播 %d 次、签名 %d 次", len(gw.broadcastRaw), c.calls)
	}
}

// TestNodeRejectionAllowsRebuild 节点在入池前拒收时钱一定没动，同单号允许重建。
//
// 这条是「永不签第二笔」的唯一例外，必须显式测出来：把它也一并按死会让所有被拒的单号卡死，
// 而把它放开得太大就是双付。
func TestNodeRejectionAllowsRebuild(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), broadcastRes: &broadcastResult{Code: "TAPOS_ERROR", Rejected: true}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed {
		t.Fatalf("入池前被拒应给 FAILED（钱没动），实得 %+v", res)
	}
	gw.broadcastRes = &broadcastResult{Code: "SUCCESS", Accepted: true}
	if _, err := ch.Withdraw(context.Background(), order()); err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 || len(gw.broadcastRaw) != 2 {
		t.Fatalf("被拒后重入应重建一次: 签名 %d、广播 %d", c.calls, len(gw.broadcastRaw))
	}
	// 两次广播的是不同字节（第二笔是新的 refBlock/expiration），但都只有一笔在链上：
	// 第一笔被节点明确拒于池外，不存在双付。
	if sameRaw(gw.broadcastRaw[0], gw.broadcastRaw[1]) {
		t.Error("重建应当产出新的交易，而不是逐字重发被拒那一笔")
	}
}

// TestPrechecksRunBeforeSigning 余额与 gas 的门槛排在签名之前：一条都不许漏过去。
func TestPrechecksRunBeforeSigning(t *testing.T) {
	cases := map[string]struct {
		gw   *fakeGateway
		want string
	}{
		"代币余额不足":  {&fakeGateway{buildTx: buildableTx(), tokenBal: 1}, "insufficient_token"},
		"读不出代币余额": {&fakeGateway{buildTx: buildableTx(), tokenBalErr: errors.New("boom")}, "balance_unread"},
		"gas 不足":  {&fakeGateway{buildTx: buildableTx(), acct: &accountState{BalanceSun: 1}}, "insufficient_gas"},
		"读不出账户":   {&fakeGateway{buildTx: buildableTx(), acctErr: errors.New("boom")}, "account_unread"},
	}
	for name, cs := range cases {
		c := &counters{}
		ch := newTestChannel(cs.gw, nil, c.signer())
		res, err := ch.Withdraw(context.Background(), order())
		if err == nil || res != nil {
			t.Errorf("%s: 应当拒绝，实得 res=%v err=%v", name, res, err)
			continue
		}
		if !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s: 报错里找不到 %s: %v", name, cs.want, err)
		}
		if c.calls != 0 || cs.gw.buildCalls != 0 || len(cs.gw.broadcastRaw) != 0 {
			t.Errorf("%s: 前置闸没拦住，签名 %d 次、构造 %d 次、广播 %d 次", name, c.calls, cs.gw.buildCalls, len(cs.gw.broadcastRaw))
		}
	}
}

// TestJournalWriteFailureAbortsBroadcast 台账写不进去时不许动钱。
func TestJournalWriteFailureAbortsBroadcast(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, failingJournal{}, c.signer())
	_, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("记账失败必须放弃广播，实得 %v", err)
	}
	if len(gw.broadcastRaw) != 0 || c.calls != 1 {
		t.Fatalf("签名 %d 次、广播 %d 次：应当签完就停在记账前", c.calls, len(gw.broadcastRaw))
	}
}

type failingJournal struct{}

func (failingJournal) Put(*ledgerEntry) error { return errors.New("库挂了") }
func (failingJournal) Get(string) (*ledgerEntry, bool) {
	return nil, false
}

// firstPutFailsJournal 只让第一次记账失败，后面的写入照旧成功。
//
// 存在的必要是把「签名后先记账」这一道单独钉住：出款路径上有两道记账闸
// （prepare 的 prepared 落账、broadcastEntry 的 broadcast 落账），任一道失效另一道都能
// 让「记不上就别动钱」这条不变量仍然成立，于是一把全失败的替身分不出是哪道在起作用——
// 用第一道失败、第二道成功的替身才能把第一道单独杀掉。
type firstPutFailsJournal struct {
	mu   sync.Mutex
	puts int
	seen []*ledgerEntry
}

func (j *firstPutFailsJournal) Put(entry *ledgerEntry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.puts++
	j.seen = append(j.seen, entry)
	if j.puts == 1 {
		return errors.New("第一次落账失败")
	}
	return nil
}

func (j *firstPutFailsJournal) Get(string) (*ledgerEntry, bool) { return nil, false }

// TestPrepareMustBeJournaledBeforeBroadcast 签名完成后落账失败，就不许把这一笔交出去。
func TestPrepareMustBeJournaledBeforeBroadcast(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	j := &firstPutFailsJournal{}
	ch := newTestChannel(gw, j, c.signer())

	_, err := ch.Withdraw(context.Background(), order())
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("第一道记账闸失守时应当报错，实得 %v", err)
	}
	if c.calls != 1 {
		t.Fatalf("签名次数 %d：记账失败前就该只签一次", c.calls)
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatalf("记不上账仍然广播了 %d 次", len(gw.broadcastRaw))
	}
	// 失败的那一次必须是被拒的 prepared 落账，而不是后面那次状态推进。
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.puts != 1 || j.seen[0].State != statePrepared {
		t.Fatalf("应当停在第一次 prepared 落账，实得 puts=%d state=%s", j.puts, j.seen[0].State)
	}
}

// TestRebuildAfterLedgerLossLooksUpChainFirst 台账丢了（重启）时，先按收端+金额反查链上，
// 命中就禁止广播并挂回台账。
func TestRebuildAfterLedgerLossLooksUpChainFirst(t *testing.T) {
	c := &counters{}
	o := order()
	gw := &fakeGateway{buildTx: buildableTx(), outbound: []outboundTransfer{{
		TxID: strings.Repeat("ab", 32), To: o.Address, Amount: o.Amount, TimestampMS: testClock.UnixMilli(),
	}}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatalf("反查命中后仍广播了 %d 次，这就是双付", len(gw.broadcastRaw))
	}
	if c.calls != 0 || gw.buildCalls != 0 {
		t.Fatalf("反查命中后仍组了笔: 签名 %d、构造 %d", c.calls, gw.buildCalls)
	}
	if res.TxHash != gw.outbound[0].TxID {
		t.Fatalf("结果应带回链上那一笔的 txid，实得 %+v", res)
	}
	if res.Status != pay.StatusPending && res.Status != pay.StatusSuccess {
		t.Fatalf("反查命中后应继续查证，实得 %s", res.Status)
	}
	// 命中必须落回台账，否则下一次重入还会再赌一遍。
	if _, found := ch.ledger.get(o.OrderNo); !found {
		t.Error("反查命中的那一笔没挂回台账")
	}
}

// TestReverseLookupFailureAbortsBroadcast 反查读不动时按「不能确认」处理：宁可不发。
func TestReverseLookupFailureAbortsBroadcast(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), outboundErr: errNoTransferListing}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err == nil || res != nil {
		t.Fatalf("反查不可用应当拒绝广播，实得 res=%v err=%v", res, err)
	}
	if !pay.IsProviderRetryable(err) {
		t.Error("这一类失败应标记为可重试（下一次重入会再查一遍）")
	}
	if len(gw.broadcastRaw) != 0 {
		t.Fatal("反查不可用却广播了")
	}
}

// TestReverseLookupTruncatedPageAbortsBroadcast 反查取满一页却没命中时，按「没能查全」
// 拒绝广播：第 outboundLimit 条之后的转出读不到，「没在这一页里」不等于「没有」——
// 台账丢失场景下照发就是赌双付，与读不懂同等处理。
func TestReverseLookupTruncatedPageAbortsBroadcast(t *testing.T) {
	full := make([]outboundTransfer, outboundLimit)
	for i := range full {
		// 每一条都对不上本单（收端与金额都不同），而页面是满的。
		full[i] = outboundTransfer{
			TxID: strings.Repeat("cd", 32), To: "TSomebodyElse", Amount: int64(i + 1),
			TimestampMS: testClock.UnixMilli(),
		}
	}
	gw := &fakeGateway{buildTx: buildableTx(), outbound: full}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err == nil || res != nil {
		t.Fatalf("一页取满且未命中应当拒绝广播，实得 res=%v err=%v", res, err)
	}
	if !pay.IsProviderRetryable(err) {
		t.Error("查不全应标记为可重试（下一次重入会把窗口或分页再查一遍）")
	}
	if len(gw.broadcastRaw) != 0 || c.calls != 0 || gw.buildCalls != 0 {
		t.Fatal("查不全却动了链上")
	}
}

// TestQueryOrderReportsLedgerStateHonestly 查单按台账状态给结论：签了没广播的不能说成
// 「广播已交出」，节点拒过的要给 FAILED（与广播那一次的结论一致）而不是永远 PENDING。
func TestQueryOrderReportsLedgerStateHonestly(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{}}
	ch := newTestChannel(gw, nil, (&counters{}).signer())

	prepared := &ledgerEntry{
		OrderNo: "WD-P", TxID: strings.Repeat("11", 32), State: statePrepared,
		Amount: 1, ExpiresAtMS: testClock.Add(time.Minute).UnixMilli(),
	}
	if err := ch.ledger.put(prepared); err != nil {
		t.Fatal(err)
	}
	res, err := ch.QueryOrder(context.Background(), "WD-P")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending || !strings.Contains(res.RawNote, "未广播") {
		t.Fatalf("prepared 条目的查单结论应说明未广播，实得 %+v", res)
	}

	rejected := &ledgerEntry{OrderNo: "WD-R", TxID: strings.Repeat("22", 32), State: stateRejected, Amount: 1}
	if err := ch.ledger.put(rejected); err != nil {
		t.Fatal(err)
	}
	res, err = ch.QueryOrder(context.Background(), "WD-R")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || !strings.Contains(res.RawNote, "拒") {
		t.Fatalf("rejected 条目的查单结论应为失败且说明节点拒过，实得 %+v", res)
	}
}

// TestConcurrentWithdrawSignsOnce 同单号并发进来时，分段锁必须让第二个人看见第一个人的台账。
func TestConcurrentWithdrawSignsOnce(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx()}
	ch := newTestChannel(gw, nil, c.signer())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ch.Withdraw(context.Background(), order())
		}()
	}
	wg.Wait()
	if c.calls != 1 {
		t.Fatalf("8 个并发调用签了 %d 笔", c.calls)
	}
	if len(gw.broadcastRaw) != 1 {
		t.Fatalf("8 个并发调用广播了 %d 次", len(gw.broadcastRaw))
	}
}

// TestExpiredPendingBroadcastSettlesFailed 过期且链上查不到时给 FAILED：这一笔已不可能再上链。
func TestExpiredPendingBroadcastSettlesFailed(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx()}
	c := &counters{}
	ch := newTestChannel(gw, nil, c.signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusPending {
		t.Fatalf("刚广播完应是 pending，实得 %+v", res)
	}
	ch.nowFn = func() time.Time { return testClock.Add(2 * time.Minute) }
	again, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != pay.StatusFailed {
		t.Fatalf("过期未上链应给 FAILED，实得 %+v", again)
	}
	if len(gw.broadcastRaw) != 1 || c.calls != 1 {
		t.Fatalf("过期后仍动了笔: 广播 %d、签名 %d", len(gw.broadcastRaw), c.calls)
	}
}

// TestReceiptReadFailureGivesUnknown 回执读不动时不能推断成败。
func TestReceiptReadFailureGivesUnknown(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receiptErr: errors.New("node unavailable")}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("查证失败必须给 UNKNOWN 并带 txid，实得 %+v", res)
	}
}

// TestFailedReceiptIsTerminalButGasBurned 进了块却没转成功 → FAILED，且写明 gas 已烧。
func TestFailedReceiptIsTerminalButGasBurned(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{Found: true, InBlock: true, Success: false, VerdictKnown: true, BlockNumber: 10, FeeSun: 13_000_000, ResultCode: "FAILED"}}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || res.Fee != 13_000_000 || !strings.Contains(res.RawNote, "gas 已消耗") {
		t.Fatalf("回执判定为未成功时要把烧掉的手续费一起交出去: %+v", res)
	}
}

// TestUnreadableVerdictIsUnknownNotFailed 进了块但读不出执行结果时，只能给 UNKNOWN。
//
// 这条闸来自真机复现：USDT 的 transfer 不返回 bool，回执里的执行结果字段会随节点
// 版本省略。判成 FAILED 上游就会重下一单，而链上那一笔可能已经把币转出去了——
// 那是业务层的双付，比卡一单贵得多。
func TestUnreadableVerdictIsUnknownNotFailed(t *testing.T) {
	c := &counters{}
	gw := &fakeGateway{buildTx: buildableTx(), receipt: &txReceipt{
		Found: true, InBlock: true, BlockNumber: 10, Success: false, VerdictKnown: false, ResultCode: "/",
	}}
	ch := newTestChannel(gw, nil, c.signer())

	res, err := ch.Withdraw(context.Background(), order())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash == "" {
		t.Fatalf("判定不明必须给 UNKNOWN 并带 txid，实得 %+v", res)
	}
	// 不落终局：等节点把执行结果回填后，同单号再查一次就能翻成成功。
	entry, found := ch.ledger.get(order().OrderNo)
	if !found || entry.terminal() {
		t.Fatalf("判定不明就被打了终局，这一单再也没机会翻案: %+v", entry)
	}
	gw.receipt = &txReceipt{Found: true, InBlock: true, BlockNumber: 10, Success: true, VerdictKnown: true, ResultCode: "SUCCESS"}
	gw.head = 12
	again, err := ch.QueryOrder(context.Background(), order().OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != pay.StatusSuccess {
		t.Fatalf("回执补齐后应翻成成功，实得 %+v", again)
	}
	if c.calls != 1 || len(gw.broadcastRaw) != 1 {
		t.Fatalf("翻案过程中不该再动钱: 签名 %d 次、广播 %d 次", c.calls, len(gw.broadcastRaw))
	}
}

// TestQueryOrderWithoutLedgerStaysUnknown 查单查不到单号时给 UNKNOWN，不给成败结论。
func TestQueryOrderWithoutLedgerStaysUnknown(t *testing.T) {
	ch := newTestChannel(&fakeGateway{}, nil, (&counters{}).signer())
	res, err := ch.QueryOrder(context.Background(), "WD-999")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("台账没有这一单号时应给 UNKNOWN，实得 %+v", res)
	}
	if _, err := ch.QueryOrder(context.Background(), "  "); err == nil {
		t.Fatal("空单号必须报错")
	}
}

// TestWithdrawGatesRunBeforeAnyChainCall 地址格式、memo、币种、网络四道闸都排在链上调用之前。
func TestWithdrawGatesRunBeforeAnyChainCall(t *testing.T) {
	mutate := func(f func(*pay.PayOrder)) *pay.PayOrder {
		o := order()
		f(o)
		return o
	}
	cases := map[string]*pay.PayOrder{
		"收款地址校验和不过": mutate(func(o *pay.PayOrder) { o.Address = tronBogusChecksum }),
		"带了备注":      mutate(func(o *pay.PayOrder) { o.Memo = "order ref" }),
		"网络与通道不符":   mutate(func(o *pay.PayOrder) { o.Network = pay.NetworkShasta }),
		"币种不是 USDT": mutate(func(o *pay.PayOrder) { o.Currency = pay.CurrencyTRX }),
		"金额为 0":     mutate(func(o *pay.PayOrder) { o.Amount = 0 }),
		"收款地址为空":    mutate(func(o *pay.PayOrder) { o.Address = "" }),
	}
	for name, o := range cases {
		gw := &fakeGateway{buildTx: buildableTx()}
		c := &counters{}
		ch := newTestChannel(gw, nil, c.signer())
		if _, err := ch.Withdraw(context.Background(), o); err == nil {
			t.Errorf("%s: 应当被拒", name)
			continue
		}
		if gw.buildCalls != 0 || c.calls != 0 || gw.lookupCalls != 0 {
			t.Errorf("%s: 拒单前碰了链上: build=%d sign=%d lookup=%d", name, gw.buildCalls, c.calls, gw.lookupCalls)
		}
	}
}

// TestNoSecretsInAnyOutcome 私钥与 API Key 不得出现在返回值、错误或回执摘要里。
func TestNoSecretsInAnyOutcome(t *testing.T) {
	gw := &fakeGateway{buildTx: buildableTx(), broadcastErr: fmt.Errorf("dial %s failed: connection refused", fakeAPIKey)}
	ch := newTestChannel(gw, nil, (&counters{}).signer())
	res, err := ch.Withdraw(context.Background(), order())
	for _, s := range []string{fakePrivKey, fakeAPIKey} {
		if err != nil && strings.Contains(err.Error(), s) {
			t.Errorf("错误文案含敏感值 %s: %v", s, err)
		}
		if res != nil && strings.Contains(res.RawNote, s) {
			t.Errorf("RawNote 含敏感值 %s: %s", s, res.RawNote)
		}
	}
	if res == nil {
		t.Fatal("广播不确定也要给出回执，好让对账有 txid 可用")
	}
}

// TestDriverRegisteredUnderOwnName 驱动名登记在本包，配置换个 driver 就换资金路径。
func TestDriverRegisteredUnderOwnName(t *testing.T) {
	found := false
	for _, d := range pay.Drivers() {
		if d == DriverTronGrid {
			found = true
		}
	}
	if !found {
		t.Fatalf("驱动 %s 未登记，可用驱动: %v", DriverTronGrid, pay.Drivers())
	}
	if _, err := pay.OpenDriver(DriverTronGrid, pay.ProviderOptions{Name: "usdt"}); err == nil {
		t.Fatal("缺 chain 选项时驱动必须拒绝装配，不能返回半成品通道")
	}
}
