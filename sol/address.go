package sol

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"touchgocore/pay"
)

// 本文件是 Solana 侧的收款端校验，登记给 pay 的 ChainRule 用。
//
// 之所以写在这个包而不是 pay：地址怎么编码是 Solana 的知识，不是资金契约的知识。
// Solana 地址没有 TRON 那样的内嵌校验和——它就是 ed25519 公钥的 Base58 编码，
// 于是「长度恰好 32 字节 + 字母表合法」是本地能白送的拦截：短一个字符、
// 混进一个 0/O，都在发出请求之前变成报错，而不是一笔进了无人地址的钱。
//
// base58 解码在本包独立实现（约 20 行纯函数）而不是引 tron 包：跨包耦合一段
// 字母表实现没有回报，两条链的地址语义本来就不同（有/无校验和）。

// base58Alphabet 是比特币系（含 Solana）的 Base58 字母表：剔掉了易混的 0OIl。
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// solAddressLen 是 Solana 地址的字节长度：ed25519 公钥恰好 32 字节。
const solAddressLen = 32

// base58Decode 把 Base58 串解成字节。前导 '1' 表示一个 0x00 字节——
// big.Int 会吞掉高位零，所以这段必须单独补，否则 32 字节会莫名其妙少一截。
func base58Decode(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("空串")
	}
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	num := new(big.Int)
	base := big.NewInt(58)
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(base58Alphabet, s[i])
		if idx < 0 {
			return nil, fmt.Errorf("含非 Base58 字符 %q", s[i])
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(idx)))
	}
	body := num.Bytes()
	out := make([]byte, zeros+len(body))
	copy(out[zeros:], body)
	return out, nil
}

// IsValidSolanaAddress 判断是否为格式合法的 Solana 账户地址（32 字节 ed25519 公钥的
// Base58 编码）。
//
// 它只回答「这是不是一个 Solana 地址」，不回答「这个地址存在吗」，更不校验私钥
// 是否为合法群元素（那是链上程序的事）——格式正确而指向无人持有账户的地址，
// 钱同样找不回来。
func IsValidSolanaAddress(s string) bool {
	raw, err := base58Decode(strings.TrimSpace(s))
	if err != nil || len(raw) != solAddressLen {
		return false
	}
	return true
}

// solRule 是 Solana 一侧的收款端规则：原生 SOL 与 SPL 代币共用地址形态，
// 但 SOL 的登记归本包（链的原生币规则归属链包）。
type solRule struct{}

// CheckAddress 空地址放行：充值侧的收款端由供应商返回，提现侧的非空检查
// 在 pay.CheckOrder 里已经做过，这里只管格式。
func (solRule) CheckAddress(o *pay.PayOrder) error {
	addr := strings.TrimSpace(o.Address)
	if addr == "" {
		return nil
	}
	if !IsValidSolanaAddress(addr) {
		return fmt.Errorf("Solana 收款地址 %s 不合法：应为 Base58 编码且解出恰好 32 字节（ed25519 公钥形态）", addr)
	}
	return nil
}

// CheckMemo 拒绝任何备注：Solana 原生转账与 SPL 转账都没有 TON comment 那种
// 附在转账上的认款机制。
//
// 填了就拒而不是忽略：上游给不许带备注的链填上备注，说明它把 TON 的规则套错了对象，
// 这种错配在 TON 侧表现为「钱进了别人账户」，在这里静默吞掉等于放它去别处发作。
func (solRule) CheckMemo(o *pay.PayOrder) error {
	if strings.TrimSpace(o.Memo) != "" {
		return errors.New("Solana 转账不支持备注（memo）：交易所归集账户的认款机制是 TON 那条链的事")
	}
	return nil
}

func init() {
	// 登记失败只可能是本包把币种名写错了，让它炸在启动期。
	if err := pay.RegisterChainRule(pay.CurrencySOL, solRule{}); err != nil {
		panic(err)
	}
}
