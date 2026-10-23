package ton

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonkeeper/tongo/ton"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件只测一件事：钱会不会被动两次。
//
// 每个用例都断言「签名次数」和「广播次数」，而不只是返回值：链上出款的正确性
// 不在「这一单返回了什么」，而在「说不清的时候本包有没有再去签第二笔」。
// 被测的网关是假的——把广播打成超时、打成 406、打成都读不出结果，这些形态
// 对着真链是没法稳定复现的，而这恰恰是最需要被固定的几支。

const (
	tonFakeAPIKey = "tonapi-key-should-never-appear-in-any-note"
	tonFakeMemo   = "8890"
)

// tonTestClock 是本文件统一的时间原点，让「过期/未过期」这两支可判而不是靠跑多快。
var tonTestClock = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// fakeTONGateway 是可以逐条指定结论的假接入点。
type fakeTONGateway struct {
	mu sync.Mutex

	balance  int64
	status   string
	acctErr  error
	seqnoVal uint32
	seqnoErr error

	sends    [][]byte
	sendResp *tonBroadcast
	sendErr  error

	txs     []tonTx
	txErr   error
	txCalls int

	closed bool
}

func (f *fakeTONGateway) seqno(context.Context, ton.AccountID) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seqnoVal, f.seqnoErr
}

func (f *fakeTONGateway) account(context.Context, ton.AccountID) (*tonAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acctErr != nil {
		return nil, f.acctErr
	}
	status := f.status
	if status == "" {
		status = "active"
	}
	if f.balance == 0 {
		f.balance = 1 << 40 // 默认「钱够」：本文件测的是出资次数，余额另有专门用例
	}
	return &tonAccount{BalanceNanoton: f.balance, Status: status}, nil
}

func (f *fakeTONGateway) sendExtMessage(_ context.Context, payload []byte) (*tonBroadcast, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]byte(nil), payload...)
	f.sends = append(f.sends, cp)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	if f.sendResp != nil {
		return f.sendResp, nil
	}
	return &tonBroadcast{Code: "200", Accepted: true}, nil
}

func (f *fakeTONGateway) transactions(context.Context, ton.AccountID, int) ([]tonTx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txCalls++
	if f.txErr != nil {
		return nil, f.txErr
	}
	return f.txs, nil
}

func (f *fakeTONGateway) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTONGateway) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// uncertainBroadcast 是「请求可能已经到达网络」那一类：超时、5xx、限流都归它。
func uncertain() *tonBroadcast {
	return &tonBroadcast{Code: "transport: dial tcp timeout", Uncertain: true}
}

// rejectedBroadcast 复刻实测拿到的那条：接入点在投递之前就拒了这条外部消息。
func rejected() *tonBroadcast {
	return &tonBroadcast{Code: "406 lite server error, code 0: cannot apply external message to current state", Rejected: true}
}

// tonSignCounter 包住真实签名，用来数「这一单号到底签了几次」并留住 bounce 位。
type tonSignCounter struct {
	mu      sync.Mutex
	calls   int
	bounces []bool
	seqnos  []uint32
}

func (c *tonSignCounter) wrap(next tonSignFunc) tonSignFunc {
	return func(to ton.AccountID, o *pay.PayOrder, bounce bool, seqno uint32, now time.Time) (*tonPrepared, error) {
		c.mu.Lock()
		c.calls++
		c.bounces = append(c.bounces, bounce)
		c.seqnos = append(c.seqnos, seqno)
		c.mu.Unlock()
		return next(to, o, bounce, seqno, now)
	}
}

func (c *tonSignCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newTestTONChannel 装一条只对着假网关的通道：私钥、地址、版本都是真算出来的，
// 只有「链上发生了什么」是假的——那正是本文件要控制的唯一变量。
func newTestTONChannel(t *testing.T, gw tonGateway, j ledger.Journal) (*tonChannel, *tonSignCounter) {
	t.Helper()
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", pay.NetworkMainnet, nil)
	if err != nil {
		t.Fatal(err)
	}
	led := ledger.New("ton")
	if j != nil {
		led.SetJournal(j)
	}
	c := &tonChannel{
		gw:  gw,
		opt: channelOptions{Name: "ton", Driver: DriverTonAPI},
		chain: &chainConfig{
			HTTPBase:      "https://tonapi.invalid",
			APIKey:        tonFakeAPIKey,
			WalletAddress: tonDeriveAddrV5R1,
			PrivateKey:    hex.EncodeToString(tonTestSeed()),
			Network:       pay.NetworkMainnet,
		},
		led:    led,
		acct:   w.GetAddress(),
		wallet: tonDeriveAddrV5R1,
		w:      w,
		ver:    ver,
		nowFn:  func() time.Time { return tonTestClock },
	}
	counters := &tonSignCounter{}
	c.sign = counters.wrap(c.defaultSign)
	return c, counters
}

func newTONOrder(no string) *pay.PayOrder {
	return &pay.PayOrder{
		OrderNo:  no,
		Currency: pay.CurrencyTON,
		Amount:   1_000_000_000,
		To: &pay.Party{
			Kind:    pay.PartyWallet,
			Network: pay.NetworkMainnet,
			Address: tonAcctBounceable,
			Memo:    tonFakeMemo,
		},
	}
}

// TestUncertainBroadcastNeverGetsASignature 广播结果说不清时，同单号再调用一次也不许签第二笔。
//
// 这是全仓最贵的一支：接入点超时不等于「没发出去」，把它当成失败去重下单，
// 链上那第一笔可能已经把币转出去了。
func TestUncertainBroadcastNeverGetsASignature(t *testing.T) {
	gw := &fakeTONGateway{sendErr: errors.New("dial tcp: i/o timeout")}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-1")

	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatalf("第一次提现应给出回执而不是报错: %v", err)
	}
	if n := sign.count(); n != 1 {
		t.Fatalf("第一次应当只签一笔，实得 %d", n)
	}
	res, err := c.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatalf("第二次调用不应报错: %v", err)
	}
	if n := sign.count(); n != 1 {
		t.Fatalf("广播不确定后同单号又签了一笔（%d 次）——那就是第二次出资", n)
	}
	if gw.sendCount() != 1 {
		t.Fatalf("广播 %d 次：不确定态只能查证，不能重发", gw.sendCount())
	}
	if res.Status == pay.StatusFailed {
		t.Errorf("「没查到」被下沉成了 FAILED，上游据此就会重下一单: %+v", res)
	}
	if res.TxHash == "" {
		t.Error("不确定回执必须带上外部消息 hash，否则对账没有把手")
	}
}

// TestPreparedEntryIsRebroadcastNotResigned 签好了但一条也没交出去时（进程在两次落账之间被杀掉
// 就是这一态），同单号重入必须逐字复用台账里的字节，而不是再签一笔。
func TestPreparedEntryIsRebroadcastNotResigned(t *testing.T) {
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-2")
	prepared, err := c.sign(mustTONAccount(t, o.To.Address), o, true, 5, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	// 手工摆出一条 prepared 条目：等价于「签好并落账，但还没走到广播」。
	if err := c.led.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc, To: o.To.Address,
		Amount: o.Amount, FeeLimit: c.feeReserve(), State: ledger.StatePrepared,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := sign.count(); n != 1 {
		t.Errorf("prepared 未过期时应当复用已签好的那一笔，却又走了 %d 次签名路径", n)
	}
	if gw.sendCount() != 1 {
		t.Fatalf("应当把台账里那一笔交出去，实得广播 %d 次", gw.sendCount())
	}
	if string(gw.sends[0]) != string(prepared.Boc) {
		t.Error("广播的字节与台账里记的那笔不是同一份：重发必须逐字复用 RawTx")
	}
	entry, _ := c.led.Get(o.OrderNo)
	if entry.TxID != prepared.MsgHash {
		t.Errorf("复用重发之后台账里的 hash 变了（%s → %s），那对账就追不到原先那一笔", prepared.MsgHash, entry.TxID)
	}
}

// TestRejectedBroadcastAllowsRebuild 接入点在投递前拒了（实测 406 那一类）时，
// 钱确定没动，同单号允许重签一笔新的——这是 rejected 不是终局。
func TestRejectedBroadcastAllowsRebuild(t *testing.T) {
	gw := &fakeTONGateway{sendResp: rejected()}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-3")

	res, err := c.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed {
		t.Errorf("被拒的一笔应给可重发的 FAILED，实得 %s", res.Status)
	}
	entry, _ := c.led.Get(o.OrderNo)
	if entry.State != ledger.StateRejected {
		t.Errorf("被拒不该记成终局，实得 %s", entry.State)
	}
	gw.sendResp = &tonBroadcast{Code: "200", Accepted: true}
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if sign.count() != 2 {
		t.Errorf("确定没进池的单号应当可以重建，实得签名 %d 次", sign.count())
	}
	if gw.sendCount() != 2 {
		t.Errorf("广播次数应随重建到 2，实得 %d", gw.sendCount())
	}
}

// TestSettledOrderRefusesSecondSpend 已终局的单号再调用必须报 ErrAlreadySettled 且一动不发。
func TestSettledOrderRefusesSecondSpend(t *testing.T) {
	o := newTONOrder("WD-4")
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	prepared, err := c.sign(mustTONAccount(t, o.To.Address), o, true, 4, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{
		Hash: strings.Repeat("cd", 32), UnixTime: tonTestClock.Unix(),
		InMsgHash: prepared.MsgHash, InMsgKind: tonExtInKind, Success: true, VerdictKnown: true,
		TotalFeesNanoton: 5_000_000,
		Outs:             []tonOut{{To: o.To.Address, AmountNanoton: o.Amount}},
	}}
	if err := c.led.Put(&ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}); err != nil {
		t.Fatal(err)
	}

	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess || res.Fee != 5_000_000 || res.FeeCurrency != pay.CurrencyTON {
		t.Fatalf("链上成功要带手续费与币种，实得 %+v", res)
	}
	if res.TxHash != prepared.MsgHash {
		t.Errorf("成功回执带的必须是被认领的那条消息 hash，实得 %s", res.TxHash)
	}
	if res.Confirmations != nil {
		t.Errorf("TON 侧没有可直接读出的确认数，编造一个 %d 会被上游当成已足够安全", *res.Confirmations)
	}
	if _, err := c.Withdraw(context.Background(), o); !errors.Is(err, ledger.ErrAlreadySettled) {
		t.Errorf("终局后再调用应报已结算，实得 %v", err)
	}
	if sign.count() != 1 || gw.sendCount() != 0 {
		t.Errorf("终局后又动了笔：签名 %d 次、广播 %d 次", sign.count(), gw.sendCount())
	}
}

// TestUnreadableVerdictStaysUnknown 进了块但执行阶段读不出来时必须停在 UNKNOWN 且不落终局。
//
// 判成 FAILED 的下一秒就是上游重下一单，而链上那一笔可能已经把币转出去了。
func TestUnreadableVerdictStaysUnknown(t *testing.T) {
	o := newTONOrder("WD-5")
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	prepared, err := c.sign(mustTONAccount(t, tonAcctBounceable), o, true, 9, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	entry := &ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: tonTestClock.Add(time.Minute).UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}
	if err := c.led.Put(entry); err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{Hash: strings.Repeat("ee", 32), UnixTime: tonTestClock.Unix(),
		InMsgHash: prepared.MsgHash, InMsgKind: tonExtInKind, VerdictKnown: false}}

	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("读不出执行结果必须给 UNKNOWN，实得 %s", res.Status)
	}
	got, _ := c.led.Get(o.OrderNo)
	if got.Terminal() {
		t.Error("读不出结果就被打了终局，这一单再也没机会翻案")
	}
	if sign.count() != 1 {
		t.Errorf("查证路径上不该再签，实得 %d 次", sign.count())
	}
}

// TestExpiryRulingNeedsCoverage 「查不到 + 已过期」只有在读回的列表覆盖到那一刻之前时才判失败。
func TestExpiryRulingNeedsCoverage(t *testing.T) {
	for _, cs := range []struct {
		name string
		txs  []tonTx
		want string
	}{
		{"列表覆盖到过期之前", []tonTx{{Hash: "t1", UnixTime: tonTestClock.Add(-time.Hour).Unix(),
			InMsgHash: "other", InMsgKind: tonExtInKind}}, pay.StatusFailed},
		{"列表一条都没有", []tonTx{}, pay.StatusFailed},
		{"列表没覆盖到那一刻", []tonTx{{Hash: "t2", UnixTime: tonTestClock.Unix(),
			InMsgHash: "other", InMsgKind: tonExtInKind}}, pay.StatusUnknown},
	} {
		gw := &fakeTONGateway{txs: cs.txs}
		c, _ := newTestTONChannel(t, gw, nil)
		o := newTONOrder("WD-" + cs.name)
		entry := &ledger.Entry{OrderNo: o.OrderNo, TxID: strings.Repeat("11", 32),
			RawTx: []byte("boc"), To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
			ExpiresAtMS: tonTestClock.Add(-time.Minute).UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}
		if err := c.led.Put(entry); err != nil {
			t.Fatal(err)
		}
		res, err := c.QueryOrder(context.Background(), o.OrderNo)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != cs.want {
			t.Errorf("%s：期望 %s，实得 %s（%s）", cs.name, cs.want, res.Status, res.RawNote)
		}
	}
}

// TestBouncedTransferIsFailedButSettled 可退回写法打到不可达账户会整笔弹回：钱没到手，费照烧。
func TestBouncedTransferIsFailedButSettled(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-bounce")
	prepared, err := c.sign(mustTONAccount(t, tonAcctBounceable), o, true, 1, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{Hash: "tx-b", UnixTime: tonTestClock.Unix(), InMsgHash: prepared.MsgHash,
		InMsgKind: tonExtInKind, Success: true, VerdictKnown: true, Bounced: true, TotalFeesNanoton: 7}}
	if err := c.led.Put(&ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}); err != nil {
		t.Fatal(err)
	}
	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || !strings.Contains(res.RawNote, "退回") {
		t.Fatalf("弹回的一笔应说明钱已退回，实得 %+v", res)
	}
	got, _ := c.led.Get(o.OrderNo)
	if !got.Terminal() {
		t.Error("弹回是终局：重发同一条报文序号已用过，不落终局就会一直挂着等一个不会来的结论")
	}
}

// failingJournal 一次也写不进去：此时必须放弃动钱。
type failingJournal struct{}

func (failingJournal) Put(*ledger.Entry) error { return errors.New("库挂了") }
func (failingJournal) Get(string, string) (*ledger.Entry, bool) {
	return nil, false
}

// TestJournalWriteFailureAbortsBroadcast 台账记不上就不广播——宁可这一单不发。
func TestJournalWriteFailureAbortsBroadcast(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, failingJournal{})
	_, err := c.Withdraw(context.Background(), newTONOrder("WD-6"))
	if !errors.Is(err, ledger.ErrJournalUnavailable) {
		t.Fatalf("记账失败应报台账不可用，实得 %v", err)
	}
	if gw.sendCount() != 0 {
		t.Errorf("没记上台账却把钱交出去了（广播 %d 次）", gw.sendCount())
	}
	// 签名允许发生一次：它在本机、且那一条字节随进程一起丢掉，没有任何路径会把它再交出去
	// （要广播必须先记账）。这一口径与 tron 的链上通道逐字一致，见 tron/double_pay_test.go。
}

// firstPutFailsJournal 只让第一次记账失败：把 prepare 那道闸单独钉住。
type firstPutFailsJournal struct {
	mu   sync.Mutex
	puts int
	seen []*ledger.Entry
}

func (j *firstPutFailsJournal) Put(entry *ledger.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.puts++
	j.seen = append(j.seen, entry)
	if j.puts == 1 {
		return errors.New("第一次落账失败")
	}
	return nil
}

func (j *firstPutFailsJournal) Get(string, string) (*ledger.Entry, bool) { return nil, false }

func TestPrepareMustBeJournaledBeforeBroadcast(t *testing.T) {
	gw := &fakeTONGateway{}
	j := &firstPutFailsJournal{}
	c, _ := newTestTONChannel(t, gw, j)
	_, err := c.Withdraw(context.Background(), newTONOrder("WD-7"))
	if !errors.Is(err, ledger.ErrJournalUnavailable) {
		t.Fatalf("应报台账不可用，实得 %v", err)
	}
	if gw.sendCount() != 0 {
		t.Errorf("签名后先记账那道闸失效了：广播 %d 次", gw.sendCount())
	}
	j.mu.Lock()
	puts, state := j.puts, ledger.State("")
	if len(j.seen) > 0 {
		state = j.seen[0].State
	}
	j.mu.Unlock()
	if puts != 1 || state != ledger.StatePrepared {
		t.Errorf("第一次落账应当是 prepared 那一道，实得 puts=%d state=%s", puts, state)
	}
}

// TestPrecheckBlocksBeforeSign 余额不足、账户未部署、序号读不出时都不该动笔。
func TestPrecheckBlocksBeforeSign(t *testing.T) {
	cases := []struct {
		name  string
		gw    *fakeTONGateway
		want  string
		probe int64
	}{
		{"余额不足", &fakeTONGateway{balance: 1_000_000_000 + 1}, "insufficient", 0},
		{"钱包未部署", &fakeTONGateway{status: "uninit"}, "尚未部署", 0},
		{"钱包冻结", &fakeTONGateway{status: "frozen"}, "不可出资", 0},
		{"序号读不出", &fakeTONGateway{seqnoErr: errors.New("seqno unavailable")}, "", 0},
	}
	for _, cs := range cases {
		c, sign := newTestTONChannel(t, cs.gw, nil)
		_, err := c.Withdraw(context.Background(), newTONOrder("WD-pre-"+cs.name))
		if err == nil {
			t.Errorf("%s：前置闸没拦住", cs.name)
			continue
		}
		if cs.want != "" && !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s：报错文案该指向原因，实得 %v", cs.name, err)
		}
		if sign.count() != 0 || cs.gw.sendCount() != 0 {
			t.Errorf("%s：前置拒单却在后面动了笔（签名 %d、广播 %d）", cs.name, sign.count(), cs.gw.sendCount())
		}
	}
	// 刚好等于金额也不行：TON 上出款币与 gas 币同一种，余额要留出手续费余量。
	gw := &fakeTONGateway{balance: 1_000_000_000}
	c, _ := newTestTONChannel(t, gw, nil)
	if _, err := c.Withdraw(context.Background(), newTONOrder("WD-exact")); err == nil ||
		!strings.Contains(err.Error(), "不足以支付本单") {
		t.Errorf("余额刚好等于本单金额时应当拒，实得 %v", err)
	}
}

// TestBounceFlagComesFromOurParser EQ 与 UQ 两种形态必须落成不同的可退回位。
//
// 这一条防的是 tongo 根包那个 `bytes[0]&0x11 == 0x11` 的判定：它把 UQ 也算成可退回，
// 于是「同一交易所给的两种写法」会变成两条不同的链上报文。
func TestBounceFlagComesFromOurParser(t *testing.T) {
	gw := &fakeTONGateway{}
	c, counters := newTestTONChannel(t, gw, nil)
	for _, cs := range []struct {
		addr string
		want bool
		desc string
	}{
		{tonAcctBounceable, true, "EQ 可退回"},
		{tonAcctNonBounce, false, "UQ 不可退回"},
		{tonAcctRaw, true, "原始形态按可退回处理"},
	} {
		counters.mu.Lock()
		counters.bounces = counters.bounces[:0]
		counters.mu.Unlock()
		o := newTONOrder("WD-bounce-" + cs.desc)
		o.To.Address = cs.addr
		if _, err := c.Withdraw(context.Background(), o); err != nil {
			t.Fatalf("%s：提现失败 %v", cs.desc, err)
		}
		counters.mu.Lock()
		got := append([]bool(nil), counters.bounces...)
		counters.mu.Unlock()
		if len(got) != 1 || got[0] != cs.want {
			t.Errorf("%s：签出去的可退回位是 %v，期望 %v", cs.desc, got, cs.want)
		}
	}
}

// TestReverseLookupBlocksBroadcastAfterJournalLoss 台账丢了先反查：命中就禁止广播。
//
// 这是进程内默认台账重启后的兜底。反查不动或取满一页没命中时同样不许发——
// 「查不到」在这里不等于「没有」。
func TestReverseLookupBlocksBroadcastAfterJournalLoss(t *testing.T) {
	o := newTONOrder("WD-8")
	t0 := tonTestClock.Unix()

	t.Run("命中就不广播", func(t *testing.T) {
		gw := &fakeTONGateway{txs: []tonTx{{
			Hash: "tx-hit", UnixTime: t0 - 60, InMsgHash: strings.Repeat("77", 32), InMsgKind: tonExtInKind,
			Success: true, VerdictKnown: true,
			Outs: []tonOut{{To: tonAcctBounceable, AmountNanoton: o.Amount}},
		}}}
		c, sign := newTestTONChannel(t, gw, nil)
		res, err := c.Withdraw(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if gw.sendCount() != 0 {
			t.Errorf("链上已经有同收端同金额的一笔，还是把它交出去了（广播 %d 次）", gw.sendCount())
		}
		if sign.count() != 0 {
			t.Errorf("反查命中后又签了一笔（%d 次）", sign.count())
		}
		if res.Status == pay.StatusFailed {
			t.Errorf("已经在链上的那一笔不该被判失败: %+v", res)
		}
		if _, found := c.led.Get(o.OrderNo); !found {
			t.Error("反查命中的那一笔没挂回台账，下一次重入还会再赌一遍")
		}
	})

	t.Run("读不动时放弃广播", func(t *testing.T) {
		gw := &fakeTONGateway{txErr: errors.New("接入点无响应")}
		c, sign := newTestTONChannel(t, gw, nil)
		_, err := c.Withdraw(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "无法确认") {
			t.Fatalf("反查读不动时应放弃广播并说明原因，实得 %v", err)
		}
		if gw.sendCount() != 0 || sign.count() != 0 {
			t.Error("反查读不动却还是动了笔")
		}
	})

	t.Run("取满一页仍未命中按查不到处理", func(t *testing.T) {
		full := make([]tonTx, 0, tonScanLimit)
		for i := 0; i < tonScanLimit; i++ {
			full = append(full, tonTx{Hash: "tx" + string(rune('a'+i%26)), UnixTime: t0 - int64(i),
				InMsgHash: strings.Repeat("9", 32), InMsgKind: tonExtInKind,
				Outs: []tonOut{{To: tonAcctBounceable, AmountNanoton: 1}}})
		}
		gw := &fakeTONGateway{txs: full}
		c, _ := newTestTONChannel(t, gw, nil)
		if _, err := c.Withdraw(context.Background(), o); err == nil ||
			!strings.Contains(err.Error(), "取满一页") {
			t.Fatalf("窗口内取满一页未命中时应报无法确认，实得 %v", err)
		}
		if gw.sendCount() != 0 {
			t.Error("那一笔可能压在后面没读到的页里，这时广播就是双付")
		}
	})
}

// TestSecretsNeverLeakIntoResults 对外摘要里不许出现私钥与接入点凭证。
//
// RawNote 是按「可以公开给日志」设计的字段，而链上错误会原样进来。
func TestSecretsNeverLeakIntoResults(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-9")
	// 先正常走一次，让台账里有一条 broadcast 态的笔；再把读侧换成带凭证的错误原文。
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	gw.txErr = errors.New("GET https://tonapi.invalid: 401 unauthorized, header Authorization: Bearer " + tonFakeAPIKey)
	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatalf("查证失败应给 UNKNOWN 而不是报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("读侧故障必须停在 UNKNOWN，实得 %s", res.Status)
	}
	for _, leak := range []string{tonFakeAPIKey, hex.EncodeToString(tonTestSeed())} {
		if strings.Contains(res.RawNote, leak) || strings.Contains(err2String(err), leak) {
			t.Errorf("凭证泄漏进对外字段: %s", leak)
		}
	}
	if !strings.Contains(res.RawNote, "***") {
		t.Errorf("凭证原文被抹除时应当留下 *** 标记，实得摘要：%s", res.RawNote)
	}
	if len([]rune(res.RawNote)) > 160 {
		t.Errorf("摘要没截断，长度 %d", len([]rune(res.RawNote)))
	}

	// 广播那一路同样不许泄漏：接入点拒绝时的原文里常带请求头回显。
	gw2 := &fakeTONGateway{sendResp: &tonBroadcast{
		Code:     "406 header Authorization: Bearer " + tonFakeAPIKey + " rejected",
		Rejected: true}}
	c2, _ := newTestTONChannel(t, gw2, nil)
	res2, err := c2.Withdraw(context.Background(), newTONOrder("WD-9b"))
	if err != nil {
		t.Fatalf("被拒的单应给可重发的 FAILED 而不是报错: %v", err)
	}
	if strings.Contains(res2.RawNote, tonFakeAPIKey) {
		t.Errorf("被拒摘要里带着凭证: %s", res2.RawNote)
	}
	if !strings.Contains(res2.RawNote, "***") {
		t.Errorf("被拒摘要应留下抹除标记: %s", res2.RawNote)
	}
	// 反查失败的原因要能看见（两种「查不到」是两种处置），但同样不许带凭证。
	gw3 := &fakeTONGateway{txErr: errors.New("取满一页未命中；Bearer " + tonFakeAPIKey)}
	c3, _ := newTestTONChannel(t, gw3, nil)
	if _, err := c3.Withdraw(context.Background(), newTONOrder("WD-9c")); err == nil ||
		!strings.Contains(err.Error(), "取满一页") || strings.Contains(err.Error(), tonFakeAPIKey) {
		t.Errorf("反查失败应把原因交出去且不泄漏凭证，实得 %v", err)
	}
}

func err2String(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestQueryOrderUnknownWhenNoLedger 台账没这一单号时给 UNKNOWN，不去链上瞎猜也不给 FAILED。
func TestQueryOrderUnknownWhenNoLedger(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	res, err := c.QueryOrder(context.Background(), "WD-none")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash != "" {
		t.Fatalf("没这一单号时应给 UNKNOWN 且不给 hash，实得 %+v", res)
	}
	if _, err := c.QueryOrder(context.Background(), "  "); err == nil {
		t.Error("空单号应直接报错")
	}
}

// TestWithdrawRejectsWrongAssetAndNetwork 币种不是 TON、网络与本通道不符时一律拒。
func TestWithdrawRejectsWrongAssetAndNetwork(t *testing.T) {
	for _, cs := range []struct {
		name string
		mut  func(*pay.PayOrder)
		want string
	}{
		{"发 USDT 的单", func(o *pay.PayOrder) { o.Currency = pay.CurrencyUSDT }, "只发原生 TON"},
		{"跨到测试网", func(o *pay.PayOrder) { o.To.Network = pay.NetworkTestnet; o.To.Address = tonAcctTestnet }, "换网就是换币"},
		{"测试网地址配主网单", func(o *pay.PayOrder) { o.To.Address = tonAcctTestnet }, "测试网地址"},
		{"金额为零", func(o *pay.PayOrder) { o.Amount = 0 }, "金额"},
		{"地址为空", func(o *pay.PayOrder) { o.To.Address = "" }, "地址"},
		{"备注越界", func(o *pay.PayOrder) { o.To.Memo = strings.Repeat("x", maxMemoRunes+1) }, "备注"},
		{"备注带换行", func(o *pay.PayOrder) { o.To.Memo = "a\nb" }, "控制字符"},
	} {
		gw := &fakeTONGateway{}
		c, sign := newTestTONChannel(t, gw, nil)
		o := newTONOrder("WD-bad-" + cs.name)
		cs.mut(o)
		if _, err := c.Withdraw(context.Background(), o); err == nil || !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s：应被拒且报错含「%s」，实得 %v", cs.name, cs.want, err)
		}
		if sign.count() != 0 || gw.sendCount() != 0 {
			t.Errorf("%s：被拒的单还是动了笔（签名 %d、广播 %d）", cs.name, sign.count(), gw.sendCount())
		}
	}
}

// TestQueryAccountReadsChainBalance 供应商账户查询已删除，账户门面读默认热钱包链上余额。
func TestQueryAccountReadsChainBalance(t *testing.T) {
	const balance int64 = 100_000_000
	c, _ := newTestTONChannel(t, &fakeTONGateway{balance: balance, status: "active"}, nil)
	info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
	if err != nil {
		t.Fatalf("链上余额查询失败: %v", err)
	}
	if info.Balance != balance {
		t.Errorf("余额=%d，期望 %d", info.Balance, balance)
	}
	if want := balance - defaultTONFeeReserveNanoton; info.Available != want {
		t.Errorf("可动用=%d，期望扣掉手续费余量后的 %d", info.Available, want)
	}
	if info.Status != pay.AcctActive {
		t.Errorf("状态=%s，期望 active", info.Status)
	}
	if info.Currency != pay.CurrencyTON || strings.TrimSpace(info.MerchantID) == "" {
		t.Errorf("账户快照缺币种/地址: %+v", info)
	}
	if c.Driver() != DriverTonAPI {
		t.Errorf("Driver 应回登记名，实得 %s", c.Driver())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustTONAccount(t *testing.T, s string) ton.AccountID {
	t.Helper()
	id, ok := tonAccountID(s)
	if !ok {
		t.Fatalf("地址 %s 解不出账户", s)
	}
	return id
}

// TestTonWithdrawRejectsAmountUnits TON 同样只消费 int64 最小单位，字符串金额必须显式拒绝。
func TestTonWithdrawRejectsAmountUnits(t *testing.T) {
	c := &tonChannel{}
	_, err := c.Withdraw(context.Background(), &pay.PayOrder{
		OrderNo: "O-UNITS", AmountUnits: "1000000",
		To: &pay.Party{Kind: pay.PartyWallet, Address: "TAddr1"},
	})
	if err == nil || !strings.Contains(err.Error(), "amount_units") {
		t.Fatalf("TON 未拒绝 AmountUnits: %v", err)
	}
}
