package ton

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tonkeeper/tongo/ton"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件回答一个问题：这一单号在 TON 链上到底走到哪一步了。
//
// 它是出款链路的另一半：广播只管把钱交出去，结论必须由链上读回。
// 认不出来的情况一律给 UNKNOWN 而不是 FAILED——把「没查到」当成「失败」，
// 上游就会重下一单，而那正是双付；FAILED 只留给链上明确说了不行的情况。
//
// 认领键的口径已用真实链上数据坐实（2026-10-20，主网账户
// 0:6ccd325a858c379693fae2bcaab1c2906831a4e10a6c3bb44ee8b615bca1d220 的 4 笔
// ext_in_msg 交易）：接入点回的 in_msg.hash 与本地「外部消息 cell 的 Hash256」逐字相等，
// 且与 tongo 的归一化 hash 也相等——不带 StateInit、import_fee 为空、body 走引用时，
// 归一化不改形状。所以台账记那一个 hash 就够，不必两个都试。
// 这条证据由只读用例 TestLiveTONExternalMessageHashConvention 复跑，见 live_verify_ton_test.go。
//
// 为什么仍然按账户取交易列表逐笔比，而不是「按 hash 直查交易」：那条路要
// GetBlockchainTransactionByMessageHash，它对 msg_id 吃哪种口径没有文档，
// 一旦口径变了就是静默查不到；按账户翻页的读法多几次请求，但结论可自查。
// 台账没记 seqno 是有意的：读侧现取才是权威，而重发同一条报文由钱包合约按 seqno 去重，
// 记一个会过期的号反而多一处可能说谎的字段。

// tonExtInKind 是接入点给「外部消息」的类型标签（取值逐字来自 TonAPI 的枚举）。
const tonExtInKind = "ext_in_msg"

// confirm 按台账里的外部消息 hash 查证并归一状态。
func (c *tonChannel) confirm(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	txs, err := c.gw.transactions(ctx, c.acct, tonScanLimit)
	if err != nil {
		// 查不动：交回 UNKNOWN 并带上消息 hash，让对账有把手。绝不下沉成 FAILED。
		return c.unknown(entry, "链上查证失败: "+c.redact(err.Error())), nil
	}
	for i := range txs {
		tx := txs[i]
		if !tonTxClaims(tx, entry) {
			continue
		}
		if !tx.VerdictKnown {
			// 进了块，但链上没给出可判定的执行结果：回 UNKNOWN 且不落终局，留着下次再查。
			// 判成 FAILED 的代价是上游重下一单，而链上那一笔可能已经把币转出去了。
			return c.unknown(entry, "链上回执未给出可判定的执行结果 tx="+tx.Hash), nil
		}
		switch {
		case tx.Bounced:
			// 可退回的写法在收款端不可达时会把款项弹回本账户：钱没到手，费已经烧了。
			// 它是终局（重发同一条报文序号已用过），但不是「转出去了」。
			if err := c.settle(entry, "链上终局: bounced"); err != nil {
				return nil, err
			}
			return &pay.PayResult{
				OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
				Status: pay.StatusFailed, Amount: entry.Amount,
				Fee: tx.TotalFeesNanoton, FeeCurrency: pay.CurrencyTON,
				RawNote: c.note("款项已退回本账户（收款端不可达），手续费已消耗 tx=" + tx.Hash),
			}, nil
		case tx.Success:
			res := &pay.PayResult{
				OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
				Status: pay.StatusSuccess, Amount: entry.Amount,
				Fee: tx.TotalFeesNanoton, FeeCurrency: pay.CurrencyTON,
				// 确认数留 nil：TON 没有「链头高度减交易高度」这种直接可读的确认数
				// （出款交易在分片上，不在主链），编一个 0 会被读成「刚进块还没确认」。
				RawNote: c.note("链上成功 tx=" + tx.Hash),
			}
			if err := c.settle(entry, "链上终局: success"); err != nil {
				return nil, err
			}
			return res, nil
		default:
			// 执行未成功（计算阶段被跳过或中止、动作阶段失败）：状态没变，钱没动，
			// 但 gas 照烧。这里给 FAILED 且不再重建——同一条报文重发结论一样，
			// 换一笔就是第二笔钱。
			if err := c.settle(entry, "链上终局: failed"); err != nil {
				return nil, err
			}
			return &pay.PayResult{
				OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
				Status: pay.StatusFailed, Amount: entry.Amount,
				Fee: tx.TotalFeesNanoton, FeeCurrency: pay.CurrencyTON,
				RawNote: c.note("链上执行未成功（gas 已消耗）tx=" + tx.Hash),
			}, nil
		}
	}

	switch {
	case entry.State == ledger.StateRejected:
		// 接入点在投递前就拒过这一笔：钱确定没动，且它不是终局，同单号允许重建重发。
		// 给 FAILED 而不是永远 PENDING——与当初广播那一次收到的结论保持一致。
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount,
			RawNote: c.note("接入点曾拒此笔，未发生资金动作，同单号可重建重发"),
		}, nil

	case entry.Expired(now):
		// 既找不到那一笔、又过了链上有效期。此时只有一种情形能判失败：读回来的交易列表
		// 已经覆盖到了过期时刻之前——否则「这一页里没有」不等于「没有」。
		if tonWindowCovered(txs, entry.ExpiresAtMS) {
			if err := c.settle(entry, "过期未上链"); err != nil {
				return nil, err
			}
			return &pay.PayResult{
				OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
				Status: pay.StatusFailed, Amount: entry.Amount,
				RawNote: c.note("外部消息已过链上有效期且未上链，未发生资金动作"),
			}, nil
		}
		return c.unknown(entry, "已过链上有效期，但读回的交易列表没覆盖到那一刻（可能还有更新的笔压在后面）"), nil

	default:
		// 还没过期、也没查到：可能还在池里。给 PENDING，让上游继续查而不是重下单。
		// 文案按台账状态分：prepared 是签好了还没交出去，把它说成「已交出」
		// 会让运维以为接入点已经见过这一笔。
		note := "广播已交出，链上尚无回执"
		if entry.State == ledger.StatePrepared {
			note = "已签名未广播，链上尚无回执"
		}
		return c.pending(entry, note), nil
	}
}

// tonTxClaims 判这条链上交易是不是由台账那条外部消息触发的。
//
// hash 一律小写比：接入点回的是十六进制串，本包算出来也是小写，但谁也不能保证中间
// 没有一处按大写回。大小写差一位就永远认领不到，症状是「钱已经发出去了但本包说没查到」。
func tonTxClaims(tx tonTx, entry *ledger.Entry) bool {
	if tx.InMsgKind != tonExtInKind {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(entry.TxID))
	got := strings.ToLower(strings.TrimSpace(tx.InMsgHash))
	return want != "" && got != "" && want == got
}

// tonWindowCovered 判读回来的交易列表有没有一直覆盖到 atMS 那一刻之前。
//
// 判的是「列表里最早那一笔的时刻」而不是「有没有回东西」：只有列表已经翻到了
// 有效期之前，才可以说「那一笔不在里面」。拿阈值本身当起点会让这道判据恒真，
// 后果是把还可能在后面页里的单判成失败——上游据此重下一单，那就是双付。
func tonWindowCovered(txs []tonTx, atMS int64) bool {
	if len(txs) == 0 {
		// 一条都没有：热钱包从未有过交易，这一笔当然也没上链。
		return true
	}
	first := true
	var oldest int64
	for _, t := range txs {
		ms := t.UnixTime * 1000
		if first || ms < oldest {
			oldest, first = ms, false
		}
	}
	return oldest <= atMS
}

// pending 给一条「已受理、结果未出」的回执。
func (c *tonChannel) pending(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusPending, Amount: entry.Amount, RawNote: c.note(note),
	}
}

// settle 给这一单打上终局并落账。
//
// 落不进台账就必须报错而不是照样返回结论：终局没记上，下一次同单号的调用就还会
// 走到「查证」以外的分支去，那时它看到的已经不是同一条时间线了。
func (c *tonChannel) settle(entry *ledger.Entry, note string) error {
	entry.State = ledger.StateSettled
	entry.UpdatedAtMS = ledger.NowMS(c.nowFn())
	if err := c.led.Put(entry); err != nil {
		return fmt.Errorf("%w（%s 这一结论没能落账，先不要重发）: %v", ErrJournalUnavailable, note, err)
	}
	return nil
}

// QueryOrder 按单号查链上结论。
//
// 台账里没有这一单号时不给 FAILED 也不去链上瞎猜：本方法只回答「这一单我们知不知道」，
// 不知道就是 UNKNOWN，把结论留给能查到的人（对账或人工）。
//
// 持同单号的台账锁：confirm 里的 settle 会 Put 覆盖台账状态，必须与同一单号的
// Withdraw 串行，否则查证的 settle 与出款的状态迁移会互相盖写。取的是每单台账锁
// （c.led.WithLock），不是全局钱包锁 tonWalletMu——查单是只读路径，不能因为钱包
// 正在给别的单签名就被串行（见 TestQueryOrderDoesNotTakeWalletLock）。
func (c *tonChannel) QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("订单号（幂等键）为空")
	}
	var res *pay.PayResult
	err := c.led.WithLock(orderNo, func() error {
		entry, found := c.led.Get(orderNo)
		if !found {
			res = &pay.PayResult{OrderNo: orderNo, Status: pay.StatusUnknown,
				RawNote: "TON 出款台账没有这一单号（可能是进程重启且未注入持久台账）"}
			return nil
		}
		var e error
		res, e = c.confirm(ctx, entry, c.nowFn())
		return e
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// lookupLedgerByChain 在台账查不到时向链上反查「有没有一笔像本单的转出」。
//
// 返回值是**候选**，不是本单的已落账条目：命中只能证明「本账户向本收端发过
// 同额的一笔」，不能证明那一笔就是这一单（TON 的 memo 也没有参与匹配）。
// 调用方只能据此禁止广播并回 UNKNOWN，绝不能把它写进台账或交给 confirm——
// 否则历史上同址同额的另一笔会被结算成本单的 SUCCESS（F2）。
func (c *tonChannel) lookupLedgerByChain(ctx context.Context, o *pay.PayOrder, now time.Time) (*ledger.Entry, error) {
	txs, err := c.gw.transactions(ctx, c.acct, tonScanLimit)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-tonScanWindow).Unix()
	seen := 0
	for _, tx := range txs {
		if tx.UnixTime < cutoff {
			continue
		}
		seen++
		for _, out := range tx.Outs {
			if out.AmountNanoton != o.Amount || !sameTONAddress(out.To, o.To.Address) {
				continue
			}
			if strings.TrimSpace(tx.InMsgHash) == "" {
				// 认领不上外部消息 hash 的交易连候选都不是：无法给出可对账的 txid。
				continue
			}
			// 只构造候选，不 Put：台账一旦落成 broadcast，后续 confirm 会把
			// 「同收端同金额的任意一笔」读成本单成功的证据。
			return &ledger.Entry{
				OrderNo: strings.TrimSpace(o.OrderNo),
				TxID:    strings.ToLower(strings.TrimSpace(tx.InMsgHash)),
				From:    c.wallet,
				To:      strings.TrimSpace(o.To.Address),
				Amount:  o.Amount,
				// Contract 留空：原生 TON 没有合约地址这一维。
			}, nil
		}
	}
	if len(txs) >= tonScanLimit && seen == len(txs) {
		// 一页取满且全都在窗口内却未命中：窗口里的转出可能还有没读完的页，「没在这一页里」
		// 不等于「没有」。此刻按「查不到」放行广播，赌的就是那一笔压在第 tonScanLimit 条
		// 之后——台账丢失场景下这就是双付，与读不懂同样处理。
		return nil, fmt.Errorf("链上反查取满一页（%d 条）仍在窗口内未命中，无法确认本单是否已上过链（窗口 %s）",
			tonScanLimit, tonScanWindow)
	}
	return nil, nil
}

// 编译期钉子：查证只用本包已定义的账户标识，避免哪天 ton 的类型换了路径悄悄编译不过。
var _ = ton.AccountID{}
