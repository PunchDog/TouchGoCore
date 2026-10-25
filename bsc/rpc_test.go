package bsc

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestERC20TransferData 手工拼的 ABI 编码逐字节对拍。
//
// 不引 ABI 编码器是刻意的（只调两个函数，引工具链反而多一份私货），代价是
// 选择器或左补零写错时不会有编译器报错——所以这里按字节钉死：
// 前 4 字节选择器、随后 32 字节左补零地址、再 32 字节大端金额。
func TestERC20TransferData(t *testing.T) {
	to := common.HexToAddress(evmChecksumMixed2)
	if got := addressTopic(to); got != common.BytesToHash(common.LeftPadBytes(to.Bytes(), 32)) {
		t.Fatalf("addressTopic 与 32 字节左补零不一致: %s", got.Hex())
	}

	for _, amount := range []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		big.NewInt(1_000_000),
		new(big.Int).SetUint64(^uint64(0)),   // 2^64-1
		new(big.Int).Lsh(big.NewInt(1), 255), // 最高位为 1，左补零不能退化成符号扩展
	} {
		data := erc20TransferData(to, amount)
		if len(data) != 4+32+32 {
			t.Fatalf("金额 %s: 调用数据长度 %d，期望 68", amount, len(data))
		}
		if !bytes.Equal(data[:4], erc20TransferSelector) {
			t.Errorf("金额 %s: 选择器 %x，期望 %x", amount, data[:4], erc20TransferSelector)
		}
		if want := common.LeftPadBytes(to.Bytes(), 32); !bytes.Equal(data[4:36], want) {
			t.Errorf("金额 %s: 地址段 %x，期望 %x", amount, data[4:36], want)
		}
		if want := common.LeftPadBytes(amount.Bytes(), 32); !bytes.Equal(data[36:68], want) {
			t.Errorf("金额 %s: 金额段 %x，期望 %x", amount, data[36:68], want)
		}
	}

	// nil 金额按 0 处理而不是 panic：零金额在 pay.CheckOrder 已经拦过一次，
	// 组包这一层不该因为上游没填就把进程打挂。
	if data := erc20TransferData(to, nil); !bytes.Equal(data[36:], make([]byte, 32)) {
		t.Errorf("nil 金额未按 0 编码: %x", data[36:])
	}

	bal := erc20BalanceOfData(to)
	if len(bal) != 4+32 {
		t.Fatalf("balanceOf 调用数据长度 %d，期望 36", len(bal))
	}
	if !bytes.Equal(bal[:4], erc20BalanceOfSelector) {
		t.Errorf("balanceOf 选择器 %x，期望 %x", bal[:4], erc20BalanceOfSelector)
	}
	if want := common.LeftPadBytes(to.Bytes(), 32); !bytes.Equal(bal[4:], want) {
		t.Errorf("balanceOf 地址段 %x，期望 %x", bal[4:], want)
	}
}

// TestTransferEventTopic 事件 topic0 的黄金值。
//
// 它是查单与反查的锚：算错一个字节，所有「本单的 Transfer 事件」都认不出来，
// 而认不出来的表现是「回执成功但未见 Transfer」——一笔已经成功的钱被报成 UNKNOWN。
func TestTransferEventTopic(t *testing.T) {
	const want = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	if got := transferEventTopic.Hex(); got != want {
		t.Fatalf("Transfer 事件 topic0 = %s，期望 %s", got, want)
	}
	if !bytes.Equal(erc20DecimalsSelector, []byte{0x31, 0x3c, 0xe5, 0x67}) {
		t.Errorf("decimals() 选择器不符: %x", erc20DecimalsSelector)
	}
}

// TestDecodeUint256 返回值必须恰好是一个 uint256。
//
// 空返回是「这个地址没有合约」的典型表现（节点返回 0x），当成 0 余额会把配置错误
// 报成「余额不足」；超长返回说明那不是标准 ERC-20，读前 32 字节等于拿一段
// 不知道含义的字节当余额。
func TestDecodeUint256(t *testing.T) {
	raw := common.LeftPadBytes(big.NewInt(123456789).Bytes(), 32)
	got, err := decodeUint256(raw)
	if err != nil {
		t.Fatalf("合法返回被拒: %v", err)
	}
	if got.Cmp(big.NewInt(123456789)) != 0 {
		t.Errorf("解出的值 %s，期望 123456789", got)
	}

	if _, err := decodeUint256(nil); err == nil {
		t.Error("空返回未被拒")
	}
	if _, err := decodeUint256([]byte{}); err == nil {
		t.Error("零长返回未被拒")
	}
	for _, n := range []int{1, 31, 33, 64} {
		if _, err := decodeUint256(make([]byte, n)); err == nil {
			t.Errorf("%d 字节返回未被拒（不是单个 uint256）", n)
		}
	}
}

// TestClassifySendError 广播错误的分档。
//
// 分错的方向代价不对称：把「已进池」误判成「没进池」，上游会重建一单（双付）；
// 把「没进池」误判成「说不清」，只是停在 UNKNOWN 等对账。所以只有节点在入池前
// 就拒的那几句进 rejectBeforePool，其余一律 rejectUnknown。
func TestClassifySendError(t *testing.T) {
	cases := []struct {
		err  string
		want rejectClass
	}{
		{"", rejectUnknown},
		{"i/o timeout", rejectUnknown},
		{"connection reset by peer", rejectUnknown},
		{"already known", rejectAlreadyKnown},
		{"known transaction: 0xabc", rejectAlreadyKnown},
		{"transaction already exists", rejectAlreadyKnown},
		{"nonce too low", rejectNonceUsed},
		{"nonce too low: next nonce 8, tx nonce 7", rejectNonceUsed},
		{"insufficient funds for gas * price + value", rejectBeforePool},
		{"intrinsic gas too low", rejectBeforePool},
		{"exceeds block gas limit", rejectBeforePool},
		{"invalid sender", rejectBeforePool},
		{"invalid chain id", rejectBeforePool},
		{"transaction underpriced", rejectBeforePool},
		{"replacement transaction underpriced", rejectBeforePool},
		{"gas price too low", rejectBeforePool},
		{"rlp: value size exceeds available input length", rejectBeforePool},
		{"oversized data", rejectBeforePool},
	}
	for _, cs := range cases {
		var err error
		if cs.err != "" {
			err = errString(cs.err)
		}
		if got := classifySendError(err); got != cs.want {
			t.Errorf("classifySendError(%q)=%v，期望 %v", cs.err, got, cs.want)
		}
	}
	// 大小写不敏感：各家节点的文案大小写不一。
	if got := classifySendError(errString("NONCE TOO LOW")); got != rejectNonceUsed {
		t.Errorf("大写文案未被识别: %v", got)
	}
}

// errString 是最小的 error 实现，避免为了造一个带文案的 error 去引 fmt。
type errString string

func (e errString) Error() string { return string(e) }

// TestIsReadRetryable 读侧只重试「接入点没答上来」，业务性失败不重试。
func TestIsReadRetryable(t *testing.T) {
	retryable := []error{
		errString("i/o timeout"),
		errString("connection refused"),
		errString("unexpected EOF"),
		errString("429 Too Many Requests"),
		context.DeadlineExceeded,
	}
	for _, err := range retryable {
		if !isReadRetryable(err) {
			t.Errorf("%v 应可重试", err)
		}
	}
	no := []error{
		nil,
		errString("execution reverted"),
		errString("invalid argument 0"),
		context.Canceled,
	}
	for _, err := range no {
		if isReadRetryable(err) {
			t.Errorf("%v 不该重试", err)
		}
	}
}

// TestGweiToWei 配置里的 gwei 价换 wei。
func TestGweiToWei(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{-1, "0"},
		{1, "1000000000"},
		{1.5, "1500000000"},
		{3, "3000000000"},
		{0.000000001, "1"},
		{0.1, "100000000"},
	}
	for _, cs := range cases {
		if got := gweiToWei(cs.in).String(); got != cs.want {
			t.Errorf("gweiToWei(%v)=%s，期望 %s", cs.in, got, cs.want)
		}
	}
}

// TestSaturateInt64 收窄大整数：超出即饱和，负数归 0。
func TestSaturateInt64(t *testing.T) {
	max := int64(^uint64(0) >> 1)
	cases := []struct {
		in   *big.Int
		want int64
	}{
		{nil, 0},
		{big.NewInt(0), 0},
		{big.NewInt(-5), 0},
		{big.NewInt(123), 123},
		{big.NewInt(max), max},
		{new(big.Int).Add(big.NewInt(max), big.NewInt(1)), max},
		{new(big.Int).Lsh(big.NewInt(1), 200), max},
	}
	for _, cs := range cases {
		if got := saturateInt64(cs.in); got != cs.want {
			t.Errorf("saturateInt64(%v)=%d，期望 %d", cs.in, got, cs.want)
		}
	}
}

// TestLooksLikeRPCURL 只判形态：写错协议前缀的接入点在装配期就该被拒。
func TestLooksLikeRPCURL(t *testing.T) {
	ok := []string{"http://127.0.0.1:8545", "https://bsc-dataseed.binance.org", "ws://n:1", "wss://n:1", "HTTP://X.Y"}
	for _, s := range ok {
		if !looksLikeRPCURL(s) {
			t.Errorf("%q 应被认作接入点", s)
		}
	}
	no := []string{"", "127.0.0.1:8545", "tcp://x:1", "file:///tmp/x", "bsc-dataseed"}
	for _, s := range no {
		if looksLikeRPCURL(s) {
			t.Errorf("%q 不该被认作接入点", s)
		}
	}
}

// TestBigString nil 大整数给 "0"，免得日志里出现 "<nil>"。
// （摘要截断 clip 已上移为 util.Clip，其用例见 util/string_test.go。）
func TestBigString(t *testing.T) {
	if bigString(nil) != "0" {
		t.Errorf("nil 大整数应给 0，实得 %q", bigString(nil))
	}
	if bigString(big.NewInt(42)) != "42" {
		t.Error("大整数字面量不符")
	}
}
