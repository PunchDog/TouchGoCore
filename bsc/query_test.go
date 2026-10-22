package bsc

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

// transferLog 造一条本合约发出的 Transfer(address,address,uint256) 事件。
func transferLog(token, from, to common.Address, amount *big.Int) *types.Log {
	return &types.Log{
		Address: token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to)},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
	}
}

// broadcastEntry 造一条「已交给节点、等回执」的台账条目。
func broadcastEntry(to string, amount int64) *ledger.Entry {
	return &ledger.Entry{
		OrderNo: "O-1", TxID: "0xabc0000000000000000000000000000000000000000000000000000000000001",
		To: to, Contract: evmAllLower, Amount: amount,
		State: ledger.StateBroadcast, UpdatedAtMS: ledger.NowMS(time.Now()),
	}
}

// TestJudgeFailedReceipt 进块但执行失败：gas 照烧、钱没动，是终局。
//
// 同一份 raw tx 重广播结论一样，换一笔就是第二笔钱——所以判 FAILED 并落终局。
func TestJudgeFailedReceipt(t *testing.T) {
	c := newTestChannel(t)
	entry := broadcastEntry(evmAllLower, 1_000_000)
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	rec := &types.Receipt{
		Status:            types.ReceiptStatusFailed,
		GasUsed:           51_000,
		EffectiveGasPrice: big.NewInt(1_000_000_000),
	}
	res, err := c.judge(context.Background(), entry, rec, time.Now())
	if err != nil {
		t.Fatalf("judge 报错: %v", err)
	}
	if res == nil || res.Status != pay.StatusFailed {
		t.Fatalf("状态 = %v，期望 FAILED", res)
	}
	if res.TxHash != entry.TxID || res.OrderNo != entry.OrderNo {
		t.Errorf("回执没带上定位信息: %+v", res)
	}
	if want := int64(51_000) * 1_000_000_000; res.Fee != want {
		t.Errorf("手续费 = %d，期望 %d", res.Fee, want)
	}
	if res.FeeCurrency != pay.CurrencyBNB {
		t.Errorf("手续费币种 = %q，期望 BNB", res.FeeCurrency)
	}
	stored, _ := c.ledger.Get(entry.OrderNo)
	if stored == nil || stored.State != ledger.StateSettled {
		t.Errorf("失败结论没有落成终局: %+v", stored)
	}
}

// TestJudgeSuccessNeedsTransferEvent 成功的双判：status=1 且本单的 Transfer 事件在场。
func TestJudgeSuccessNeedsTransferEvent(t *testing.T) {
	c := newTestChannel(t)
	entry := broadcastEntry(evmAllLower, 1_000_000)
	if err := c.ledger.Put(entry); err != nil {
		t.Fatalf("台账写入失败: %v", err)
	}
	rec := &types.Receipt{
		Status:            types.ReceiptStatusSuccessful,
		GasUsed:           51_000,
		EffectiveGasPrice: big.NewInt(1_000_000_000),
		// BlockNumber 留 nil：本用例不接节点，确认数走「读不到链头则留空」的分支。
		Logs: []*types.Log{
			transferLog(c.token, c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(1_000_000)),
		},
	}
	res, err := c.judge(context.Background(), entry, rec, time.Now())
	if err != nil {
		t.Fatalf("judge 报错: %v", err)
	}
	if res == nil || res.Status != pay.StatusSuccess {
		t.Fatalf("状态 = %v，期望 SUCCESS", res)
	}
	if res.Confirmations != nil {
		t.Errorf("读不到链头时不该编出确认数: %d", *res.Confirmations)
	}
	stored, _ := c.ledger.Get(entry.OrderNo)
	if stored == nil || stored.State != ledger.StateSettled {
		t.Errorf("成功结论没有落成终局: %+v", stored)
	}
}

// TestJudgeSuccessWithoutTransferIsUnknown status=1 但找不到本单事件：说不清，不落终局。
//
// 有些老代币在余额不足时返回 false 而不 revert：回执是成功的，钱却没动。
// 把它读成成功是账实不符，读成失败又可能已经动过钱——只能交给对账按哈希去核。
func TestJudgeSuccessWithoutTransferIsUnknown(t *testing.T) {
	c := newTestChannel(t)
	cases := []struct {
		desc string
		logs []*types.Log
	}{
		{"没有任何日志", nil},
		{"金额不符", []*types.Log{transferLog(c.token, c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(999_999))}},
		{"收款方不符", []*types.Log{transferLog(c.token, c.sign.Address(), common.HexToAddress(evmChecksumMixed2), big.NewInt(1_000_000))}},
		{"别的合约发的", []*types.Log{transferLog(common.HexToAddress(evmChecksumMixed2), c.sign.Address(), common.HexToAddress(evmAllLower), big.NewInt(1_000_000))}},
	}
	for _, cs := range cases {
		entry := broadcastEntry(evmAllLower, 1_000_000)
		if err := c.ledger.Put(entry); err != nil {
			t.Fatalf("%s: 台账写入失败: %v", cs.desc, err)
		}
		res, err := c.judge(context.Background(), entry, &types.Receipt{
			Status: types.ReceiptStatusSuccessful, Logs: cs.logs,
		}, time.Now())
		if err != nil {
			t.Errorf("%s: judge 报错: %v", cs.desc, err)
			continue
		}
		if res == nil || res.Status != pay.StatusUnknown {
			t.Errorf("%s: 状态 = %v，期望 UNKNOWN", cs.desc, res)
			continue
		}
		if !strings.Contains(res.RawNote, "Transfer") {
			t.Errorf("%s: 回执没说明为何认不出: %q", cs.desc, res.RawNote)
		}
		stored, _ := c.ledger.Get(entry.OrderNo)
		if stored == nil || stored.State != ledger.StateBroadcast {
			t.Errorf("%s: UNKNOWN 不该落终局: %+v", cs.desc, stored)
		}
	}
}

// TestMatchesTransfer 四个条件逐一比，任何一条不符都不算本单。
func TestMatchesTransfer(t *testing.T) {
	c := newTestChannel(t)
	from := c.sign.Address()
	to := common.HexToAddress(evmAllLower)
	amount := big.NewInt(7_777)

	base := broadcastEntry(evmAllLower, amount.Int64())
	ok := &types.Receipt{Logs: []*types.Log{transferLog(c.token, from, to, amount)}}
	if !c.matchesTransfer(ok, base) {
		t.Fatal("完全匹配的日志未被认出来")
	}

	// 少数代币把 value 也标成 indexed：金额落在第四个 topic 上。
	indexed := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to), common.BytesToHash(common.LeftPadBytes(amount.Bytes(), 32))},
	}}}
	if !c.matchesTransfer(indexed, base) {
		t.Error("value 被 indexed 的日志未被认出来")
	}

	if c.matchesTransfer(nil, base) {
		t.Error("nil 回执被判成匹配")
	}
	short := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from)},
		Data:    common.LeftPadBytes(amount.Bytes(), 32),
	}}}
	if c.matchesTransfer(short, base) {
		t.Error("只有两个 topic 的日志被判成匹配")
	}
	noData := &types.Receipt{Logs: []*types.Log{{
		Address: c.token,
		Topics:  []common.Hash{transferEventTopic, addressTopic(from), addressTopic(to)},
		Data:    []byte{1, 2, 3},
	}}}
	if c.matchesTransfer(noData, base) {
		t.Error("数据段不足以读出金额的日志被判成匹配")
	}
	// 同一回执里既有别人的事件、也有本单的事件：只要有一条对上就算成功。
	mixed := &types.Receipt{Logs: []*types.Log{
		transferLog(common.HexToAddress(evmChecksumMixed2), from, to, amount),
		transferLog(c.token, from, to, amount),
	}}
	if !c.matchesTransfer(mixed, base) {
		t.Error("混有多条日志时未认出本单那一条")
	}
}

// TestPickTransfer 反查只比金额：收端已由 topic 过滤掉。
func TestPickTransfer(t *testing.T) {
	amount := big.NewInt(4_242)
	logs := []types.Log{
		{Data: common.LeftPadBytes(big.NewInt(1).Bytes(), 32)},
		{Data: common.LeftPadBytes(amount.Bytes(), 32), TxHash: common.HexToHash("0xaa")},
	}
	hit := pickTransfer(logs, amount)
	if hit == nil || hit.TxHash != common.HexToHash("0xaa") {
		t.Fatalf("未命中的金额: %+v", hit)
	}
	four := []types.Log{{
		Topics: []common.Hash{transferEventTopic, {}, {}, common.BytesToHash(common.LeftPadBytes(amount.Bytes(), 32))},
		TxHash: common.HexToHash("0xbb"),
	}}
	if hit := pickTransfer(four, amount); hit == nil || hit.TxHash != common.HexToHash("0xbb") {
		t.Errorf("四 topic 形态未命中: %+v", hit)
	}
	if hit := pickTransfer(logs, big.NewInt(999_999)); hit != nil {
		t.Errorf("不该命中金额 999999: %+v", hit)
	}
	if hit := pickTransfer(nil, amount); hit != nil {
		t.Error("空日志列表命中了")
	}
	if hit := pickTransfer([]types.Log{{Data: []byte{1}}}, big.NewInt(1)); hit != nil {
		t.Error("数据段过短的日志被命中")
	}
}

// TestFeeOf 手续费留痕：缺 EffectiveGasPrice 时给 0，不编数字。
func TestFeeOf(t *testing.T) {
	c := newTestChannel(t)
	if got := c.feeOf(nil); got != 0 {
		t.Errorf("nil 回执的手续费 = %d，期望 0", got)
	}
	if got := c.feeOf(&types.Receipt{GasUsed: 21000}); got != 0 {
		t.Errorf("缺 gas 价的手续费 = %d，期望 0", got)
	}
	if got := c.feeOf(&types.Receipt{GasUsed: 21000, EffectiveGasPrice: big.NewInt(2)}); got != 42000 {
		t.Errorf("手续费 = %d，期望 42000", got)
	}
	// 超出 int64 时饱和而不是溢出成负数。
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	if got := c.feeOf(&types.Receipt{GasUsed: 21000, EffectiveGasPrice: huge}); got <= 0 {
		t.Errorf("超大手续费溢出成 %d", got)
	}
}
