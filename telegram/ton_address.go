package telegram

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"touchgocore/pay"
)

// 本文件是 TON 侧的收款端校验，登记给 pay 的 ChainRule 用。
//
// TON 的用户友好地址自带 CRC16，格式判定的成本是一次 base64 解码加两次循环——
// 相比「钱进了一个谁也解不出私钥的地址」，这道校验便宜到没有理由省。

// TON 用户友好地址的标签字节取值。
//
// 0x11 与 0x51 是同一个账户的两种写法（差在 bit6，即「是否可退回」），出款侧
// 两者都得收：同一个交易所可能一会儿给 EQ 开头、一会儿给 UQ 开头。
// 0x80 是测试网标记位，可与前两者叠加（0x91 / 0xD1）。
const (
	tagBounceable     = 0x11
	tagNonBounceable  = 0x51
	tagTestnetBit     = 0x80
	friendlyAddrLen   = 48 // 36 字节的 base64url 长度，无填充
	friendlyAddrBytes = 36
)

// maxMemoRunes 是备注的长度上限。交易所给的 memo 基本都是短数字串，
// 放开到几百只会把「上游把整段 JSON 塞进 memo」这类事故带上报文。
const maxMemoRunes = 64

// crc16CCITTFalse 算 CRC-16/CCITT-FALSE：多项式 0x1021、初值 0xFFFF、不反射、
// 无异或输出。TON 用户友好地址末尾两字节就是它（大端）。
//
// 别照抄成 XMODEM（同样 0x1021，但初值是 0）：两者只在「123456789」这种
// 自检串上差一个常量，真实地址上却是全表错，症状是所有合法地址都被判非法。
func crc16CCITTFalse(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// tonAddress 是解析后的收款端。
type tonAddress struct {
	// Workchain 是链分片号：基础链 0，主网主链 -1。
	Workchain int8
	// Testnet 来自标签字节的 0x80 位；原始十六进制形态没有这一位，为 false。
	Testnet bool
	// Bounceable 表示这笔转账失败后能否退回（EQ 形态）。
	Bounceable bool
	// HasTestnetFlag 标记这一形态是否真的带了测试网位——
	// 没有位的形态就没法与 PayOrder.Network 比对，不能当成「比过且一致」。
	HasTestnetFlag bool
}

// parseTONAddress 解析两种合法形态：
// 48 字符 base64url 的用户友好地址（带 CRC16 校验），以及 "0:" / "-1:" 前缀的
// 64 位十六进制原始形态。
//
// 用户友好形态只认 URL-safe 字母表（-_）。标准 base64 的 +/ 换过来看着只是一次
// 字符替换，实际会让同一个地址有两种写法，而其中一种在对面节点上根本查不到。
func parseTONAddress(s string) (*tonAddress, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if wc, raw, ok := strings.Cut(s, ":"); ok {
		return parseRawTONAddress(wc, raw)
	}
	if len(s) != friendlyAddrLen {
		return nil, false
	}
	buf, err := base64.URLEncoding.DecodeString(s)
	if err != nil || len(buf) != friendlyAddrBytes {
		return nil, false
	}
	tag := buf[0]
	switch tag & ^byte(tagTestnetBit) {
	case tagBounceable:
	case tagNonBounceable:
	default:
		return nil, false
	}
	want := crc16CCITTFalse(buf[:friendlyAddrBytes-2])
	got := uint16(buf[friendlyAddrBytes-2])<<8 | uint16(buf[friendlyAddrBytes-1])
	if want != got {
		return nil, false
	}
	return &tonAddress{
		Workchain:      int8(buf[1]),
		Testnet:        tag&tagTestnetBit != 0,
		Bounceable:     tag&^byte(tagTestnetBit) == tagBounceable,
		HasTestnetFlag: true,
	}, true
}

// parseRawTONAddress 处理 "workchain:64位十六进制" 形态。它没有校验和，
// 能判的只有分片号与长度，所以比用户友好形态弱一截。
func parseRawTONAddress(wc, hash string) (*tonAddress, bool) {
	if wc != "0" && wc != "-1" {
		return nil, false
	}
	if len(hash) != 64 {
		return nil, false
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return nil, false
	}
	workchain := int8(0)
	if wc == "-1" {
		workchain = -1
	}
	return &tonAddress{Workchain: workchain}, true
}

// IsValidTONAddress 判断地址形态是否合法（含 CRC16 校验，若为该形态）。
func IsValidTONAddress(s string) bool {
	_, ok := parseTONAddress(s)
	return ok
}

// tonRule 是 TON 一侧的收款端规则。原生 TON 与 jetton 共用同一套账户地址形态。
type tonRule struct{}

// CheckAddress 判形态，并把地址自带的测试网位与订单的 network 对齐。
//
// network 留空按主网口径处理：带测试网标记的地址此时会被拒。方向是选过的——
// 拒单一准错，把测试网地址送进主网报文则是不可逆的资损。
func (tonRule) CheckAddress(o *pay.PayOrder) error {
	addr := strings.TrimSpace(o.Address)
	if addr == "" {
		return nil
	}
	a, ok := parseTONAddress(addr)
	if !ok {
		return fmt.Errorf("TON 收款地址 %s 不合法：既不是 48 位用户友好地址（或 CRC16 校验不过），也不是 0: 或 -1: 开头的原始形态", addr)
	}
	if a.HasTestnetFlag && a.Testnet != pay.IsTestnet(o.Network) {
		if a.Testnet {
			return fmt.Errorf("TON 收款地址是测试网地址，但本单 network=%s", o.Network)
		}
		return fmt.Errorf("TON 收款地址是主网地址，但本单 network=%s", o.Network)
	}
	return nil
}

// CheckMemo 只校长度与字符集，不强制非空。
//
// 该不该必填取决于收款方是不是交易所的共用归集账户，那是业务侧的事实：
// 本包既看不到对方是谁，也没有一份能据此判断的地址表，硬要就会把所有
// 普通钱包提现一并拒掉。必填规则由下游在发起前按自己的账户名录把门。
func (tonRule) CheckMemo(o *pay.PayOrder) error {
	memo := strings.TrimSpace(o.Memo)
	if memo == "" {
		return nil
	}
	if utf8.RuneCountInString(memo) > maxMemoRunes {
		return fmt.Errorf("备注长度 %d 超过上限 %d", utf8.RuneCountInString(memo), maxMemoRunes)
	}
	// 控制字符（换行尤其）会顺着报文进到对方的对账行与日志里，
	// 一条备注变成两行记录是能改变别人账本形状的。
	for _, r := range memo {
		if unicode.IsControl(r) {
			return errors.New("备注含控制字符，可能是拼接时带进了换行")
		}
	}
	return nil
}

func init() {
	// 登记失败只可能是本包把币种名写错了，让它炸在启动期。
	if err := pay.RegisterChainRule(pay.CurrencyTON, tonRule{}); err != nil {
		panic(err)
	}
}
