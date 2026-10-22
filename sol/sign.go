package sol

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
)

// 本文件是 Solana 侧的离线签名器：私钥解析、派生地址比对、交易签名。
//
// 签名只在本机发生，私钥不出门（与 tron / bsc 两条链同一个口径）。Solana 的签名
// 是 ed25519，且交易的身份就是第一个签名：签名一旦产出，交易哈希（base58 签名）
// 就定了，区块里不会出现两笔同签名的交易。因此「同一单号永不签第二笔」在本链上
// 等价于「同一份已签名字节只广播一次，或按签名查证」。

// signer 是「一把 ed25519 私钥 + 一个链上地址」的组合。
//
// 两者放在同一个结构里而不是各传一个参数：地址是私钥的函数，分开传就有
// 「按 A 的地址记账、用 B 的私钥签名」这种组合存在，而那种错在链上不留痕迹——
// 交易由 B 的钱包付钱，台账却写着 A。
type signer struct {
	key  solana.PrivateKey
	addr solana.PublicKey
}

// newSigner 解析私钥并要求派生地址与配置地址是同一个账户。
//
// 两条都在装配期判：
//   - 私钥解不出来：半成品通道只会在第一笔出款时炸，而那一刻上游已经在等结果；
//   - 派生地址与 wallet_address 不等：那是「按 A 的地址记账、用 B 的私钥出款」，
//     这种错在链上不留任何可查痕迹，只能靠装配期拒掉。
func newSigner(privateKey, wallet string) (*signer, error) {
	sgn, err := newSignerFromKey(privateKey)
	if err != nil {
		return nil, err
	}
	wallet = strings.TrimSpace(wallet)
	if !IsValidSolanaAddress(wallet) {
		// 先过格式再看派生地址：PublicKeyFromBase58 对解不出来的串会返回零值，
		// 直接拿它比较就会把「配置里写了个错地址」读成「派生地址和它不等」。
		return nil, fmt.Errorf("chain.wallet_address %q 不是合法的 Solana 地址（Base58 32 字节）", wallet)
	}
	want, err := solana.PublicKeyFromBase58(wallet)
	if err != nil {
		return nil, fmt.Errorf("chain.wallet_address %q 不可解析: %w", wallet, err)
	}
	if !sgn.addr.Equals(want) {
		return nil, fmt.Errorf("私钥派生地址 %s 与 chain.wallet_address %s 不是同一个账户", sgn.addr, want)
	}
	return sgn, nil
}

// newSignerFromKey 用任意一把私钥构造签名器，不与配置钱包比对。
//
// 按单出款时，钱包地址由 From.Address 与本函数的派生结果比对；配置热钱包
// 只在该单没有携带私钥时参与。
func newSignerFromKey(privateKey string) (*signer, error) {
	key, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	return &signer{key: key, addr: key.PublicKey()}, nil
}

// parsePrivateKey 解析 Solana 私钥，支持 base58（钱包导出）与 hex（部署脚本）两种形态。
//
// hex 允许 64 字节完整私钥或 32 字节 seed：前者是多数钱包与 solana-keygen 的
// 内存形态，后者是助记词派生出来的那一段。两种都能唯一确定一个 ed25519 密钥对，
// 因此不该让运维先自己换算一次——换算脚本里多写一个字节就是另一把私钥。
func parsePrivateKey(raw string) (solana.PrivateKey, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errors.New("链上私钥为空（应为 util.CallPayChainMsg+段名 注入）")
	}
	if key, err := solana.PrivateKeyFromBase58(s); err == nil && len(key) == ed25519.PrivateKeySize {
		return key, nil
	}
	body := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if b, err := hex.DecodeString(body); err == nil {
		switch len(b) {
		case ed25519.PrivateKeySize:
			return solana.PrivateKey(append([]byte(nil), b...)), nil
		case ed25519.SeedSize:
			return solana.PrivateKey(ed25519.NewKeyFromSeed(b)), nil
		}
	}
	// 原文不写进错误：私钥不得以任何形式出现在返回值或日志里。
	return nil, errors.New("链上私钥不可解析：应为 Base58 编码的 64 字节私钥，或 32/64 字节十六进制")
}

// Address 返回这把私钥对应的地址（装配期已与配置比对过）。
func (s *signer) Address() solana.PublicKey { return s.addr }

// Sign 用 ed25519 给一笔已组好的交易签名。
//
// 它把签名失败统一包成一个错误：调用方会把这一单记成 sign_failed，
// 而「签不出来」意味着这一单从未离开本机，允许同单号重建。
func (s *signer) Sign(tx *solana.Transaction) error {
	if tx == nil {
		return errors.New("待签名交易为 nil")
	}
	key := s.key
	owner := s.addr
	if _, err := tx.Sign(func(pk solana.PublicKey) *solana.PrivateKey {
		if pk.Equals(owner) {
			return &key
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ed25519 签名失败: %w", err)
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0].IsZero() {
		return errors.New("签名后交易没有第一个签名（交易身份缺失）")
	}
	return nil
}

// txSignature 返回交易身份：第一个签名的 base58 形态。
//
// 它就是台账里的 TxID。Solana 的「按哈希查证」查的是这个签名，而不是
// EVM 那种对 RLP 取哈希——两者在台账里同名列，含义各自按链解释。
func txSignature(tx *solana.Transaction) string {
	if tx == nil || len(tx.Signatures) == 0 {
		return ""
	}
	return tx.Signatures[0].String()
}
