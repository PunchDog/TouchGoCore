package sol

// 按单私钥在 SOL 的落地判据：资产解析、台账钉住、余额净变化与反查命中都跟随订单钱包。

import (
	"context"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"touchgocore/ledger"
	"touchgocore/pay"
)

func TestWithdrawLockedPinsPerOrderWallet(t *testing.T) {
	c := solTestChannel(t)
	keyRaw, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	o := &pay.PayOrder{
		OrderNo: "SOL-PER-1", Amount: 600, Currency: pay.CurrencyUSDT,
		To:   &pay.Party{Address: to.String()},
		From: &pay.Party{Address: owner.String(), PrivateKey: pay.Secret(keyRaw)},
	}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "sig", From: c.sign.Address().String(),
		To: to.String(), Contract: c.opt.Mint, Amount: o.Amount, State: ledger.StateSettled,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同单号换钱包未被拒: %v", err)
	}
}

func TestWithdrawLockedPinsMint(t *testing.T) {
	c := solTestChannel(t)
	keyRaw, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	o := &pay.PayOrder{
		OrderNo: "SOL-PER-2", Amount: 600, Currency: pay.CurrencyUSDT,
		To:   &pay.Party{Address: to.String()},
		From: &pay.Party{Address: owner.String(), PrivateKey: pay.Secret(keyRaw)},
	}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	otherMint, err := solana.PublicKeyFromBase58(solPatternAcct)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "sig", From: owner.String(),
		To: to.String(), Contract: otherMint.String(), Amount: o.Amount, State: ledger.StateSettled,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同单号换 mint 未被拒: %v", err)
	}
}

func TestTokenDeltaUsesEntryOwnerAndMint(t *testing.T) {
	c := solTestChannel(t)
	_, owner := qaSOLPerOrderKey(t)
	entry := &ledger.Entry{From: owner.String(), Contract: c.opt.Mint}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "400")},
	}
	delta, err := c.tokenDelta(meta, entry)
	if err != nil {
		t.Fatalf("按单 owner 的净变化计算失败: %v", err)
	}
	if delta.String() != "-600" {
		t.Fatalf("净变化=%s，期望 -600", delta)
	}
	// 台账缺 From/Contract 时才回落配置热钱包；这里换成配置钱包后，上面那组
	// 按单 owner 的余额不能被算进净变化。
	legacyDelta, err := c.tokenDelta(meta, &ledger.Entry{})
	if err != nil {
		t.Fatalf("旧台账回落计算失败: %v", err)
	}
	if legacyDelta.Sign() != 0 {
		t.Fatalf("旧台账回落配置钱包时不应命中按单 owner 的余额，实得 %s", legacyDelta)
	}
}

func TestPaidToUsesEntryOwnerAndMint(t *testing.T) {
	c := solTestChannel(t)
	_, owner := qaSOLPerOrderKey(t)
	to, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatal(err)
	}
	entry := &ledger.Entry{From: owner.String(), Contract: c.opt.Mint, To: to.String(), Amount: 600}
	meta := &rpc.TransactionMeta{
		PreTokenBalances:  []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "1000")},
		PostTokenBalances: []rpc.TokenBalance{solTokenBal(1, c.mint, owner, "400"), solTokenBal(2, c.mint, to, "600")},
	}
	if !c.paidTo(meta, entry) {
		t.Fatal("按单钱包的正常转出未被识别")
	}
	// 换成配置热钱包：同一份 meta 不能被认成这一单。
	entry.From = c.sign.Address().String()
	if c.paidTo(meta, entry) {
		t.Fatal("meta 里的 owner 与台账不是同一账户却被认成已付")
	}
}
