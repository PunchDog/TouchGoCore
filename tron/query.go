package tron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件回答一个问题：这一单号在链上到底走到哪一步了。
//
// 它是出款链路的另一半：广播只管把钱交出去，结论必须由按 txid 的查证给出。
// 认不出来的情况一律给 UNKNOWN 而不是 FAILED——把「没查到」当成「失败」，
// 上游就会重下一单，而那正是双付；FAILED 只留给链上明确说了不行的情况。

// confirm 按台账里的 txid 查证并归一状态。
func (c *onchainChannel) confirm(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	rec, err := c.gw.lookupTx(ctx, entry.TxID)
	if err != nil {
		// 查不动：交回 UNKNOWN 并带上 txid，让对账有把手。绝不下沉成 FAILED。
		return c.unknown(entry, "链上查证失败: "+c.redact(err.Error())), nil
	}
	switch {
	case rec.Found && rec.InBlock && rec.Success:
		confs, err := c.confirmations(ctx, rec.BlockNumber)
		hasConfs := err == nil
		if err != nil {
			// 已经确认成功，只是确认数读不出：给 nil 而不是编一个 0（口径见 PayResult.Confirmations）。
			confs = 0
		}
		res := &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusSuccess, Amount: entry.Amount, Fee: rec.FeeSun,
			FeeCurrency: pay.CurrencyTRX, RawNote: c.note("链上成功 result=" + rec.ResultCode),
		}
		if hasConfs {
			// 已进块且读到了确认数：0 也要显式回 &0，否则上游会把 nil 当成
			// 「没有确认数概念」直接 success，required_confs 形同虚设。
			v := confs
			res.Confirmations = &v
		}
		if err := c.settle(entry, "链上终局: success"); err != nil {
			return nil, err
		}
		return res, nil

	case rec.Found && rec.InBlock && !rec.VerdictKnown:
		// 进了块，但链上没给出可判定的执行结果：回 UNKNOWN 且不落终局，留着下次再查。
		//
		// 判成 FAILED 的代价是上游重下一单，而链上那一笔可能已经把币转出去了。
		// 真机复现过这层风险：USDT 的 transfer 不返回 bool，回执里的执行结果字段
		// 会随节点版本省略，读不到不等于失败。
		return c.unknown(entry, "链上回执未给出可判定的执行结果 result="+rec.ResultCode), nil

	case rec.Found && rec.InBlock:
		// 进了块却没转成功：能量照烧，钱没动。这里给 FAILED 且不再重建——
		// 同一份 raw tx 重广播结论一样，而换一笔就是第二笔钱。
		if err := c.settle(entry, "链上终局: failed"); err != nil {
			return nil, err
		}
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount, Fee: rec.FeeSun,
			FeeCurrency: pay.CurrencyTRX, RawNote: c.note("链上执行未成功 result=" + rec.ResultCode + "（gas 已消耗）"),
		}, nil

	case rec.Found:
		// 有回执却没区块号：节点侧的中间态，按未确认处理。
		return c.pending(entry, "已受理未确认 result="+rec.ResultCode), nil

	case entry.State == ledger.StateRejected:
		// 节点在入池前就拒过这一笔（签名/TAPOS/过期/超限）：钱确定没动，且它不是终局，
		// 同单号允许重建重发（见 withdrawLocked）。给 FAILED 而不是永远 PENDING——
		// 与当初广播那一次收到的结论保持一致，上游据此走重建而不是继续干等。
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount,
			RawNote: c.note("节点曾拒此笔入池，未发生资金动作，同单号可重建重发"),
		}, nil

	case entry.Expired(now):
		// 既查不到回执、又过了链上有效期：这一笔不可能再上链，钱确定没动。
		if err := c.settle(entry, "过期未上链"); err != nil {
			return nil, err
		}
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount,
			RawNote: c.note("交易已过链上有效期且未上链，未发生资金动作"),
		}, nil

	default:
		// 没进块也没过期：可能还在池里。给 PENDING，让上游继续查而不是重下单。
		// 文案按台账状态分：prepared 是签好了还没交出去，把它说成「广播已交出」
		// 会让运维以为节点已经见过这一笔。（rejected 在上面已有专门分支。）
		note := "广播已交出，链上尚无回执"
		if entry.State == ledger.StatePrepared {
			note = "已签名未广播，链上尚无回执"
		}
		return c.pending(entry, note), nil
	}
}

// pending 给一条「已受理、结果未出」的回执。
func (c *onchainChannel) pending(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusPending, Amount: entry.Amount, RawNote: c.note(note),
	}
}

// confirmations 把区块高换算成确认数（链头高度 - 交易所在高度）。
//
// 读不到链头时返回错误而不猜一个数：确认数是上游决定「要不要放款给对方」的依据，
// 猜出来的 0 会被读成「刚进块还没确认」，猜出来的大数会被读成「早已安全」。
func (c *onchainChannel) confirmations(ctx context.Context, blockNumber int64) (int64, error) {
	head, err := c.gw.headBlock(ctx)
	if err != nil {
		return 0, err
	}
	if head <= blockNumber {
		return 0, nil
	}
	return head - blockNumber, nil
}

// settle 给这一单打上终局并落账。
//
// 落不进台账就必须报错而不是照样返回结论：终局没记上，下一次同单号的调用就还会
// 走到「查证」以外的分支去，那时它看到的已经不是同一条时间线了。
func (c *onchainChannel) settle(entry *ledger.Entry, note string) error {
	entry.State = ledger.StateSettled
	entry.UpdatedAtMS = ledger.NowMS(c.nowFn())
	if err := c.ledger.Put(entry); err != nil {
		return fmt.Errorf("%w（%s 这一结论没能落账，先不要重发）: %v", ErrJournalUnavailable, note, err)
	}
	return nil
}

// QueryOrder 按单号查链上结论。
//
// 台账里没有这一单号时不给 FAILED 也不去链上瞎猜：本方法只回答「这一单我们知不知道」，
// 不知道就是 UNKNOWN，把结论留给能查到的人（对账或人工）。
//
// 全程持同单号的台账锁：confirm 里的 settle 会 Put 覆盖台账状态，若与同一单号的
// Withdraw（同样持 WithLock）并发，两边会互相盖写——查证的 settle 可能把 Withdraw
// 刚落的 broadcast 覆盖，或反过来。取同一把锁把「查这一单」与「发这一单」串行化。
func (c *onchainChannel) QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("订单号（幂等键）为空")
	}
	var res *pay.PayResult
	err := c.ledger.WithLock(orderNo, func() error {
		entry, found := c.ledger.Get(orderNo)
		if !found {
			res = &pay.PayResult{OrderNo: orderNo, Status: pay.StatusUnknown,
				RawNote: "出款台账没有这一单号（可能是进程重启且未注入持久台账）"}
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
// 返回值是**候选**，不是本单的已落账条目：命中只能证明「本地址向本收端发过
// 同额同合约的一笔」，不能证明那一笔就是这一单。调用方只能据此禁止广播并回
// UNKNOWN，绝不能把它写进台账或交给 confirm——否则历史上同址同额的另一笔会被
// 结算成本单的 SUCCESS（F2）。真正跨重启的幂等靠下游注入持久 journal。
func (c *onchainChannel) lookupLedgerByChain(ctx context.Context, o *pay.PayOrder, a orderAsset, now time.Time) (*ledger.Entry, error) {
	hits, err := c.gw.outboundTransfers(ctx, a.wallet, a.contract, now.Add(-scanWindow).UnixMilli())
	if err != nil {
		// 反查读不动（没配 http_base 时就是这样）：按「不能确认」处理。
		return nil, err
	}
	for _, h := range hits {
		if !strings.EqualFold(strings.TrimSpace(h.To), strings.TrimSpace(o.To.Address)) || h.Amount != o.Amount {
			continue
		}
		if strings.TrimSpace(h.TxID) == "" {
			continue
		}
		// 只构造候选，不 Put：台账一旦落成 broadcast，后续 confirm 会把
		// 「同址同额的任意一笔」读成本单成功的证据。
		return &ledger.Entry{
			OrderNo: strings.TrimSpace(o.OrderNo), TxID: strings.TrimSpace(h.TxID),
			From: a.wallet, To: strings.TrimSpace(o.To.Address), Contract: a.contract, Amount: o.Amount,
		}, nil
	}
	if len(hits) >= outboundLimit {
		// 一页取满却未命中：窗口内的转出可能还有没读完的页，「没在这一页里」
		// 不等于「没有」。此刻按「查不到」放行广播，赌的就是那一笔压在第
		// outboundLimit 条之后——台账丢失场景下这就是双付，与读不懂同样处理。
		return nil, fmt.Errorf("链上反查取满一页（%d 条）仍未命中，无法确认本单是否已上过链（窗口 %s）",
			outboundLimit, scanWindow)
	}
	return nil, nil
}
