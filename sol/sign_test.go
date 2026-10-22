package sol

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"

	"touchgocore/ledger"
)

// 本文件是离线签名器的断言：私钥三种形态解析、派生地址比对、交易签名与身份。
//
// 全部是纯本地用例：签名器不该为「证明自己能用」去连一次节点，而链上行为
// 已经在 query/onchain 的判据里各自钉过。

// solTestSeed 是测试用的确定性 seed（1..32）。用固定字节而不是随机密钥：
// 失败时的地址每次都一样，能直接拿去核对，不必先复现一次随机。
func solTestSeed() []byte {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return seed
}

// solTestKey 返回 seed 对应的 64 字节 ed25519 私钥。
func solTestKey(t *testing.T) solana.PrivateKey {
	t.Helper()
	return solana.PrivateKey(ed25519.NewKeyFromSeed(solTestSeed()))
}

// solTestSigner 返回一个已通过派生地址核对的签名器。
func solTestSigner(t *testing.T) *signer {
	t.Helper()
	key := solTestKey(t)
	s, err := newSigner(key.String(), key.PublicKey().String())
	if err != nil {
		t.Fatalf("用合法私钥构造签名器失败: %v", err)
	}
	return s
}

// solTestMint 返回一个确定的 mint 地址（与热钱包不同的另一把公钥）。
func solTestMint(t *testing.T) solana.PublicKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(101 + i)
	}
	return solana.PrivateKey(ed25519.NewKeyFromSeed(seed)).PublicKey()
}

// solTestChannel 构造一条不含接入点的通道，只供纯函数判据使用。
//
// rpc 字段留 nil 是刻意的：这些用例一旦开始依赖节点，就会在离线环境里静默跳过，
// 而「跳过的资金闸」与「没有这道闸」在验收上看起来一样。
func solTestChannel(t *testing.T) *onchainChannel {
	t.Helper()
	sgn := solTestSigner(t)
	mint := solTestMint(t)
	return &onchainChannel{
		sign: sgn,
		opt: channelOptions{
			Name:     "usdt",
			Network:  "mainnet-beta",
			Mint:     mint.String(),
			Decimals: defaultSOLDecimals,
		},
		chain: &chainConfig{
			WalletAddress: sgn.Address().String(),
			PrivateKey:    solTestKey(t).String(),
			RPC:           "https://example.invalid/rpc",
			APIKey:        "test-api-key",
		},
		ledger: ledger.New("sol-test"),
		mint:   mint,
		nowFn:  time.Now,
	}
}

// TestParsePrivateKeyForms 三种私钥形态必须解出同一把密钥。
//
// 这条断言的意义在运维侧：助记词派生给的是 32 字节 seed，钱包导出给的是 64 字节
// 完整私钥，base58 是钱包常见的文本形态。三种都能用时，「换算脚本多写一个字节」
// 这条路径才会消失。
func TestParsePrivateKeyForms(t *testing.T) {
	key := solTestKey(t)
	want := key.PublicKey().String()
	cases := []struct {
		name string
		raw  string
	}{
		{"base58 64 字节", key.String()},
		{"hex 64 字节", hex.EncodeToString(key)},
		{"hex 32 字节 seed", hex.EncodeToString(solTestSeed())},
		{"0x 前缀 hex seed", "0x" + hex.EncodeToString(solTestSeed())},
		{"两侧空白", "  " + key.String() + "\n"},
	}
	for _, cs := range cases {
		got, err := parsePrivateKey(cs.raw)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", cs.name, err)
			continue
		}
		if got.PublicKey().String() != want {
			t.Errorf("%s: 派生地址 %s，期望 %s", cs.name, got.PublicKey(), want)
		}
	}
}

// TestParsePrivateKeyRejects 认不出来的形态必须报错，且错误里不得出现原文。
func TestParsePrivateKeyRejects(t *testing.T) {
	key := solTestKey(t)
	short := hex.EncodeToString(key[:31])
	overlong := hex.EncodeToString(append(append([]byte(nil), key...), 0x00))
	for _, cs := range []struct {
		name string
		raw  string
	}{
		{"空串", ""},
		{"只有空白", "   "},
		{"hex 31 字节", short},
		{"hex 65 字节", overlong},
		{"非 base58 的串", "not-a-key-0OIl"},
	} {
		got, err := parsePrivateKey(cs.raw)
		if err == nil {
			t.Errorf("%s: 应当解析失败，实得 %d 字节", cs.name, len(got))
			continue
		}
		if cs.raw != "" && strings.Contains(err.Error(), cs.raw) {
			t.Errorf("%s: 错误文案把私钥原文带了出去: %v", cs.name, err)
		}
	}
}

// TestNewSignerRequiresMatchingWallet 派生地址与配置地址不等时必须拒绝装配。
//
// 这是本通道唯一能拦住「按 A 的地址记账、用 B 的私钥出款」的地方：那种错在链上
// 不留痕迹（交易由 B 付钱，台账写着 A），只能在装配期判。
func TestNewSignerRequiresMatchingWallet(t *testing.T) {
	key := solTestKey(t)
	if _, err := newSigner(key.String(), key.PublicKey().String()); err != nil {
		t.Fatalf("匹配的钱包地址被拒: %v", err)
	}
	if _, err := newSigner(key.String(), solBytes1To32); err == nil {
		t.Error("派生地址与配置地址不等时仍然装配成功")
	}
	if _, err := newSigner(key.String(), "not-an-address"); err == nil {
		t.Error("非法 wallet_address 未被拦下")
	}
	if _, err := newSigner("", key.PublicKey().String()); err == nil {
		t.Error("空私钥未被拦下")
	}
}

// TestSignProducesStableIdentity 同一份交易字节签两次必须得到同一个签名。
//
// Solana 的交易身份就是第一个签名，ed25519 是确定性签名：同一份 message 签两次
// 结果逐字节相同。这正是「同一单号重建后如果拿到同一个签名，它仍然只有一笔」
// 的依据，也让台账里的 TxID 可以被复算核对。
func TestSignProducesStableIdentity(t *testing.T) {
	sgn := solTestSigner(t)
	inst := computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()
	build := func() *solana.Transaction {
		t.Helper()
		tx, err := solana.NewTransactionBuilder().
			SetRecentBlockHash(solana.Hash{1}).
			SetFeePayer(sgn.Address()).
			AddInstruction(inst).
			Build()
		if err != nil {
			t.Fatalf("组包失败: %v", err)
		}
		return tx
	}
	tx1, tx2 := build(), build()
	if err := sgn.Sign(tx1); err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if err := sgn.Sign(tx2); err != nil {
		t.Fatalf("第二次签名失败: %v", err)
	}
	if len(tx1.Signatures) == 0 || tx1.Signatures[0].IsZero() {
		t.Fatal("签名后交易没有第一个签名")
	}
	if !tx1.Signatures[0].Equals(tx2.Signatures[0]) {
		t.Error("同一份交易签两次得到了不同签名：交易身份不再稳定")
	}
	if got, want := txSignature(tx1), tx1.Signatures[0].String(); got != want {
		t.Errorf("txSignature=%s，期望 %s", got, want)
	}
	// 签名必须能被公钥验过：这一步才真正证明它是这把私钥产的，而不是一段随机字节。
	msg, err := tx1.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("交易 message 不可序列化: %v", err)
	}
	if !ed25519.Verify(ed25519.PublicKey(sgn.Address().Bytes()), msg, tx1.Signatures[0][:]) {
		t.Error("签名无法用热钱包公钥验过")
	}
}

// TestSignRejectsForeignPayer 交易的第一签名人不是本钱包时必须拒绝签名。
//
// 拒的理由是资金面而不是格式：那种交易由别的账户付钱，本包的余额预检与台账
// 都会记到本钱包头上，签出来的结果在链上却指向另一个主体。
func TestSignRejectsForeignPayer(t *testing.T) {
	sgn := solTestSigner(t)
	foreign, err := solana.PublicKeyFromBase58(solBytes1To32)
	if err != nil {
		t.Fatalf("夹具地址不可解析: %v", err)
	}
	tx, err := solana.NewTransactionBuilder().
		SetRecentBlockHash(solana.Hash{2}).
		SetFeePayer(foreign).
		AddInstruction(computebudget.NewSetComputeUnitLimitInstruction(1_000).Build()).
		Build()
	if err != nil {
		t.Fatalf("组包失败: %v", err)
	}
	if err := sgn.Sign(tx); err == nil {
		t.Error("付款人不是本钱包的交易仍然签名成功")
	}
	if err := sgn.Sign(nil); err == nil {
		t.Error("nil 交易未被拦下")
	}
	if got := txSignature(nil); got != "" {
		t.Errorf("nil 交易的身份应为空串，实得 %q", got)
	}
}

// TestTxSignatureEmpty 空交易的身份是空串而不是 panic。
func TestTxSignatureEmpty(t *testing.T) {
	if got := txSignature(&solana.Transaction{}); got != "" {
		t.Errorf("无签名交易的身份应为空串，实得 %q", got)
	}
}
