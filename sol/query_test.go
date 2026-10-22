package sol

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/rpc"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件钉查单侧那几条「纯本地就能判」的判据：台账原文还原、余额净变化、
// 反查命中条件、失败终局、以及「说不清就给 UNKNOWN」。
//
// 连不上节点时，confirm 的「查不到 → blockhash 判死」与 judge 的确认数分支
// 无法在这里覆盖（它们需要节点回答）；但它们的判据本身是纯函数，
// 而那正是「钱动没动」这句话真正的依据。

// solTokenBal 构造一条 token 余额记录。Owner 是指针：链上返回里它可以缺失，
// 缺失的记录必须被排除而不是当成零地址。
func solTokenBal(accountIndex uint16, mint, owner solana.PublicKey, amount string) rpc.TokenBalance {
	o := owner
	return rpc.TokenBalance{
		AccountIndex:  accountIndex,
		Mint:          mint,
		Owner:         &o,
		UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: 6},
	}
}

// TestDecodeRawTx 台账里的已签名字节必须能还原成同一笔交易。
func TestDecodeRawTx(t *testing.T) {
	if _, err := decodeRawTx(nil); err == nil {
		t.Error("nil 条目未被拦下")
	}
	if _, err := decodeRawTx(&ledger.Entry{}); err == nil {
		t.Error("缺 RawTx 的条目未被拦下")
	}
	if _, err := decodeRawTx(&ledger.Entry{RawTx: []byte{1, 2, 3}}); err == nil {
		t.Error("不可解析的字节未被拦下")
	}

	sgn := solTestSigner(t)
	hash := solana.Hash{7}
	tx, err := solana.NewTransactionBuilder().
		SetRecentBlockHash(hash).
		SetFeePayer(sgn.Address()).
		AddInstruction(computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()).
		Build()
	if err != nil {
		t.Fatalf("组包失败: %v", err)
	}
	if err := sgn.Sign(tx); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := decodeRawTx(&ledger.Entry{TxID: txSignature(tx), RawTx: raw})
	if err != nil {
		t.Fatalf("还原失败: %v", err)
	}
	if back.Message.RecentBlockhash != hash {
		t.Errorf("还原后 blockhash=%s，期望 %s", back.Message.RecentBlockhash, hash)
	}
	if len(back.Signatures) == 0 || !back.Signatures[0].Equals(tx.Signatures[0]) {
		t.Error("还原后签名与原文不一致")
	}
}

// TestTokenDelta 净变化只统计「本 mint + 热钱包持有」的那些账户。
func TestTokenDelta(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	mint := c.mint
	otherMint, err := solana.PublicKeyFromBase58(solPatternAcct) // 与 c.mint 不同的另一把公钥
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	otherOwner, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}

	cases := []struct {
		name string
		pre  []rpc.TokenBalance
		post []rpc.TokenBalance
		want string
	}{
		{
			name: "正常转出",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400")},
			want: "-600",
		},
		{
			name: "收款方 ATA 新建（只出现在 post）",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, mint, otherOwner, "600")},
			want: "0",
		},
		{
			name: "别的 mint 不算",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, otherMint, owner, "500")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400"), solTokenBal(2, otherMint, owner, "0")},
			want: "-600",
		},
		{
			name: "别人的账户不算",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000"), solTokenBal(2, mint, otherOwner, "900")},
			post: []rpc.TokenBalance{solTokenBal(1, mint, owner, "400"), solTokenBal(2, mint, otherOwner, "1500")},
			want: "-600",
		},
		{
			name: "账户关闭（只出现在 pre）",
			pre:  []rpc.TokenBalance{solTokenBal(1, mint, owner, "1000")},
			post: nil,
			want: "-1000",
		},
	}
	for _, cs := range cases {
		delta, err := c.tokenDelta(&rpc.TransactionMeta{PreTokenBalances: cs.pre, PostTokenBalances: cs.post}, &ledger.Entry{})
		if err != nil {
			t.Errorf("%s: 计算失败: %v", cs.name, err)
			continue
		}
		if delta.String() != cs.want {
			t.Errorf("%s: 净变化=%s，期望 %s", cs.name, delta, cs.want)
		}
	}
}

// TestTokenDeltaRejectsBadAmount 读不出余额时不能当成 0：那会把「读失败」读成「没动钱」。
func TestTokenDeltaRejectsBadAmount(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	bad := solTokenBal(1, c.mint, owner, "not-a-number")
	if _, err := c.tokenDelta(&rpc.TransactionMeta{PreTokenBalances: []rpc.TokenBalance{bad}}, &ledger.Entry{}); err == nil {
		t.Error("不可解析的余额未被拦下")
	}
}

// TestPaidTo 反查命中必须同时满足「本钱包净减少本单金额」与「收端在交易后持有本 mint」。
func TestPaidTo(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	entry := &ledger.Entry{To: to.String(), Amount: 600}
	mk := func(pre, post []rpc.TokenBalance) *rpc.TransactionMeta {
		return &rpc.TransactionMeta{PreTokenBalances: pre, PostTokenBalances: post}
	}
	ours := func(amount string) rpc.TokenBalance { return solTokenBal(1, c.mint, owner, amount) }

	if !c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("400"), solTokenBal(2, c.mint, to, "600")}), entry) {
		t.Error("金额与收端都相符的交易未被识别为已付")
	}
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("400")}), entry) {
		t.Error("收端不在交易后余额表里却判成了已付")
	}
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")},
		[]rpc.TokenBalance{ours("900"), solTokenBal(2, c.mint, to, "100")}), entry) {
		t.Error("金额不符却判成了已付")
	}
	if c.paidTo(nil, entry) || c.paidTo(mk(nil, nil), nil) {
		t.Error("nil meta / nil 条目应判为未命中")
	}
	// 台账里的收端不可解析时不能 panic，只能判未命中。
	if c.paidTo(mk([]rpc.TokenBalance{ours("1000")}, []rpc.TokenBalance{ours("400")}),
		&ledger.Entry{To: "garbage", Amount: 600}) {
		t.Error("收端不可解析时判成了已付")
	}
}

// TestJudgeFailedIsTerminal 进块但执行失败是终局：手续费照烧，钱没动，不重建。
func TestJudgeFailedIsTerminal(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-J1", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.judge(context.Background(), entry,
		&rpc.GetTransactionResult{Meta: &rpc.TransactionMeta{Err: errors.New("custom program error: 0x1"), Fee: 5000}}, c.nowFn())
	if err != nil {
		t.Fatalf("判失败时不应报错: %v", err)
	}
	if res.Status != pay.StatusFailed {
		t.Errorf("状态=%s，期望 failed", res.Status)
	}
	if res.Fee != int64(signatureFeeLamports) || res.FeeCurrency != pay.CurrencySOL {
		t.Errorf("手续费留痕=%d %s，期望 %d SOL", res.Fee, res.FeeCurrency, signatureFeeLamports)
	}
	if res.RawNote == "" {
		t.Error("失败回执必须带可读原因")
	}
	got, ok := c.ledger.Get("SOL-J1")
	if !ok || !got.Terminal() {
		t.Errorf("失败结论未落成终局: ok=%v state=%v", ok, got)
	}
}

// TestJudgeMismatchedDeltaIsUnknown 无错但余额变化对不上：说不清，绝不给成功或失败。
func TestJudgeMismatchedDeltaIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	entry := &ledger.Entry{OrderNo: "SOL-J2", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
	}
	res, err := c.judge(context.Background(), entry, &rpc.GetTransactionResult{Meta: meta}, c.nowFn())
	if err != nil {
		t.Fatalf("判 UNKNOWN 时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
	if !strings.Contains(res.RawNote, "净变化") || !strings.Contains(res.RawNote, "本单应付") {
		t.Errorf("UNKNOWN 文案未点明判据与对不上的金额: %q", res.RawNote)
	}
}

// TestJudgeMissingMetaIsUnknown 落了块却没有 meta：读不出余额变化，只能 UNKNOWN。
func TestJudgeMissingMetaIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-J3", TxID: "sig", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.judge(context.Background(), entry, &rpc.GetTransactionResult{}, c.nowFn())
	if err != nil {
		t.Fatalf("判 UNKNOWN 时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
}

// TestConfirmBadSignatureIsUnknown 台账里的签名不可解析时给 UNKNOWN，不 panic 也不判死。
func TestConfirmBadSignatureIsUnknown(t *testing.T) {
	c := solTestChannel(t)
	entry := &ledger.Entry{OrderNo: "SOL-C1", TxID: "0OIl-not-base58", Amount: 600, State: ledger.StateBroadcast}
	res, err := c.confirm(context.Background(), entry, c.nowFn())
	if err != nil {
		t.Fatalf("签名不可解析时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown", res.Status)
	}
	if res.TxHash != entry.TxID {
		t.Errorf("UNKNOWN 回执必须带上台账里的签名供对账: %q", res.TxHash)
	}
}

// TestQueryOrderWithoutEntry 台账里没有这一单号时给 UNKNOWN，不去链上瞎猜。
func TestQueryOrderWithoutEntry(t *testing.T) {
	c := solTestChannel(t)
	if _, err := c.QueryOrder(context.Background(), "   "); err == nil {
		t.Error("空单号未被拦下")
	}
	res, err := c.QueryOrder(context.Background(), "SOL-NOT-FOUND")
	if err != nil {
		t.Fatalf("查无此单时不应报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Errorf("状态=%s，期望 unknown（把查不到说成失败会诱使上游重下单）", res.Status)
	}
}

// TestSettleRequiresWritableJournal 终局落不进台账时必须报错，而不是照样返回结论。
func TestSettleRequiresWritableJournal(t *testing.T) {
	c := solTestChannel(t)
	c.ledger.SetJournal(failingJournal{})
	entry := &ledger.Entry{OrderNo: "SOL-S1", TxID: "sig", Amount: 600}
	err := c.settle(entry, "链上终局")
	if !errors.Is(err, ErrJournalUnavailable) {
		t.Errorf("台账写不进去时应报 ErrJournalUnavailable，实得 %v", err)
	}
}

// failingJournal 是一个永远写不进去的台账存储，用来验证「记不上就不下结论」。
type failingJournal struct{}

func (failingJournal) Put(*ledger.Entry) error                  { return errors.New("disk full") }
func (failingJournal) Get(string, string) (*ledger.Entry, bool) { return nil, false }

// TestTokenDeltaSignsAreBig 金额比较必须走大整数：u64 余额在 int64 边界附近会回绕。
func TestTokenDeltaSignsAreBig(t *testing.T) {
	c := solTestChannel(t)
	owner := c.sign.Address()
	// 余额接近 u64 上限时，post-pre 用 int64 算会回绕成正数。
	big1 := "18446744073709551615"
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, big1)},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "0")},
	}
	delta, err := c.tokenDelta(meta, &ledger.Entry{})
	if err != nil {
		t.Fatalf("计算失败: %v", err)
	}
	if delta.Sign() >= 0 {
		t.Errorf("净变化=%s，应为一个负数（大整数不能回绕）", delta)
	}
	if delta.Cmp(new(big.Int).Neg(new(big.Int).SetUint64(^uint64(0)))) != 0 {
		t.Errorf("净变化=%s，期望 -%s", delta, big1)
	}
}
