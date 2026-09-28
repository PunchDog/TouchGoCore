package bsc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/sha3"

	"touchgocore/pay"
)

// 本文件是 BSC（EVM 形态）侧的收款端校验，登记给 pay 的 ChainRule 用。
//
// 之所以写在这个包而不是 pay：地址怎么编码、校验和怎么算，是 EVM 那条链的知识，
// 不是资金契约的知识。EVM 地址自带大小写校验和（EIP-55），白送的错误拦截——
// 一次手抖改了一个字母的大小写，都能在发出请求之前变成一条报错。

// evmAddressHexLen 是 EVM 地址去掉 0x 前缀后的十六进制长度（20 字节）。
const evmAddressHexLen = 40

// evmChecksumHash 对地址小写串取 Keccak-256（不是 NIST SHA3-256，二者填充率不同），
// EIP-55 的大小写规则就是以这个摘要为种子的。
func evmChecksumHash(lower string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lower))
	return h.Sum(nil)
}

// toEIP55 把 40 位十六进制地址（不含前缀、可带大小写）转成 EIP-55 校验和形态：
// 摘要对应 nibble >= 8 的字母大写，其余小写。
func toEIP55(hex40 string) string {
	lower := strings.ToLower(hex40)
	sum := evmChecksumHash(lower)
	out := []byte(lower)
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c < 'a' || c > 'f' {
			continue
		}
		nibble := sum[i/2]
		if i%2 == 0 {
			nibble >>= 4
		}
		if nibble >= 8 {
			out[i] = c - 32
		}
	}
	return string(out)
}

// IsValidEVMAddress 判断是否为格式合法的 EVM 地址（0x + 40 位十六进制）。
//
// 大小写口径：全小写与全大写是历史遗留形态，不携带校验和信息，格式过关即放行；
// 一旦出现混合大小写，就按 EIP-55 逐字符核校验和——混合大小写却不等于校验和形态，
// 几乎必然是复制途中被改过大小写，这正是校验和要拦的那类错。
//
// 它只回答「这是不是一个合法的 EVM 地址」，不回答「这个地址背后有没有账户」——
// 后者要问链上节点，而格式正确的地址指向无人持有的账户，钱同样找不回来。
func IsValidEVMAddress(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 2+evmAddressHexLen || !strings.HasPrefix(s, "0x") {
		return false
	}
	body := s[2:]
	if _, err := hex.DecodeString(body); err != nil {
		return false
	}
	hasUpper, hasLower := false, false
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c >= 'A' && c <= 'F':
			hasUpper = true
		case c >= 'a' && c <= 'f':
			hasLower = true
		}
	}
	// 无字母或单一大小写：不带校验和语义，格式过关即可。
	if !(hasUpper && hasLower) {
		return true
	}
	return body == toEIP55(body)
}

// bnbRule 是 BSC 一侧的收款端规则：原生 BNB 与同链的 BEP20 代币共用地址形态，
// 但 BNB 的登记归本包（链的原生币规则归属链包）。
type bnbRule struct{}

// CheckAddress 空地址放行：充值侧的收款端由供应商返回，提现侧的非空检查
// 在 pay.CheckOrder 里已经做过，这里只管格式。
func (bnbRule) CheckAddress(o *pay.PayOrder) error {
	addr := strings.TrimSpace(o.Address)
	if addr == "" {
		return nil
	}
	if !IsValidEVMAddress(addr) {
		return fmt.Errorf("BSC 收款地址 %s 不合法：应为 0x 开头的 40 位十六进制，混合大小写时还须过 EIP-55 校验和", addr)
	}
	return nil
}

// CheckMemo 拒绝任何备注：EVM 转账没有 memo 这个概念。
//
// 填了就拒而不是忽略：上游给不许带备注的链填上备注，说明它把 TON 的规则套错了对象，
// 这种错配在 TON 侧表现为「钱进了别人账户」，在这里静默吞掉等于放它去别处发作。
func (bnbRule) CheckMemo(o *pay.PayOrder) error {
	if strings.TrimSpace(o.Memo) != "" {
		return errors.New("BSC 转账不支持备注（memo）：交易所归集账户的认款机制是 TON 那条链的事")
	}
	return nil
}

func init() {
	// 登记失败只可能是本包把币种名写错了，让它炸在启动期。
	if err := pay.RegisterChainRule(pay.CurrencyBNB, bnbRule{}); err != nil {
		panic(err)
	}
}
