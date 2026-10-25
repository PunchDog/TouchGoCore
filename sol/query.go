package sol

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件回答一个问题：这一单号在 Solana 上到底走到哪一步了。
//
// 它是出款链路的另一半：广播只管把钱交出去，结论必须由按签名查证给出。
// 认不出来的情况一律给 UNKNOWN 而不是 FAILED——把「没查到」当成「失败」，
// 上游就会重下一单，而那正是双付；FAILED 只留给链上明确说了不行的情况。
//
// Solana 比 EVM 多一条确定性判据：交易靠 blockhash 定寿命。已签名交易的
// blockhash 一旦失效，任何持有它的字节都不可能再上链。因此「按签名查不到 +
// blockhash 已失效」可以安全判死，而不必让它永远挂在 PENDING；EVM 那边对应的是
// 「回执查不到 + nonce 被越过」，判死口径一致，判据各自取自本链的排序机制。
const (
	// scanWindow 是链上反查的时间窗口。窗口之外不再翻：进程内默认台账重启即丢，
	// 而「重启超过三天后才用同一个单号再发一次」在业务上已经不是同一笔了。
	scanWindow = 72 * time.Hour
	// sigPageLimit 是单页签名条数的上限（节点允许的最大值）。
	sigPageLimit = 1000
	// maxSigPages 是反查最多翻几页。翻满仍未覆盖窗口就必须报错拒绝广播，
	// 不能把「没翻完」读成「没有」。
	maxSigPages = 10
	// maxTxLookups 是反查最多逐笔取几笔交易。签名列表里绝大多数是别人付给
	// 热钱包的入账，真正要核对的本钱包出账是少数；这个上限保证一次反查的
	// 代价有界，超过就报错而不是无限翻下去。
	maxTxLookups = 200
)

// confirm 按台账里的签名查证并归一状态。
func (c *onchainChannel) confirm(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	sig, err := solana.SignatureFromBase58(strings.TrimSpace(entry.TxID))
	if err != nil {
		// 台账里的签名不可解析：说不清这一笔是什么，绝不下沉成 FAILED。
		return c.unknown(entry, "台账里的交易签名不可解析: "+c.redact(err.Error())), nil
	}
	out, err := c.rpc.Transaction(ctx, sig)
	switch {
	case err == nil && out != nil:
		return c.judge(ctx, entry, out, now)
	case err != nil && !errors.Is(err, rpc.ErrNotFound):
		// 查不动：交回 UNKNOWN 并带上签名，让对账有把手。绝不下沉成 FAILED。
		return c.unknown(entry, "链上查证失败: "+c.redact(err.Error())), nil
	}
	// 签名还查不到：用 blockhash 是否失效判它是否已经不可能上链。
	if dead, err := c.deadByBlockhash(ctx, entry); err != nil {
		return c.pending(entry, "尚无交易记录，且读不出 blockhash 状态: "+c.redact(err.Error())), nil
	} else if dead != nil {
		return dead, nil
	}
	note := "广播已交出，链上尚无交易记录"
	if entry.State == ledger.StatePrepared {
		note = "已签名未广播，链上尚无交易记录"
	}
	return c.pending(entry, note), nil
}

// judge 按交易 meta 归一：执行结果 + 本钱包代币余额变化双判。
//
// 只看 meta.Err 是不够的：transferChecked 在源账户余额不足时会失败（meta.Err 非空），
// 但「成功」也不能只凭 meta.Err == nil——同一笔交易里可能还有其他指令在动同一个
// token account（ATA 创建、代理转发）。因此成功必须同时满足「无错」与
// 「本钱包在该 mint 上的代币余额净减少恰好等于本单金额」。
func (c *onchainChannel) judge(ctx context.Context, entry *ledger.Entry, out *rpc.GetTransactionResult, now time.Time) (*pay.PayResult, error) {
	meta := out.Meta
	if meta == nil {
		// 落了块却没有 meta：读不出余额变化，也就说不出钱动没动。
		return c.unknown(entry, "交易已落块但节点未返回 meta（无法核对余额变化）"), nil
	}
	if meta.Err != nil {
		// 进块但执行失败：手续费照烧，钱没动。终局，不重建——
		// 同一份 raw tx 重广播结论一样，而换一笔就是第二笔钱。
		if err := c.settle(entry, "链上执行失败"); err != nil {
			return nil, err
		}
		return &pay.PayResult{
			OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
			Status: pay.StatusFailed, Amount: entry.Amount,
			Fee: saturateUint64(meta.Fee), FeeCurrency: pay.CurrencySOL,
			RawNote: c.note("链上执行失败（手续费已消耗）: " + fmt.Sprint(meta.Err)),
		}, nil
	}
	delta, err := c.tokenDelta(meta, entry)
	if err != nil {
		return c.unknown(entry, "读不出 token 余额变化: "+err.Error()), nil
	}
	if delta.Cmp(new(big.Int).Neg(big.NewInt(entry.Amount))) != 0 {
		// 没有错、但本钱包这个 mint 上的净变化对不上本单金额：说不清这笔做了什么。
		// 不落终局、不判成功——留给对账按签名去核，绝不给上游「可以重下」的结论。
		return c.unknown(entry, fmt.Sprintf("交易无错但本钱包代币净变化为 %s，与本单应付 %d 不符", delta.String(), entry.Amount)), nil
	}
	confs := int64(0)
	if head, err := c.rpc.Head(ctx); err == nil && head > out.Slot {
		confs = int64(head - out.Slot)
	}
	res := &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusSuccess, Amount: entry.Amount,
		Fee: saturateUint64(meta.Fee), FeeCurrency: pay.CurrencySOL,
		RawNote: c.note("链上成功（余额净减少与本单一致）"),
	}
	// 已进块即显式回确认数（含 0）；nil 会被上游当成「无确认数概念」直接 success。
	v := confs
	res.Confirmations = &v
	if err := c.settle(entry, "链上终局: success"); err != nil {
		return nil, err
	}
	return res, nil
}

// tokenDelta 算出「本钱包在本 mint 上的代币余额」在这一笔里的净变化。
//
// Solana 的代币余额挂在 token account 上，pre/post 两张表按 AccountIndex 与交易
// 账户表对应，但账户在交易中新建（收款方 ATA）或关闭时只会出现在一边——
// 缺失的一边按 0 处理，这与「账户当时余额为 0」在求和口径上等价。
//
// 只累计 owner 是热钱包、mint 是本通道 mint 的账户：交易里可能同时有别的
// 代币、别人的账户，把它们算进来就会把一笔正常的转账算成对不上。
func (c *onchainChannel) tokenDelta(meta *rpc.TransactionMeta, entry *ledger.Entry) (*big.Int, error) {
	owner, err := c.entryOwner(entry)
	if err != nil {
		return nil, err
	}
	mint, err := c.entryMint(entry)
	if err != nil {
		return nil, err
	}
	pre, err := c.sumOwnedTokenBalances(meta.PreTokenBalances, owner, mint)
	if err != nil {
		return nil, err
	}
	post, err := c.sumOwnedTokenBalances(meta.PostTokenBalances, owner, mint)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Sub(post, pre), nil
}

// sumOwnedTokenBalances 累加热钱包在本 mint 上所有 token account 的余额。
//
// 按「所有」而不是只按 ATA：同一钱包在同一 mint 上非 ATA 的 token account 在
// Solana 上合法存在。求和口径下，从哪个账户转出都算进净变化，比只盯 ATA
// 更难被一笔「先从别处归集再转出」的交易骗过去。
func (c *onchainChannel) sumOwnedTokenBalances(list []rpc.TokenBalance, owner, mint solana.PublicKey) (*big.Int, error) {
	sum := new(big.Int)
	for i := range list {
		tb := &list[i]
		if !tb.Mint.Equals(mint) {
			continue
		}
		if tb.Owner == nil || !tb.Owner.Equals(owner) {
			continue
		}
		if tb.UiTokenAmount == nil {
			continue
		}
		v, err := parseTokenAmount(tb.UiTokenAmount.Amount)
		if err != nil {
			return nil, fmt.Errorf("token 余额字段不可解析: %w", err)
		}
		sum.Add(sum, new(big.Int).SetUint64(v))
	}
	return sum, nil
}

// deadByBlockhash 判断这一笔是否已经不可能再上链。
//
// 两个条件必须同时成立，才敢判 FAILED：
//  1. 按签名问节点，这笔交易查不到（调用方已确认 rpc.ErrNotFound）；
//  2. 这一笔的 blockhash 已不在有效期内。
//
// 判据 2 是 Solana 独有的确定性：blockhash 失效之后，任何持有它的已签名交易
// 都会被节点拒收，也就不可能再动钱。反过来只要它还有效，就必须继续等，
// 不能因为「查了几次没查到」就判死。
func (c *onchainChannel) deadByBlockhash(ctx context.Context, entry *ledger.Entry) (*pay.PayResult, error) {
	tx, err := decodeRawTx(entry)
	if err != nil {
		return nil, err
	}
	valid, err := c.rpc.IsBlockhashValid(ctx, tx.Message.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	if valid {
		return nil, nil
	}
	if err := c.settle(entry, "blockhash 已失效且链上查无此签名"); err != nil {
		return nil, err
	}
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusFailed, Amount: entry.Amount,
		RawNote: c.note("交易 blockhash 已失效且链上查无此签名，未发生资金动作"),
	}, nil
}

// pending 给一条「已受理、结果未出」的回执。
func (c *onchainChannel) pending(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusPending, Amount: entry.Amount, RawNote: c.note(note),
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
	// 持同单号的台账锁：confirm 里的 settle 会 Put 覆盖台账状态，必须与同一单号的
	// Withdraw 串行，否则查证的 settle 与出款的状态迁移会互相盖写。
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

// decodeRawTx 把台账里的已签名交易还原出来。
func decodeRawTx(entry *ledger.Entry) (*solana.Transaction, error) {
	if entry == nil || len(entry.RawTx) == 0 {
		return nil, errors.New("台账条目缺少已签名交易")
	}
	tx, err := solana.TransactionFromBytes(entry.RawTx)
	if err != nil {
		return nil, fmt.Errorf("台账里的已签名交易不可还原: %w", err)
	}
	if tx == nil {
		return nil, errors.New("台账里的已签名交易不可还原: 解出空交易")
	}
	return tx, nil
}

// lookupLedgerByChain 在台账查不到时向链上反查「有没有一笔像本单的转出」。
//
// 返回值是**候选**，不是本单的已落账条目：命中只能证明「本钱包向本收端付过
// 同额同 mint 的一笔」，不能证明那一笔就是这一单。调用方只能据此禁止广播并回
// UNKNOWN，绝不能把它写进台账或交给 confirm——否则历史上同址同额的另一笔会被
// 结算成本单的 SUCCESS（F2）。真正跨重启的幂等靠下游注入持久 journal。
//
// 扫描方式：从链上按热钱包的签名列表倒序翻（该列表天然按 slot 降序），逐笔取
// 交易 meta 比余额变化，直到这一条的 blockTime 早于 72 小时窗口。用 blockTime
// 而不是「按 slot 估算」：Solana 的 slot 时长会漂，估算值一旦偏小，窗口就被
// 悄悄截短，而那正是「没扫到的已上链交易」变成第二笔钱的地方。
func (c *onchainChannel) lookupLedgerByChain(ctx context.Context, o *pay.PayOrder, a orderAsset, now time.Time) (*ledger.Entry, error) {
	to, err := solana.PublicKeyFromBase58(strings.TrimSpace(o.To.Address))
	if err != nil {
		return nil, fmt.Errorf("反查前解析收款地址失败: %w", err)
	}
	cutoff := now.Add(-scanWindow).Unix()
	probe := &ledger.Entry{From: a.wallet.String(), To: to.String(), Contract: c.opt.Mint, Amount: o.Amount}
	var before solana.Signature
	lookups := 0
	for page := 0; page < maxSigPages; page++ {
		sigs, err := c.rpc.SignaturesForAddress(ctx, a.wallet, before, sigPageLimit)
		if err != nil {
			return nil, err
		}
		if len(sigs) == 0 {
			return nil, nil
		}
		reachedWindow := false
		for _, s := range sigs {
			if s == nil {
				continue
			}
			if s.BlockTime != nil && int64(*s.BlockTime) < cutoff {
				// 列表按 slot 降序：这一条已在窗口之外，后面的只会更早。
				reachedWindow = true
				break
			}
			if s.Err != nil {
				// 失败交易没动过代币，不必再取 meta。
				continue
			}
			if lookups >= maxTxLookups {
				return nil, fmt.Errorf("链上反查已核对 %d 笔交易仍未覆盖 %v 窗口，无法确认本单是否已上过链", maxTxLookups, scanWindow)
			}
			lookups++
			out, err := c.rpc.Transaction(ctx, s.Signature)
			if err != nil {
				if errors.Is(err, rpc.ErrNotFound) {
					// 签名列表里刚出现、节点副本还没跟上；跳过不等于命中。
					continue
				}
				return nil, err
			}
			if out == nil || out.Meta == nil || out.Meta.Err != nil {
				continue
			}
			if !c.paidTo(out.Meta, probe) {
				continue
			}
			// 只构造候选，不 Put：台账一旦落成 broadcast，后续 confirm 会把
			// 「同址同额的任意一笔」读成本单成功的证据。
			return &ledger.Entry{
				OrderNo: strings.TrimSpace(o.OrderNo), TxID: s.Signature.String(),
				From: a.wallet.String(), To: to.String(), Contract: c.opt.Mint, Amount: o.Amount,
			}, nil
		}
		if reachedWindow {
			return nil, nil
		}
		if len(sigs) < sigPageLimit {
			// 这一页没取满：签名列表已到底。
			return nil, nil
		}
		before = sigs[len(sigs)-1].Signature
	}
	return nil, fmt.Errorf("链上反查已扫 %d 页仍未覆盖 %v 窗口，无法确认本单是否已上过链", maxSigPages, scanWindow)
}

// paidTo 判断一笔已成功落块的交易是不是「本钱包付给了本收端、金额相符」。
//
// 三个条件逐一比，不靠「余额有变化就算」：
//  1. 本钱包在本 mint 上的净变化恰好是 -amount；
//  2. 交易后的 token 余额表里有一个账户属于本收端；
//  3. 那个账户的 mint 就是本通道的 mint（与 1 同源，但单独再确认一次收端）。
//
// 反查只回答「有没有一笔同额同收端的转账」，它本来就是概率性的兜底：
// 同一收款方在窗口内被本钱包付过很多次是常态，命中之后仍要如实返回。
// 命中只是候选，调用方不得据此写台账或结算，只能禁止广播并回 UNKNOWN。
func (c *onchainChannel) paidTo(meta *rpc.TransactionMeta, entry *ledger.Entry) bool {
	if meta == nil || entry == nil {
		return false
	}
	delta, err := c.tokenDelta(meta, entry)
	if err != nil {
		return false
	}
	if delta.Cmp(new(big.Int).Neg(big.NewInt(entry.Amount))) != 0 {
		return false
	}
	to, err := solana.PublicKeyFromBase58(strings.TrimSpace(entry.To))
	if err != nil {
		return false
	}
	mint, err := c.entryMint(entry)
	if err != nil {
		return false
	}
	for i := range meta.PostTokenBalances {
		tb := &meta.PostTokenBalances[i]
		if !tb.Mint.Equals(mint) {
			continue
		}
		if tb.Owner != nil && tb.Owner.Equals(to) {
			return true
		}
	}
	return false
}
