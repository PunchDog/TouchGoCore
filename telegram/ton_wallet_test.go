package telegram

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/wallet"
)

// 本文件钉的是「组包与签名」这一侧：那些能对着固定输入逐字测的部分。
// 出款链路真正危险的「广播之后发生了什么」在 ton_double_pay_test.go。

// tonTestSeed 是固定种子（0x00..0x1F），对应的公钥与派生地址一并抄在下面。
//
// 派生地址那几格是**防漂移钉**而不是独立真值：它由 tongo v1.24.1 算出后逐字抄在这里，
// 作用是「升级依赖后同一把私钥不许悄悄落成另一个账户」。账户本身是否真的对，
// 只能由连上链的读侧证明——那在 live_verify_ton_test.go（测试网读得到该地址的余额与序号）。
// 本文件里能做的独立交叉核对是另一件事：tongo 给的地址必须过本包那套
// 自己写的 base64url + CRC16 校验（两个实现互不共享代码，错不到一起去）。
func tonTestSeed() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

const (
	tonTestPubHex     = "03a107bff3ce10be1d70dd18e74bc09967e4d6309ba50d5f1ddc8664125531b8"
	tonV5R1Mainnet    = "0:f2801419aaa1b3704a0d50b5888dccb5511ed6e3191b99d144a04ea9ea390c52"
	tonV5R1Testnet    = "0:8c97513be09383a69657bdbe1cbc8d5aa664317ed9ea38593bf5f9862a05f78f"
	tonV4R2Mainnet    = "0:d371439c26fc2d331e11e956dbfae602b4910b0b3000bdd0f8dfd7dcd98d1436"
	tonV4R2SubZero    = "0:8da1b8c5b60e6137cf7baefaf28856ae41dd785e7c4b95dbea2eeda2e9293171"
	tonV3R2Mainnet    = "0:e4fbbdcecd98affdf7d701b65510664e7236c8c0103ee82cc7bbd538eb15239a"
	tonV5R1MainnetEq  = "EQDygBQZqqGzcEoNULWIjcy1UR7W4xkbmdFEoE6p6jkMUpdb"
	tonDeriveAddrV5R1 = tonV5R1Mainnet // 门面装配核对用的默认形态
)

func tonTestKey() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(tonTestSeed()) }

// TestTONPrivateKeyForms 两种 hex 形态都收，且后半段不一致的那种必须被拒。
func TestTONPrivateKeyForms(t *testing.T) {
	full := append(tonTestSeed(), tonTestKey().Public().(ed25519.PublicKey)...)
	key, err := tonPrivateKey(hex.EncodeToString(full))
	if err != nil {
		t.Fatalf("64 字节完整密钥应当可用: %v", err)
	}
	if hex.EncodeToString(key.Public().(ed25519.PublicKey)) != tonTestPubHex {
		t.Errorf("完整密钥推出的公钥与夹具不一致: %x", key.Public())
	}
	if _, err := tonPrivateKey(hex.EncodeToString(tonTestSeed())); err != nil {
		t.Errorf("32 字节种子应当可用: %v", err)
	}
	// 种子对、公钥段被换掉：签名会一路成功，只有链上会拒。必须在解析期就拒。
	bad := append(tonTestSeed(), make([]byte, ed25519.PublicKeySize)...)
	if _, err := tonPrivateKey(hex.EncodeToString(bad)); err == nil ||
		!strings.Contains(err.Error(), "后半段公钥") {
		t.Errorf("公钥段与种子不一致时应当拒，实得 %v", err)
	}
	for _, s := range []string{"", "zz", hex.EncodeToString(make([]byte, 31))} {
		if _, err := tonPrivateKey(s); err == nil {
			t.Errorf("私钥 %q 本不该可用却放行了", s)
		}
	}
}

// TestResolveTONWalletMatchesConfiguredAddress 装配期派生地址核对。
//
// 三种错都要撞在这道闸上：私钥与地址不是同一个账户、钱包版本点错、
// 以及 v3/v4 系漏配了非默认 sub_wallet_id。
func TestResolveTONWalletMatchesConfiguredAddress(t *testing.T) {
	key := tonTestKey()
	cases := []struct {
		name    string
		addr    string
		version string
		sub     *uint32
		network string
		wantErr string
	}{
		{"v5R1 主网自动探测", tonV5R1Mainnet, "", nil, "mainnet", ""},
		{"v5R1 主网点名版本", tonV5R1Mainnet, "v5R1", nil, "mainnet", ""},
		{"v5R1 测试网是另一个账户", tonV5R1Testnet, "", nil, "testnet", ""},
		{"v4R2 默认子账户", tonV4R2Mainnet, "v4R2", nil, "mainnet", ""},
		{"v4R2 显式子账户 0", tonV4R2SubZero, "v4R2", ptr32(0), "mainnet", ""},
		{"地址是 v5R1 却点名 v4R2", tonV5R1Mainnet, "v4R2", nil, "mainnet", "派生不出"},
		{"v4R2 的地址配了 sub_wallet_id=0", tonV4R2Mainnet, "v4R2", ptr32(0), "mainnet", "派生不出"},
		{"私钥与地址无关", tonAcctBounceable, "", nil, "mainnet", "派生不出"},
		{"版本名写错", tonV5R1Mainnet, "v5r2", nil, "mainnet", "不是 tongo 认识的名字"},
		{"高载版本不接", tonV5R1Mainnet, "highload_v2R2", nil, "mainnet", "不在本通道支持范围"},
	}
	for _, cs := range cases {
		w, _, err := resolveTONWallet(key, cs.addr, cs.version, cs.network, cs.sub)
		if cs.wantErr == "" {
			if err != nil {
				t.Errorf("%s：应当装配成功，实得 %v", cs.name, err)
				continue
			}
			// 装配成功的定义只有一条：拿这把私钥组出来的钱包，地址就是配置里那一格。
			if got := w.GetAddress(); !sameTONAddress(got.ToRaw(), cs.addr) {
				t.Errorf("%s：派生地址 %s 与配置 %s 不是同一个账户", cs.name, got.ToRaw(), cs.addr)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), cs.wantErr) {
			t.Errorf("%s：应报「%s」，实得 %v", cs.name, cs.wantErr, err)
		}
	}
}

func ptr32(v uint32) *uint32 { return &v }

// TestTONAddressCrossCheck tongo 给的地址必须过本包那套独立实现的校验。
//
// 两处实现只在「字节的来源」上重叠：本包自己解 base64url 与 CRC16，tongo 自己算状态散列。
// 任一边算错，同一个账户在两边就不是同一个账户——那时出款会发给一个谁也解不出私钥的地址。
func TestTONAddressCrossCheck(t *testing.T) {
	for _, raw := range []string{tonV5R1Mainnet, tonV4R2Mainnet, tonV3R2Mainnet, tonV5R1Testnet} {
		id, err := ton.ParseAccountID(raw)
		if err != nil {
			t.Fatalf("tongo 解不出自己的输出 %s: %v", raw, err)
		}
		human := id.ToHuman(true, false)
		if !IsValidTONAddress(human) {
			t.Errorf("%s 的用户友好形态 %s 过不了本包校验（CRC16 或标签字节两边不一致）", raw, human)
		}
		flags, ok := parseTONAddress(human)
		if !ok {
			t.Errorf("本包解不出 %s", human)
			continue
		}
		if !flags.Bounceable || flags.Testnet {
			t.Errorf("%s 应当是可退回主网形态，实得 bounce=%v testnet=%v", human, flags.Bounceable, flags.Testnet)
		}
		if back, ok := tonAccountID(human); !ok || back != id {
			t.Errorf("%s 绕一圈回来不是同一个账户", human)
		}
	}
}

// TestSignTONTransferSelfChecks 组包自检的三条活闸：漏填操作码要红、换公钥要红、
// 原始 hash 与归一化 hash 必须相等。
func TestSignTONTransferSelfChecks(t *testing.T) {
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", "mainnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	self := w.GetAddress()
	to, _ := tonAccountID(tonAcctBounceable)
	validUntil := time.Now().Add(tonWalletLifetime)

	prepared, err := signTONTransfer(w, ver, self, 7, to, 1_000_000_000, "memo", true, validUntil)
	if err != nil {
		t.Fatalf("正常组包不该失败: %v", err)
	}
	if len(prepared.Boc) == 0 || len(prepared.MsgHash) != 64 {
		t.Fatalf("广播字节或 hash 形状不对: boc=%d hash=%q", len(prepared.Boc), prepared.MsgHash)
	}
	if prepared.Seqno != 7 || !prepared.ValidUntil.Equal(validUntil) {
		t.Errorf("台账要记的序号/有效期必须就是签名那一刻用的值，实得 seqno=%d validUntil=%v", prepared.Seqno, prepared.ValidUntil)
	}
	// BOC 反序列化回来必须与本地算的 hash 一致：接入点收到的就是这一份字节。
	root, err := boc.DeserializeSingleRootBoc(prepared.Boc)
	if err != nil {
		t.Fatalf("广播字节反序列化不回来: %v", err)
	}
	back, err := root.Hash256()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(back[:]) != prepared.MsgHash {
		t.Errorf("BOC 回读 hash=%x 与台账记的 %s 不是同一笔", back[:], prepared.MsgHash)
	}

	// 活闸一：操作码。直接走 tongo 的 CreateMessageBody 而漏填 V5MsgType，
	// 签名与序列化都会成功——只有这一条断言能抓住它。
	noOpcode, err := w.CreateMessageBody(wallet.MessageConfig{Seqno: 7, ValidUntil: validUntil},
		wallet.SimpleTransfer{Amount: tlb.Grams(1), Address: to})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyTONBody(ver, noOpcode, extCellOf(t, self, noOpcode), key.Public().(ed25519.PublicKey)); err == nil ||
		!strings.Contains(err.Error(), "操作码") {
		t.Errorf("漏填 V5 操作码时自检应当红，实得 %v", err)
	}

	// 活闸二：验签。正确的必须绿，错公钥的必须红——否则上面那条红可能只是因为整道闸是死的。
	body, err := w.CreateMessageBody(wallet.MessageConfig{Seqno: 7, ValidUntil: validUntil, V5MsgType: wallet.V5MsgTypeSignedExternal},
		wallet.SimpleTransfer{Amount: tlb.Grams(1), Address: to})
	if err != nil {
		t.Fatal(err)
	}
	cell := extCellOf(t, self, body)
	pub := key.Public().(ed25519.PublicKey)
	if err := verifyTONBody(ver, body, cell, pub); err != nil {
		t.Errorf("正确报文与正确公钥的自检应当通过，实得 %v", err)
	}
	other := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	if err := verifyTONBody(ver, body, cell, other); err == nil ||
		!strings.Contains(err.Error(), "自检不过") {
		t.Errorf("换公钥后自检应当红，实得 %v", err)
	}
}

// extCellOf 把报文体包成外部消息的 cell（与签名路径同一条构造）。
func extCellOf(t *testing.T, self ton.AccountID, body *boc.Cell) *boc.Cell {
	t.Helper()
	ext, err := ton.CreateExternalMessage(self, body, nil, tlb.VarUInteger16{})
	if err != nil {
		t.Fatal(err)
	}
	c := boc.NewCell()
	if err := tlb.Marshal(c, ext); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestSignTONTransferHashesAgree 原始 hash 与归一化 hash 在本通道交出去的消息形态下必须相等。
//
// 不等意味着接入点用来认领 external message 的那个 hash 与我们台账里那个不是同一个，
// 届时查证会永远「查不到」——而实现里那道 fail-closed 分支平时是走不到的死代码，
// 这条用例就是把它走一次。
func TestSignTONTransferHashesAgree(t *testing.T) {
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", "mainnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _ := tonAccountID(tonAcctBounceable)
	for _, memo := range []string{"", "12345", strings.Repeat("a", 127), strings.Repeat("字", 64)} {
		for _, bounce := range []bool{true, false} {
			p, err := signTONTransfer(w, ver, w.GetAddress(), 3, to, 1_000_000, memo, bounce, time.Now().Add(time.Minute))
			if err != nil {
				t.Fatalf("memo=%d bounce=%v 组包失败: %v", len(memo), bounce, err)
			}
			root, err := boc.DeserializeSingleRootBoc(p.Boc)
			if err != nil {
				t.Fatal(err)
			}
			h, err := root.Hash256()
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(h[:]) != p.MsgHash {
				t.Errorf("memo=%d bounce=%v 台账 hash 与字节不一致", len(memo), bounce)
			}
		}
	}
}

// TestSignTONTransferBounceChangesBytes 可退回位必须真的进到签名里。
//
// 本包用自己的解析结果定这一位（tongo 根包会把 UQ 读成可退回）；这一条证明那一位
// 不是只存在变量里——它在字节上留下了差别，所以判错就是发了另一条消息。
func TestSignTONTransferBounceChangesBytes(t *testing.T) {
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", "mainnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _ := tonAccountID(tonAcctBounceable)
	mk := func(bounce bool) string {
		p, err := signTONTransfer(w, ver, w.GetAddress(), 1, to, 1_000_000, "same", bounce, time.Unix(1_700_000_000, 0))
		if err != nil {
			t.Fatal(err)
		}
		return p.MsgHash
	}
	if a, b := mk(true), mk(false); a == b {
		t.Fatal("可退回位没进签名：bounce 判错不会改变报文，那本包的地址形态判定就只是装饰")
	}
}

// TestSignTONTransferRejectsBadAmount 金额非正或大到溢出时就地拒。
func TestSignTONTransferRejectsBadAmount(t *testing.T) {
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", "mainnet", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, _ := tonAccountID(tonAcctBounceable)
	for _, amount := range []int64{0, -1} {
		if _, err := signTONTransfer(w, ver, w.GetAddress(), 1, to, amount, "", true, time.Now()); err == nil {
			t.Errorf("金额 %d 本不该发出去", amount)
		}
	}
}
