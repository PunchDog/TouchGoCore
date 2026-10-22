package bsc

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// 本文件是 BSC 侧的离线签名器：私钥解析、派生地址比对、EIP-155 签名。
//
// 签名只在本机发生，私钥不出门（与 tron 包的 softSigner 同一个口径）。区别在于
// EVM 的签名与链 ID 绑在一起（EIP-155 的 v = chainID*2 + 35/36）：链 ID 写错时
// 本地签名照样成功，交易却会被节点按「签名不合法」拒掉——或者更糟，在另一条
// EVM 链上被接受。所以链 ID 与派生地址都在装配期钉死，不留到第一笔出款才发现。

// signer 是「一把私钥 + 一个链 ID」的组合。
//
// 它把链 ID 与私钥放在同一个结构里而不是各传一个参数：EVM 的签名值本身依赖链 ID，
// 两者分开传就有「用 A 链的 ID 签 B 链的交易」这种组合存在，而那种交易一旦被接受
// 就是发到了另一条链上，链上没有任何撤回手段。
type signer struct {
	key     *ecdsa.PrivateKey
	chainID *big.Int
	addr    common.Address
}

// newSigner 解析私钥、校验链 ID，并要求派生地址与配置地址是同一个账户。
//
// 三条都在装配期判，理由各不相同：
//   - 私钥解不出来：半成品通道只会在第一笔出款时炸，而那一刻上游已经在等结果；
//   - 链 ID 缺失或非正：EIP-155 的 v 要靠它算，缺了就只能签出节点不认的形态；
//   - 派生地址与 wallet_address 不等：那是「按 A 的地址记账、用 B 的私钥出款」，
//     这种错在链上不留任何可查痕迹，只能靠装配期拒掉。
func newSigner(privateKeyHex string, chainID *big.Int, wallet string) (*signer, error) {
	sgn, err := newSignerFromKey(privateKeyHex, chainID)
	if err != nil {
		return nil, err
	}
	wallet = strings.TrimSpace(wallet)
	if !IsValidEVMAddress(wallet) {
		// 先过格式再看派生地址：common.HexToAddress 对解不出来的串会安静地返回零地址，
		// 直接拿它比较就会把「配置里写了个错地址」读成「派生地址和它不等」。
		return nil, fmt.Errorf("chain.wallet_address %q 不是合法的 EVM 地址", wallet)
	}
	if want := common.HexToAddress(wallet); sgn.addr != want {
		return nil, fmt.Errorf("私钥派生地址 %s 与 chain.wallet_address %s 不是同一个账户", sgn.addr.Hex(), want.Hex())
	}
	return sgn, nil
}

// newSignerFromKey 用任意一把私钥构造签名器，不与配置钱包比对。
//
// 按单出款时，钱包地址由 From.Address 与本函数的派生结果逐字比对；配置热钱包
// 只在该单没有携带私钥时参与。链 ID 仍必须是配置那段——它决定 EIP-155 的 v。
func newSignerFromKey(privateKeyHex string, chainID *big.Int) (*signer, error) {
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, errors.New("chain_id 缺失或非正：EIP-155 签名必须知道链 ID")
	}
	key, err := parsePrivateKey(privateKeyHex)
	if err != nil {
		return nil, err
	}
	return &signer{key: key, chainID: new(big.Int).Set(chainID), addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

// AddressOfKey 用私钥派生 EVM 地址，不需要 chain ID。
//
// 供上游在保存线路时核对「私钥派生的地址 == from_address」；正式出款仍由
// newSignerFromKey + assetOf 再核一遍，这里不是安全边界的替代品。
func AddressOfKey(privateKeyHex string) (string, error) {
	key, err := parsePrivateKey(privateKeyHex)
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(key.PublicKey).Hex(), nil
}

// parsePrivateKey 解析 hex 形态的私钥，容忍 0x 前缀。
//
// 容错前缀而不是让上游自己剥：部署脚本从各家钱包工具导出私钥时形态不一，
// 而「带 0x 就解析失败」这种报错会让人以为是私钥错了。
func parsePrivateKey(privateKeyHex string) (*ecdsa.PrivateKey, error) {
	s := strings.TrimSpace(privateKeyHex)
	if s == "" {
		return nil, errors.New("链上私钥为空（应为 util.CallPayChainMsg+段名 注入）")
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	key, err := crypto.HexToECDSA(s)
	if err != nil {
		// 原文不写进错误：私钥不得以任何形式出现在返回值或日志里。
		return nil, errors.New("链上私钥不可解析：应为 32 字节十六进制")
	}
	return key, nil
}

// Address 返回这把私钥对应的地址（装配期已与配置比对过）。
func (s *signer) Address() common.Address { return s.addr }

// Sign 用 EIP-155 规则签名。
//
// 它把签名失败统一包成一个错误：调用方（prepare）会把这一单记成 sign_failed，
// 而「签不出来」意味着这一单从未离开本机，允许同单号重建。
func (s *signer) Sign(tx *types.Transaction) (*types.Transaction, error) {
	signed, err := types.SignTx(tx, types.NewEIP155Signer(s.chainID), s.key)
	if err != nil {
		return nil, fmt.Errorf("EIP-155 签名失败: %w", err)
	}
	return signed, nil
}

// txHash 返回交易哈希（0x 前缀的 EIP-55 无关形态，全小写 hex）。
//
// 它就是台账里的 TxID：EVM 的哈希由已签名交易的 RLP 决定，同一份 raw tx 重广播
// 得到的哈希逐字不变，因此「按哈希查证」与「同一单号只有一笔」是同一件事。
func txHash(tx *types.Transaction) string {
	if tx == nil {
		return ""
	}
	return strings.ToLower(tx.Hash().Hex())
}
