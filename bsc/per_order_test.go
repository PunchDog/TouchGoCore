package bsc

// 按单私钥在 BSC 的落地判据：资产解析、台账钉住、回执匹配与 nonce 分桶。
// 全部不连节点：出款前的拒绝分支在触链之前执行。

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"touchgocore/ledger"
	"touchgocore/pay"
)

func bscPerOrderOrder(t *testing.T) (*onchainChannel, *pay.PayOrder, orderAsset) {
	t.Helper()
	c := newTestChannel(t)
	keyHex, addr := qaBSCPerOrderKey(t)
	o := walletOrder(evmAllLower)
	o.OrderNo = "PO-BSC-PER-1"
	o.From = &pay.Party{Kind: pay.PartyWallet, Address: addr.Hex(), PrivateKey: pay.Secret(keyHex)}
	a, err := c.assetOf(o)
	if err != nil {
		t.Fatalf("解析按单资产失败: %v", err)
	}
	return c, o, a
}

func TestWithdrawLockedPinsPerOrderWallet(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "0xdead", From: evmChecksumMixed2,
		To: o.To.Address, Contract: c.opt.Contract, Amount: o.Amount,
		State: ledger.StateSettled, UpdatedAtMS: ledger.NowMS(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同单号换钱包未被拒: %v", err)
	}
}

func TestWithdrawLockedPinsContract(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	if err := c.ledger.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "0xdead", From: a.wallet.Hex(),
		To: o.To.Address, Contract: evmChecksumMixed2, Amount: o.Amount,
		State: ledger.StateSettled, UpdatedAtMS: ledger.NowMS(time.Now()),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.withdrawLocked(context.Background(), o, a)
	if err == nil || !strings.Contains(err.Error(), "不得换币") {
		t.Fatalf("同单号换合约未被拒: %v", err)
	}
}

func TestPrepareRecordsPerOrderFrom(t *testing.T) {
	c, o, a := bscPerOrderOrder(t)
	to := common.HexToAddress(strings.TrimSpace(o.To.Address))
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    1,
		GasPrice: gweiToWei(1),
		Gas:      defaultTransferGasLimit,
		To:       &c.token,
		Value:    big.NewInt(0),
		Data:     erc20TransferData(to, big.NewInt(o.Amount)),
	})
	entry, err := c.prepare(o, tx, to, a, time.Now())
	if err != nil {
		t.Fatalf("按单资产记账失败: %v", err)
	}
	if entry.From != a.wallet.Hex() {
		t.Fatalf("台账 From=%q，期望按单钱包 %s", entry.From, a.wallet.Hex())
	}
	if entry.Contract != c.opt.Contract {
		t.Fatalf("台账合约=%q，期望配置合约 %s", entry.Contract, c.opt.Contract)
	}
}

func TestMatchesTransferUsesEntryWallet(t *testing.T) {
	c := newTestChannel(t)
	_, addr := qaBSCPerOrderKey(t)
	to := common.HexToAddress(evmAllLower)
	entry := &ledger.Entry{
		From: addr.Hex(), To: to.Hex(), Contract: c.opt.Contract, Amount: 1_000_000,
	}
	rec := &types.Receipt{Logs: []*types.Log{
		transferLog(c.token, addr, to, big.NewInt(1_000_000)),
	}}
	if !c.matchesTransfer(rec, entry) {
		t.Fatal("按单钱包发出的 Transfer 未被识别")
	}
	// 换成配置热钱包后，同一份回执不能被认成这一单（旧台账没有 From 才回落配置）。
	entry.From = common.HexToAddress(testWallet).Hex()
	if c.matchesTransfer(rec, entry) {
		t.Fatal("回执里的 from 与台账不是同一账户却被认成已付")
	}
}

func TestNonceCacheIsPerAddress(t *testing.T) {
	c := newTestChannel(t)
	_, a1 := qaBSCPerOrderKey(t)
	_, a2 := qaBSCPerOrderKey(t)
	c.adoptNonce(a1, 5)
	c.adoptNonce(a2, 9)
	if got := c.nonces[a1]; got != 6 {
		t.Fatalf("地址 A 的 next nonce=%d，期望 6", got)
	}
	if got := c.nonces[a2]; got != 10 {
		t.Fatalf("地址 B 的 next nonce=%d，期望 10（不能被 A 的缓存覆盖）", got)
	}
}
