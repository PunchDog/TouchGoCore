package usdt

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"touchgocore/pay"
)

// 本文件是 TRON 侧的收款端校验，登记给 pay 的 ChainRule 用。
//
// 之所以写在这个包而不是 pay：地址怎么编码是 TRON 的知识，不是资金契约的知识。
// 之所以值得写：TRON 地址自带 4 字节校验和，白送的错误拦截——一次手抖、一次
// 上游拼接漏字，都能在发出请求之前变成一条报错，而不是一笔进了无人地址的钱。

// base58Alphabet 是比特币系（含 TRON）的 Base58 字母表：剔掉了易混的 0OIl。
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// tronAddressVersion 是 TRON 账户地址的版本字节，恒为 0x41。
// 地址总长 25 字节 = 1 版本 + 20 账户哈希 + 4 校验和，Base58 之后是 34 个字符。
const tronAddressVersion = 0x41

// base58Decode 把 Base58 串解成字节。前导 '1' 表示一个 0x00 字节——
// big.Int 会吞掉高位零，所以这段必须单独补，否则 25 字节会莫名其妙少一截。
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

// tronChecksum 取双次 SHA256 的前 4 字节，这就是 TRON 地址末尾那 4 字节的来历。
func tronChecksum(payload []byte) []byte {
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	return second[:4]
}

// IsValidTRONAddress 判断是否为格式与校验和都通过的 TRON 账户地址。
//
// 它只回答「这是不是一个 TRON 地址」，不回答「这个地址存在吗」——后者要问全节点，
// 而一条格式正确的地址指向无人持有的账户，钱同样是找不回来的。
func IsValidTRONAddress(s string) bool {
	raw, err := base58Decode(strings.TrimSpace(s))
	if err != nil || len(raw) != 25 || raw[0] != tronAddressVersion {
		return false
	}
	return bytes.Equal(tronChecksum(raw[:21]), raw[21:])
}

// tronRule 是 TRON 一侧的收款端规则：USDT-TRC20 与原生 TRX 共用地址形态。
type tronRule struct{}

// CheckAddress 空地址放行：充值侧的收款端由供应商返回，提现侧的非空检查
// 在 pay.CheckOrder 里已经做过，这里只管格式。
func (tronRule) CheckAddress(o *pay.PayOrder) error {
	addr := strings.TrimSpace(o.Address)
	if addr == "" {
		return nil
	}
	if !IsValidTRONAddress(addr) {
		return fmt.Errorf("TRON 收款地址 %s 不合法：长度、版本字节或末尾 4 字节校验和对不上", addr)
	}
	return nil
}

// CheckMemo 拒绝任何备注：TRC20 转账没有 memo 这个概念。
//
// 填了就拒而不是忽略：上游给不许带备注的链填上备注，说明它把 TON 的规则套错了对象，
// 这种错配在 TON 侧表现为「钱进了别人账户」，在这里静默吞掉等于放它去别处发作。
func (tronRule) CheckMemo(o *pay.PayOrder) error {
	if strings.TrimSpace(o.Memo) != "" {
		return errors.New("TRC20 转账不支持备注（memo）：交易所归集账户的认款机制是 TON 那条链的事")
	}
	return nil
}

func init() {
	// 登记失败只可能是本包把币种名写错了，让它炸在启动期。
	for _, currency := range []string{pay.CurrencyUSDT, pay.CurrencyTRX} {
		if err := pay.RegisterChainRule(currency, tronRule{}); err != nil {
			panic(err)
		}
	}
}
