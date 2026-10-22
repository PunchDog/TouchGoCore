package bsc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件回答一个问题：这一单号在 BSC 上到底走到哪一步了。
//
// 它是出款链路的另一半：广播只管把钱交出去，结论必须由按哈希的查证给出。
// 认不出来的情况一律给 UNKNOWN 而不是 FAILED——把「没查到」当成「失败」，
// 上游就会重下一单，而那正是双付；FAILED 只留给链上明确说了不行的情况。
//
// EVM 比 TRON 多一条确定性判据：交易靠账户 nonce 排序。「回执查不到」时再看两件事——
// 这笔交易还在不在内存池里、这个 nonce 位有没有被别人占掉——就能把「还在路上」
// 与「永远不会上链」分开，而不必让它永远挂在 PENDING。

// logResultCap 是单次 eth_getLogs 的结果条数上限（服务商的常见配额）。
//
// 取满一页说明这一段的日志可能被截断，「没在这一页里」不等于「没有」——
// 反查时遇到这种情况必须报错（拒绝广播），不能放行。
const logResultCap = 10_000

// confirm 按台账里的哈希查证并归一状态。
func (c *onchainChannel) confirm(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	rec, err := c.rpc.Receipt(ctx, common.HexToHash(entry.TxID))
	switch {
	case err == nil && rec != nil:
		return c.judge(ctx, entry, rec, now)
	case err != nil && !errors.Is(err, ethereum.NotFound):
		// 查不动：交回 UNKNOWN 并带上哈希，让对账有把手。绝不下沉成 FAILED。
		return c.unknown(entry, "链上查证失败: "+c.redact(err.Error())), nil
	}
	// 回执还没有：先用「在不在池里 + nonce 有没有被占」判它是否已经不可能上链。
	if dead, err := c.deadByNonce(ctx, entry); err != nil {
		return c.pending(entry, "尚无回执，且读不出账户 nonce: "+c.redact(err.Error())), nil
	} else if dead != nil {
		return dead, nil
	}
	note := "广播已交出，链上尚无回执"
	if entry.State == ledger.StatePrepared {
		note = "已签名未广播，链上尚无回执"
	}
	return c.pending(entry, note), nil
}

// judge 按回执归一：状态码 + Transfer 事件双判。
//
// 只看 receipt.Status 是不够的：ERC-20 的 transfer 在余额不足时有两种实现——
// 抛错（status=0）或返回 false（status=1 但没发 Transfer 事件）。后者在许多老代币上
// 真实存在，把它读成「成功」就是账实不符，读成「失败」又可能已经动过钱。
// 因此成功必须同时满足「status=1」与「本合约、本钱包、本收款方、本金额的 Transfer 事件」。
func (c *onchainChannel) judge(ctx context.Context, entry *ledger.Entry, rec *types.Receipt, now time.Time) (*pay.PayResult, error) {
	if rec.Status == types.ReceiptStatusFailed {
		// 进块但执行失败：gas 照烧，钱没动。终局，不重建——
		// 同一份 raw tx 重广播结论一样，而换一笔就是第二笔钱。
		if err := c.settle(entry, "链上终局: failed"); err != nil {
			return nil, err
		}
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount, AmountUnits: entry.AmountUnits,
			Fee: c.feeOf(rec), FeeCurrency: pay.CurrencyBNB,
			RawNote: c.note("链上执行失败 status=0（gas 已消耗）"),
		}, nil
	}
	if !c.matchesTransfer(rec, entry) {
		// status=1 却找不到本单的 Transfer 事件：说不清这笔到底做了什么。
		// 不落终局、不判成功——留给对账按哈希去核，绝不给上游一个「可以重下」的结论。
		return c.unknown(entry, "回执成功但未见本单的 Transfer 事件（可能该代币返回 false 而未 revert）"), nil
	}
	confs := int64(0)
	hasBlock := rec.BlockNumber != nil
	if hasBlock {
		if head, err := c.rpc.Head(ctx); err == nil && head > rec.BlockNumber.Uint64() {
			confs = int64(head - rec.BlockNumber.Uint64())
		}
	}
	res := &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusSuccess, Amount: entry.Amount, AmountUnits: entry.AmountUnits,
		Fee: c.feeOf(rec), FeeCurrency: pay.CurrencyBNB,
		RawNote: c.note("链上成功 status=1"),
	}
	if hasBlock {
		// 已进块但确认数为 0 也必须显式回 0：nil 在上游是「没有确认数概念」，
		// 会被当成 success 直接放行，required_confs 形同虚设。
		v := confs
		res.Confirmations = &v
	}
	if err := c.settle(entry, "链上终局: success"); err != nil {
		return nil, err
	}
	return res, nil
}

// matchesTransfer 判断回执里有没有「本合约、本钱包、本收款方、本金额」的 Transfer 事件。
//
// 四个条件逐一比，不靠「有一条 Transfer 就算」：同一个回执里可能既有别的合约的事件
// （代理合约转发）、也有本合约在别的调用里发出的转账，只认条数就是给自己留一条假成功。
func (c *onchainChannel) matchesTransfer(rec *types.Receipt, entry *ledger.Entry) bool {
	if rec == nil {
		return false
	}
	wallet, err := c.entryWallet(entry)
	if err != nil {
		return false
	}
	token := c.token
	if v := strings.TrimSpace(entry.Contract); v != "" {
		if !IsValidEVMAddress(v) {
			return false
		}
		token = common.HexToAddress(v)
	}
	from := addressTopic(wallet)
	to := addressTopic(common.HexToAddress(entry.To))
	want, err := entryAmount(entry)
	if err != nil {
		return false
	}
	for _, lg := range rec.Logs {
		if lg == nil || lg.Address != token {
			continue
		}
		if len(lg.Topics) < 3 || lg.Topics[0] != transferEventTopic {
			continue
		}
		if lg.Topics[1] != from || lg.Topics[2] != to {
			continue
		}
		var amount *big.Int
		switch {
		case len(lg.Topics) >= 4:
			// 少数代币把 value 也标成 indexed：金额落在第四个 topic 上。
			amount = new(big.Int).SetBytes(lg.Topics[3].Bytes())
		case len(lg.Data) >= 32:
			amount = new(big.Int).SetBytes(lg.Data[:32])
		default:
			continue
		}
		if amount.Cmp(want) == 0 {
			return true
		}
	}
	return false
}

// feeOf 把回执里的 gas 用量换算成 BNB 的手续费（wei）。
//
// EffectiveGasPrice 在老节点上可能缺失，缺失时退回 0：本字段是「实际花了多少」的
// 留痕，编一个数出来会让对账把节点报价当成实际支出。
func (c *onchainChannel) feeOf(rec *types.Receipt) int64 {
	if rec == nil || rec.EffectiveGasPrice == nil {
		return 0
	}
	fee := new(big.Int).Mul(new(big.Int).SetUint64(rec.GasUsed), rec.EffectiveGasPrice)
	return saturateInt64(fee)
}

// deadByNonce 判断这一笔是否已经不可能再上链。
//
// 三个条件必须同时成立，才敢判 FAILED：
//  1. 按哈希问节点，这笔交易既不在块里也不在内存池里（TransactionByHash 返回 NotFound）；
//  2. 账户的已落块 nonce 已经越过了这笔交易的 nonce（同一个 nonce 位被别的交易占了）；
//  3. 交易里那个 nonce 读得出来（RawTx 能还原）。
//
// 只满足第 2 条是不够的：占用那个 nonce 的可能正是本单自己——回执查不到只是因为
// 读到了还没同步的节点副本。只有第 1 条同时成立，才能排除「其实是本单已落块」。
func (c *onchainChannel) deadByNonce(ctx context.Context, entry *ledger.Entry) (*pay.PayResult, error) {
	tx, err := decodeRawTx(entry)
	if err != nil {
		return nil, err
	}
	hash := common.HexToHash(entry.TxID)
	if _, _, err := c.rpc.TransactionByHash(ctx, hash); err == nil {
		// 还在池里（isPending=true）或已在块里（false）：都还没到判死的时刻。
		return nil, nil
	} else if !errors.Is(err, ethereum.NotFound) {
		return nil, err
	}
	wallet, err := c.entryWallet(entry)
	if err != nil {
		return nil, err
	}
	latest, err := c.rpc.LatestNonce(ctx, wallet)
	if err != nil {
		return nil, err
	}
	if latest <= tx.Nonce() {
		return nil, nil
	}
	// nonce 位已被别的交易占用，且本单的哈希不在链上任何位置：这一笔永远不会上链。
	if err := c.settle(entry, "nonce 已被占用且交易不在链上"); err != nil {
		return nil, err
	}
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusFailed, Amount: entry.Amount, AmountUnits: entry.AmountUnits,
		RawNote: c.note(fmt.Sprintf("交易 nonce=%d 已被账户 nonce=%d 越过且链上查无此哈希，未发生资金动作", tx.Nonce(), latest)),
	}, nil
}

// pending 给一条「已受理、结果未出」的回执。
func (c *onchainChannel) pending(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusPending, Amount: entry.Amount, AmountUnits: entry.AmountUnits, RawNote: c.note(note),
	}
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
func (c *onchainChannel) QueryOrder(ctx context.Context, orderNo string) (*pay.PayResult, error) {
	orderNo = strings.TrimSpace(orderNo)
	if orderNo == "" {
		return nil, errors.New("订单号（幂等键）为空")
	}
	entry, found := c.ledger.Get(orderNo)
	if !found {
		return &pay.PayResult{OrderNo: orderNo, Status: pay.StatusUnknown,
			RawNote: "出款台账没有这一单号（可能是进程重启且未注入持久台账）"}, nil
	}
	return c.confirm(ctx, entry, c.nowFn())
}

// decodeRawTx 把台账里的已签名交易还原出来。
func decodeRawTx(entry *ledger.Entry) (*types.Transaction, error) {
	if entry == nil || len(entry.RawTx) == 0 {
		return nil, errors.New("台账条目缺少已签名交易")
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(entry.RawTx); err != nil {
		return nil, fmt.Errorf("台账里的已签名交易不可还原: %w", err)
	}
	return tx, nil
}

// lookupLedgerByChain 在台账查不到时向链上反查「这一单是不是其实已经上过链」。
//
// 它是进程内默认台账重启后的兜底，不是持久台账的替身：命中条件是本合约、本钱包、
// 同一收端、同一金额，四条都对上才算。宁可因为「查不到/查不全」而放弃广播
// （返回错误，让上游重试一次查证），也不在没查清时赌一次双付。
//
// 扫描方式：从链头按区块分段往回翻，每段读一次 eth_getLogs，直到这一段的区块时间
// 早于 72 小时窗口。用区块时间而不是「按平均出块时间估区块数」：BSC 的出块间隔
// 改过几次（3s → 1.5s → 0.75s），估算值一旦偏小，窗口就会被悄悄截短。
func (c *onchainChannel) lookupLedgerByChain(ctx context.Context, o *pay.PayOrder, a orderAsset, now time.Time) (*ledger.Entry, error) {
	amount, err := o.AmountBig()
	if err != nil {
		return nil, err
	}
	head, err := c.rpc.Head(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-scanWindow).Unix()
	target := common.HexToAddress(strings.TrimSpace(o.To.Address))
	topics := [][]common.Hash{{transferEventTopic}, {addressTopic(a.wallet)}, {addressTopic(target)}}
	high := head
	for chunk := 0; chunk < maxLogChunks; chunk++ {
		if high == 0 {
			return nil, nil
		}
		low := uint64(0)
		if high+1 > logBlockSpan {
			low = high + 1 - logBlockSpan
		}
		logs, err := c.rpc.Logs(ctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(low),
			ToBlock:   new(big.Int).SetUint64(high),
			Addresses: []common.Address{c.token},
			Topics:    topics,
		})
		if err != nil {
			return nil, err
		}
		if len(logs) >= logResultCap {
			// 取满一页：这一段可能还有没读到的日志，「没命中」不等于「没有」。
			return nil, fmt.Errorf("链上反查在区块 %d–%d 取满 %d 条日志，无法确认本单是否已上过链", low, high, logResultCap)
		}
		if hit := pickTransfer(logs, amount); hit != nil {
			entry := &ledger.Entry{
				OrderNo: strings.TrimSpace(o.OrderNo), TxID: strings.ToLower(hit.TxHash.Hex()),
				From: a.wallet.Hex(), To: target.Hex(), Contract: c.opt.Contract,
				Amount: saturateInt64(amount), AmountUnits: amount.String(),
				FeeLimit: int64(c.chain.GasLimit), State: ledger.StateBroadcast, UpdatedAtMS: ledger.NowMS(now),
				// ExpiresAtMS 留 0：反查回来的已经是在链上的笔，不再有「过期」可言。
			}
			if err := c.ledger.Put(entry); err != nil {
				return nil, err
			}
			return entry, nil
		}
		if low == 0 {
			return nil, nil
		}
		ts, err := c.rpc.BlockTime(ctx, low)
		if err != nil {
			return nil, err
		}
		if ts <= cutoff {
			return nil, nil
		}
		high = low - 1
	}
	return nil, fmt.Errorf("链上反查已扫 %d 段仍未覆盖 %s 窗口，无法确认本单是否已上过链", maxLogChunks, scanWindow)
}

// pickTransfer 在一段日志里找金额相符的那一条。
//
// 收端已经由 topic 过滤掉了，这里只比金额：同一收款方在窗口内可能被本钱包付过很多次，
// 而「同额不同单」在业务上是常态（固定金额的商品），所以命中之后仍要如实返回——
// 反查只回答「有没有一笔同额同收端的转账」，它本来就是概率性的兜底。
func pickTransfer(logs []types.Log, amount *big.Int) *types.Log {
	want := new(big.Int).Set(amount)
	for i := range logs {
		lg := &logs[i]
		if len(lg.Topics) >= 4 {
			if new(big.Int).SetBytes(lg.Topics[3].Bytes()).Cmp(want) == 0 {
				return lg
			}
			continue
		}
		if len(lg.Data) >= 32 && new(big.Int).SetBytes(lg.Data[:32]).Cmp(want) == 0 {
			return lg
		}
	}
	return nil
}
