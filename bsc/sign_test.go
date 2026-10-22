package bsc

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// 固定私钥 → 派生地址的黄金向量。
//
// 私钥取公开测试账户（Hardhat/Anvil 的 #0）：公开、可复算、与任何真实资金无关。
// 地址先由 crypto.PubkeyToAddress 现推一次后逐字钉在这里——用例要验证的是
// 「签名器算出来的地址等于配置地址」，不能让它自己算一个地址再给自己盖章。
const (
	testPrivKeyHex = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	testWallet     = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"
	// testChainID 是 BSC 主网的 chain id。
	testChainID = 56
)

// mustTestSigner 造一个已通过全部装配闸的签名器。
func mustTestSigner(t *testing.T) *signer {
	t.Helper()
	sgn, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), testWallet)
	if err != nil {
		t.Fatalf("夹具签名器构造失败: %v", err)
	}
	return sgn
}

// TestParsePrivateKey 私钥解析：容忍 0x 前缀与首尾空白，错形态一律拒且不回显原文。
func TestParsePrivateKey(t *testing.T) {
	want := common.HexToAddress(testWallet)
	ok := []struct {
		desc string
		in   string
	}{
		{"裸 hex", testPrivKeyHex},
		{"0x 前缀", "0x" + testPrivKeyHex},
		{"0X 前缀", "0X" + testPrivKeyHex},
		{"大写 hex", strings.ToUpper(testPrivKeyHex)},
		{"首尾空白", "  " + testPrivKeyHex + "\n"},
	}
	for _, cs := range ok {
		key, err := parsePrivateKey(cs.in)
		if err != nil {
			t.Errorf("%s: 解析失败: %v", cs.desc, err)
			continue
		}
		if got := crypto.PubkeyToAddress(key.PublicKey); got != want {
			t.Errorf("%s: 派生地址 %s，期望 %s", cs.desc, got.Hex(), want.Hex())
		}
	}

	bad := []struct {
		desc string
		in   string
	}{
		{"空串", ""},
		{"只有空白", "   "},
		{"只有前缀", "0x"},
		{"少一字节", testPrivKeyHex[:62]},
		{"多一字节", testPrivKeyHex + "00"},
		{"含非十六进制字符", "0x" + testPrivKeyHex[:60] + "zz"},
		{"全零", strings.Repeat("0", 64)},
		{"超出曲线阶", strings.Repeat("ff", 32)},
	}
	for _, cs := range bad {
		if _, err := parsePrivateKey(cs.in); err == nil {
			t.Errorf("%s: 应被拒却解析成功", cs.desc)
		}
	}

	// 报错不得带出私钥原文：私钥不许以任何形式出现在返回值里（它会进日志）。
	_, err := parsePrivateKey("0x" + testPrivKeyHex[:58] + "zz")
	if err == nil {
		t.Fatal("非法私钥未被拒")
	}
	if strings.Contains(err.Error(), testPrivKeyHex[:16]) {
		t.Errorf("错误文案回显了私钥片段: %v", err)
	}
}

// TestNewSignerRequiresMatchingWallet 装配期三道闸：链 ID、私钥、派生地址。
//
// 三者都必须在装配期判死：任何一条留到第一笔出款才暴露，代价都是「钱已经动了一部分」。
func TestNewSignerRequiresMatchingWallet(t *testing.T) {
	if _, err := newSigner(testPrivKeyHex, nil, testWallet); err == nil {
		t.Error("chain_id 为 nil 未被拒")
	}
	if _, err := newSigner(testPrivKeyHex, big.NewInt(0), testWallet); err == nil {
		t.Error("chain_id 为 0 未被拒")
	}
	if _, err := newSigner(testPrivKeyHex, big.NewInt(-56), testWallet); err == nil {
		t.Error("chain_id 为负未被拒")
	}
	if _, err := newSigner("", big.NewInt(testChainID), testWallet); err == nil {
		t.Error("空私钥未被拒")
	}

	// 私钥 A 配地址 B：必须拒，且错误里两个地址都要出现（否则运维不知道该改哪边）。
	_, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), evmChecksumMixed2)
	if err == nil {
		t.Fatal("派生地址与配置地址不等时未被拒")
	}
	if !strings.Contains(err.Error(), "不是同一个账户") {
		t.Errorf("拒因文案不符: %v", err)
	}

	// 地址形态不合法要在「比派生地址」之前就拒：common.HexToAddress 对解不出来的串
	// 会安静地返回零地址，直接比会把它读成「派生地址和它不等」。
	if _, err := newSigner(testPrivKeyHex, big.NewInt(testChainID), "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"); err == nil {
		t.Error("非法 EVM 地址未被拒")
	} else if !strings.Contains(err.Error(), "不是合法的 EVM 地址") {
		t.Errorf("非法地址的拒因文案不符: %v", err)
	}

	sgn := mustTestSigner(t)
	if sgn.Address() != common.HexToAddress(testWallet) {
		t.Errorf("成功构造的签名器地址 %s，期望 %s", sgn.Address().Hex(), testWallet)
	}
	if sgn.chainID.Int64() != testChainID {
		t.Errorf("签名器链 ID = %s，期望 %d", sgn.chainID, testChainID)
	}
}

// TestSignerEIP155 签名必须与链 ID 绑定：本链 ID 可恢复出发送方，换链 ID 恢复不出来。
//
// 只断言「签名没报错」是不够的：EIP-155 的 v 由链 ID 算出，链 ID 写错时本地签名
// 照样成功，而交易会被节点拒掉——或者更糟，在另一条 EVM 链上被接受。
func TestSignerEIP155(t *testing.T) {
	sgn := mustTestSigner(t)
	to := common.HexToAddress(evmAllLower)
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    7,
		GasPrice: big.NewInt(1_000_000_000),
		Gas:      defaultTransferGasLimit,
		To:       &to,
		Value:    big.NewInt(0),
		Data:     erc20TransferData(to, big.NewInt(1_234_567)),
	})
	signed, err := sgn.Sign(tx)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if got := signed.ChainId(); got == nil || got.Int64() != testChainID {
		t.Fatalf("签名后的链 ID = %v，期望 %d", got, testChainID)
	}
	from, err := types.Sender(types.NewEIP155Signer(big.NewInt(testChainID)), signed)
	if err != nil {
		t.Fatalf("按本链 ID 恢复发送方失败: %v", err)
	}
	if from != sgn.Address() {
		t.Errorf("恢复出的发送方 %s，期望 %s", from.Hex(), sgn.Address().Hex())
	}
	// 换一条同构链（Chapel，chain id 97）恢复：v 对不上，必须失败或落在别的地址上。
	if other, err := types.Sender(types.NewEIP155Signer(big.NewInt(97)), signed); err == nil && other == sgn.Address() {
		t.Error("换链 ID 后仍恢复出同一发送方：签名没有真正绑定链 ID")
	}

	// 台账按哈希查证的前提是「同一份 raw tx 重广播得到同一个哈希」：
	// 序列化往返之后哈希必须逐字不变，否则同单号第二次广播会变成另一笔。
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back := new(types.Transaction)
	if err := back.UnmarshalBinary(raw); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if txHash(back) != txHash(signed) {
		t.Errorf("序列化往返后哈希变了: %s → %s", txHash(signed), txHash(back))
	}
	h := txHash(signed)
	if h != strings.ToLower(h) || !strings.HasPrefix(h, "0x") || len(h) != 66 {
		t.Errorf("txHash 形态不符（应为全小写 0x + 64 位）: %s", h)
	}
	if txHash(nil) != "" {
		t.Error("nil 交易的哈希应为空串")
	}
}
