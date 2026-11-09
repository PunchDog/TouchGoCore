package ton

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/wallet"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"touchgocore/config"
	"touchgocore/corectx"
	"touchgocore/ledger"
	"touchgocore/pay"
	"touchgocore/util"
)

func TestQueryAccountMapsChainState(t *testing.T) {
	cases := []struct {
		name      string
		balance   int64
		status    string
		wantState string
		wantAvail int64
	}{
		{"active 扣余量", 100_000_000, "active", pay.AcctActive, 90_000_000},
		{"余额等于余量", defaultTONFeeReserveNanoton, "active", pay.AcctActive, 0},
		{"余额低于余量", 5_000_000, "active", pay.AcctActive, 0},
		{"frozen", 100_000_000, "frozen", pay.AcctFrozen, 0},
		{"uninit", 100_000_000, "uninit", pay.AcctUnknown, 0},
		{"nonexist", 100_000_000, "nonexist", pay.AcctUnknown, 0},
		{"未知状态词", 100_000_000, "weird", pay.AcctUnknown, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestTONChannel(t, &fakeTONGateway{balance: tc.balance, status: tc.status}, nil)
			info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if info.Balance != tc.balance || info.Available != tc.wantAvail || info.Status != tc.wantState {
				t.Fatalf("映射不符: %+v", info)
			}
			if info.Currency != pay.CurrencyTON || strings.TrimSpace(info.MerchantID) == "" {
				t.Fatalf("缺币种/地址: %+v", info)
			}
			if !strings.Contains(info.RawNote, tc.status) {
				t.Fatalf("RawNote 没有保留链上状态词: %q", info.RawNote)
			}
		})
	}
}

func TestQueryOnchainAccountAddressAndAssetGates(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{balance: 100_000_000, status: "active"}, nil)
	if _, err := c.QueryOnchainAccount(context.Background(), &pay.OnchainAccountQuery{Address: tonAcctBounceable}); err != nil {
		t.Fatalf("合法地址查询失败: %v", err)
	}
	for _, q := range []*pay.OnchainAccountQuery{
		{Address: "not-a-ton-address"},
		{Contract: "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"},
		{Currency: pay.CurrencyUSDT},
	} {
		if _, err := c.QueryOnchainAccount(context.Background(), q); err == nil {
			t.Errorf("非法查询 %+v 未被拒", q)
		}
	}
	if _, err := c.QueryAccount(context.Background(), &pay.AccountQuery{Currency: pay.CurrencyUSDT}); err == nil {
		t.Error("非 TON 币种的账户查询未被拒")
	}
}

func TestQueryAccountRedactsGatewaySecret(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{acctErr: errors.New("401 with " + tonFakeAPIKey)}, nil)
	_, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
	pe, ok := pay.ProviderErrorOf(err)
	if !ok || pe.Code != "account_unread" || !pe.Retryable {
		t.Fatalf("读失败应为可重试 account_unread，实得 %v", err)
	}
	if strings.Contains(err.Error(), tonFakeAPIKey) {
		t.Fatal("API Key 进了错误文案")
	}
}

// TON 地址夹具。account 部分是 0x00..0x1F 递增字节，五种标签形态各编一次。
//
// 校验和由 tongo 的 AccountID.ToHuman 生成（另一套实现，与本包不共享一行代码），
// 而不是用本包自己的 CRC 现算：本包曾经把 CRC 抄成 CCITT-FALSE（初值错），
// 自编的夹具全部自证通过，直到拿 tongo 的输出对拍才暴露——用被测实现产出的向量
// 永远测不出被测实现的错，这一条就是那件事的补丁。
//
// 为什么仍要现场造而不是抄一个网上的地址：本包原先的 jetton 夹具（EQDtFpEw...BB1q）
// 就是编的——48 位、EQ 开头、workchain 0，肉眼全然正常，校验和却对不上。
// 抄来的向量一旦本身是编的，测试就永远藏不住这个错。
const (
	tonAcctBounceable  = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHx2j"
	tonAcctNonBounce   = "UQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH0Bm"
	tonAcctTestnet     = "kQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH6Yp"
	tonAcctTestnetNonB = "0QAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH_vs"
	tonAcctMasterchain = "Ef8AAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eH-Lr"
	tonAcctBadChecksum = "EQAAAQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHx2b"
	tonAcctRaw         = "0:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	tonAcctRawMaster   = "-1:000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

	// tonAcctRealWorld 是 tongo 官方示例里那条真实主网地址（一个真实存在的账户）。
	// 它不参与上面的形态表，只用来做下面那条「本包必须认得真地址」的断言——
	// 自编夹具在 CRC 抄错时照样自证通过，只有真地址能把它照出来。
	tonAcctRealWorld = "EQBszTJahYw3lpP64ryqscKQaDGk4QpsO7RO6LYVvKHSINS0"
)

// TestCRC16XMODEMCheckVector 钉住 CRC 的参数选择。
//
// XMODEM（多项式 0x1021、初值 0x0000）的 "123456789" 检值是 0x31C3；常被混用的
// CCITT-FALSE（同多项式、初值 0xFFFF）检值是 0x29B1。抄错初值的实现能编出地址、
// 也能验出自己的编造，唯独验不了任何真实地址——症状是全线拒单，最难往回查。
func TestCRC16XMODEMCheckVector(t *testing.T) {
	if got := crc16XMODEM([]byte("123456789")); got != 0x31C3 {
		t.Fatalf("crc16XMODEM(\"123456789\")=0x%04X，期望 0x31C3", got)
	}
	if got := crc16XMODEM(nil); got != 0x0000 {
		t.Fatalf("空输入的初值应为 0x0000，实得 0x%04X", got)
	}
	// 夹具自检：抄错的地址在这一步就会露馅，不用等到跑校验用例。
	for _, s := range []string{
		tonAcctBounceable, tonAcctNonBounce, tonAcctTestnet,
		tonAcctTestnetNonB, tonAcctMasterchain, tonAcctRealWorld,
	} {
		if !IsValidTONAddress(s) {
			t.Errorf("夹具地址 %s 校验不过，说明它是编的或本实现错了", s)
		}
	}
	// 真地址的第二道判据：本包解出的 workchain 必须是 0（主网 basechain）。
	a, ok := parseTONAddress(tonAcctRealWorld)
	if !ok || a.Workchain != 0 || !a.Bounceable || a.Testnet {
		t.Errorf("真地址 %s 解析结果不对: %+v ok=%v", tonAcctRealWorld, a, ok)
	}
	if tonAcctBadChecksum == tonAcctBounceable {
		t.Fatal("坏校验和夹具与好夹具相同，这道反例是空的")
	}
}

func TestParseTONAddressForms(t *testing.T) {
	cases := []struct {
		addr      string
		ok        bool
		testnet   bool
		bounce    bool
		hasFlag   bool
		workchain int8
		desc      string
	}{
		{tonAcctBounceable, true, false, true, true, 0, "EQ 主网可退回"},
		{tonAcctNonBounce, true, false, false, true, 0, "UQ 主网不可退回（同一账户）"},
		{tonAcctTestnet, true, true, true, true, 0, "kQ 测试网可退回"},
		{tonAcctTestnetNonB, true, true, false, true, 0, "0Q 测试网不可退回"},
		{tonAcctMasterchain, true, false, true, true, -1, "Ef 主链分片"},
		{tonAcctBadChecksum, false, false, false, false, 0, "末位改动，校验和不过"},
		{tonAcctRaw, true, false, false, false, 0, "原始十六进制形态"},
		{tonAcctRawMaster, true, false, false, false, -1, "主链原始形态"},
		{"0:" + strings.Repeat("0", 63), false, false, false, false, 0, "十六进制少一位"},
		{"0:" + strings.Repeat("z", 64), false, false, false, false, 0, "十六进制含非法字符"},
		{"2:" + strings.Repeat("0", 64), false, false, false, false, 0, "不存在的分片号"},
		{tonAcctBounceable[:47], false, false, false, false, 0, "用户友好地址少一位"},
		{strings.ReplaceAll(tonAcctTestnetNonB, "_", "/"), false, false, false, false, 0, "换成标准 base64 字母表"},
		{"", false, false, false, false, 0, "空串"},
	}
	for _, cs := range cases {
		if cs.desc == "换成标准 base64 字母表" && cs.addr == tonAcctTestnetNonB {
			// 夹具本身不含 URL-safe 字符时这条反例是空的（上一轮就因为它悄悄退化过）。
			t.Fatalf("字母表反例没生效：夹具 %s 里没有被替换的字符", tonAcctTestnetNonB)
		}
		a, ok := parseTONAddress(cs.addr)
		if ok != cs.ok {
			t.Errorf("%s: parseTONAddress(%q) ok=%v，期望 %v", cs.desc, cs.addr, ok, cs.ok)
			continue
		}
		if !ok {
			continue
		}
		if a.Testnet != cs.testnet || a.Bounceable != cs.bounce || a.HasTestnetFlag != cs.hasFlag {
			t.Errorf("%s: 标志位不符 testnet=%v bounce=%v hasFlag=%v", cs.desc, a.Testnet, a.Bounceable, a.HasTestnetFlag)
		}
		if a.Workchain != cs.workchain {
			t.Errorf("%s: workchain=%d，期望 %d", cs.desc, a.Workchain, cs.workchain)
		}
	}
}

// TestTonRuleMatchesTestnetBit 地址自带的测试网位必须与订单的 network 对齐。
func TestTonRuleMatchesTestnetBit(t *testing.T) {
	rule := tonRule{}
	okCases := []pay.PayOrder{
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctBounceable, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctNonBounce, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctTestnet, Network: pay.NetworkTestnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctRaw, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: ""}},
	}
	for i := range okCases {
		if err := rule.CheckAddress(&okCases[i]); err != nil {
			t.Errorf("合法组合被拒 %+v: %v", okCases[i], err)
		}
	}
	badCases := []pay.PayOrder{
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctTestnet, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctBounceable, Network: pay.NetworkTestnet}},
		{To: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctBounceable, Network: pay.NetworkShasta}},
	}
	for i := range badCases {
		if err := rule.CheckAddress(&badCases[i]); err == nil {
			t.Errorf("主网/测试网错配未被拦下 %+v", badCases[i])
		}
	}
	// network 留空按主网口径处理：测试网地址此时同样被拒，方向是宁可拒单不可错投。
	if err := rule.CheckAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet}}); err == nil {
		t.Error("network 留空时测试网地址应当被拒")
	}
}

func TestTonRuleMemo(t *testing.T) {
	rule := tonRule{}
	if err := rule.CheckMemo(&pay.PayOrder{}); err != nil {
		t.Errorf("不带备注被拒: %v", err)
	}
	for _, memo := range []string{"8890", "复转到交易所 A 账户", strings.Repeat("x", maxMemoRunes)} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err != nil {
			t.Errorf("合法备注被拒 %q: %v", memo, err)
		}
	}
	for _, memo := range []string{strings.Repeat("x", maxMemoRunes+1), "第一行\n第二行", "tab\t分隔"} {
		if err := rule.CheckMemo(&pay.PayOrder{To: &pay.Party{Memo: memo}}); err == nil {
			t.Errorf("越界备注未被拦下（长度 %d）", len(memo))
		}
	}
}

// 本文件与 tron/usdt_assembly_test.go 同构：钉住内联装配（ton.native 一段写全）
// 的几道闸。内联之后没人替本包挡驱动名与密钥来源，所以必须各有一条用例。

const (
	tonEquivHTTPBase = "https://tonapi.invalid"
	tonEquivAPIKey   = "TON-API-KEY"
)

// tonInlineCfg 造一份字段齐备的内联形态 TON 配置。
func tonInlineCfg() *config.Cfg {
	privHex := hex.EncodeToString(tonTestSeed())
	return &config.Cfg{
		Ton: &config.TonConfig{Native: &config.TonNativeConfig{
			Enable:  "on",
			Driver:  DriverTonAPI,
			Network: pay.NetworkMainnet,
			Chain: &config.TonChainConfig{
				HTTPBase: tonEquivHTTPBase, APIKey: tonEquivAPIKey,
				WalletAddress: tonV5R1Mainnet, WalletVersion: "v5R1",
				PrivateKey: privHex, FeeLimit: 20_000_000,
				TimeoutSec: 45, MaxRetries: 2,
			},
		}},
	}
}

// mustTONChannel 走一遍真实装配入口并取回链上通道内部结构。
func mustTONChannel(t *testing.T, cfg *config.Cfg) *tonChannel {
	t.Helper()
	c, ok := newTONNativeClient(cfg)
	if !ok || c == nil {
		t.Fatal("装配失败")
	}
	ch, ok := c.Ch.(*tonChannel)
	if !ok {
		t.Fatalf("装配出的通道类型不是 *tonChannel: %T", c.Ch)
	}
	return ch
}

// TestTonInlineKeyHooks 注入钩子的键名：只认 segment ton.native，旧段名不再被读取。
//
// TON 没有供应商签名密钥了，只剩热钱包私钥一个钩子；旧键必须不再生效。
func TestTonInlineKeyHooks(t *testing.T) {
	privHex := hex.EncodeToString(tonTestSeed())

	cfg := tonInlineCfg()
	cfg.Ton.Native.Chain.PrivateKey = ""
	idKey := util.DefaultCallFunc.Register(util.CallPayChainMsg+inlineTONKey, func(pk *string) { *pk = privHex })
	ch := mustTONChannel(t, cfg)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+inlineTONKey, idKey)
	if ch.chain.PrivateKey != privHex {
		t.Fatalf("私钥钩子未按新键注入: PrivateKey=%q", ch.chain.PrivateKey)
	}

	// 旧键不再被读取：只注册旧键时，配置里仍为空，装配必须失败（没有私钥）。
	cfg = tonInlineCfg()
	cfg.Ton.Native.Chain.PrivateKey = ""
	idOld := util.DefaultCallFunc.Register(util.CallPayChainMsg+"telegram.ton", func(pk *string) { *pk = privHex })
	c, ok := newTONNativeClient(cfg)
	util.DefaultCallFunc.Unregister(util.CallPayChainMsg+"telegram.ton", idOld)
	if got := strings.TrimSpace(cfg.Ton.Native.Chain.PrivateKey); got != "" {
		t.Fatalf("旧键 telegram.ton 仍能注入私钥：配置里的 private_key=%q", got)
	}
	if ok || c != nil {
		t.Fatal("只有旧键时 TON 通道不该装配成功")
	}
}

// TestTonInlineRejectsWrongDriver 内联装配不经驱动注册表，驱动名必须自己把关。
func TestTonInlineRejectsWrongDriver(t *testing.T) {
	for _, driver := range []string{"", "generic_md5", "ton_tonapi_v2", "ton"} {
		cfg := tonInlineCfg()
		cfg.Ton.Native.Driver = driver
		if _, ok := newTONNativeClient(cfg); ok {
			t.Errorf("driver=%q 不应让内联通道启动", driver)
		}
	}
}

// TestTonInlineStillRejectsJetton jetton 与网络两道闸跑在链装配之前。
func TestTonInlineStillRejectsJetton(t *testing.T) {
	cfg := tonInlineCfg()
	cfg.Ton.Native.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTONNativeClient(cfg); ok {
		t.Error("配了 jetton 的 TON 通道不应启动")
	}
	// 通道没开也不该因为「反正不启动」就把 jetton 放过去：写错的配置要当场报出来。
	cfg = tonInlineCfg()
	cfg.Ton.Native.Enable = "off"
	cfg.Ton.Native.Jetton = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	if _, ok := newTONNativeClient(cfg); ok {
		t.Error("未开启的通道同样必须拒 jetton")
	}
}

// 本文件钉的是包内收口的 `checkOrderAddress`——它取代了原来登记在 pay 的
// ChainRule 表里的那条 TON 规则。
//
// 与 tron 侧同理：旧表「未登记即放行」，币种名拼错一个字母校验就整段消失；
// 现在规则是函数调用，漏接是编译错误。所以断言对象是这个收口函数本身，
// 而不是它内部用到的 tonRule——只测 rule 的话，把函数体改空依然全绿。

// TestTonCheckOrderAddressRejectsBadAddress 坏地址一定拒，合法地址必须放行。
func TestTonCheckOrderAddressRejectsBadAddress(t *testing.T) {
	bad := []string{
		tonAcctBadChecksum,
		"garbage",
		"EQ",            // 长度不够
		tonAcctRaw[:20], // 原始形态被截断
		"2:0001020304",  // 分片号越界
	}
	for _, addr := range bad {
		o := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: addr, Network: pay.NetworkMainnet}}
		if err := checkOrderAddress(o); err == nil {
			t.Errorf("地址 %q 未被拦下", addr)
		}
	}

	good := []string{tonAcctBounceable, tonAcctNonBounce, tonAcctRaw, tonAcctRawMaster}
	for _, addr := range good {
		if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: addr, Network: pay.NetworkMainnet}}); err != nil {
			t.Errorf("合法地址 %q 被拒: %v", addr, err)
		}
	}
}

// TestTonCheckOrderAddressMatchesNetwork 地址自带的测试网位必须与本单 network 对齐。
//
// 方向是选过的：network 留空按主网口径处理，带测试网标记的地址此时被拒。
// 拒单一准错，而把测试网地址送进主网报文是不可逆的资损。
func TestTonCheckOrderAddressMatchesNetwork(t *testing.T) {
	// 测试网地址 + 主网单：拒。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet, Network: pay.NetworkMainnet}}); err == nil {
		t.Error("测试网地址不能进主网单")
	}
	// 测试网地址 + network 留空：按主网口径，同样拒。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet}}); err == nil {
		t.Error("network 留空应按主网口径拒掉测试网地址")
	}
	// 测试网地址 + 测试网单：放行。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctTestnet, Network: pay.NetworkTestnet}}); err != nil {
		t.Errorf("测试网地址配测试网单被拒: %v", err)
	}
	// 主网地址 + 测试网单：拒（两边都得对上，不是只看一边）。
	if err := checkOrderAddress(&pay.PayOrder{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkTestnet}}); err == nil {
		t.Error("主网地址不能进测试网单")
	}
}

// TestTonCheckOrderAddressMemoLimits memo 只校长度与字符集，不强制非空——
// 该不该必填取决于收款方是不是交易所归集账户，那是业务侧的事实。
func TestTonCheckOrderAddressMemoLimits(t *testing.T) {
	base := func(memo string) *pay.PayOrder {
		return &pay.PayOrder{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: memo}}
	}
	if err := checkOrderAddress(base(strings.Repeat("1", maxMemoRunes))); err != nil {
		t.Errorf("恰好等于上限的备注被拒: %v", err)
	}
	if err := checkOrderAddress(base(strings.Repeat("1", maxMemoRunes+1))); err == nil {
		t.Error("超过上限的备注未被拦下")
	}
	// 控制字符会顺着报文进到对方的对账行与日志里，一条备注变成两行记录
	// 是能改变别人账本形状的。
	for _, memo := range []string{"12\n34", "12\r34", "\x00", "1\t2"} {
		if err := checkOrderAddress(base(memo)); err == nil {
			t.Errorf("含控制字符的备注 %q 未被拦下", memo)
		}
	}
	// 空与纯空白都算没有备注。
	for _, memo := range []string{"", "   ", "\n", "\t"} {
		if err := checkOrderAddress(base(memo)); err != nil {
			t.Errorf("空/纯空白备注 %q 被当成有值: %v", memo, err)
		}
	}
}

// TestTonCheckOrderAddressLeavesEmptinessToCheckOrder 空地址在这一层放行，
// 「收款方必填」归契约层；但必须钉住契约层确实还在判，否则两端一起放行。
func TestTonCheckOrderAddressLeavesEmptinessToCheckOrder(t *testing.T) {
	empty := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{}}
	if err := checkOrderAddress(empty); err != nil {
		t.Fatalf("格式层不该判非空: %v", err)
	}
	if err := pay.CheckOrder(empty); err == nil {
		t.Fatal("契约层必须拦住空收款地址")
	}
	if err := checkOrderAddress(nil); err != nil {
		t.Errorf("nil 订单不该报错: %v", err)
	}
	if err := checkOrderAddress(&pay.PayOrder{}); err != nil {
		t.Errorf("无收款方的订单不该在格式层报错: %v", err)
	}
}

// TestTonCheckOrderAddressCoversAllRules 收口函数必须同时用上地址与 memo 两条规则。
// 两条规则各有正例与反例，缺哪一条都会有一格对不上。
func TestTonCheckOrderAddressCoversAllRules(t *testing.T) {
	rule := tonRule{}
	cases := []*pay.PayOrder{
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Address: tonAcctBadChecksum, Network: pay.NetworkMainnet}},
		{To: &pay.Party{Address: "garbage"}},
		{To: &pay.Party{Address: tonAcctTestnet}},
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: strings.Repeat("9", maxMemoRunes+1)}},
		{To: &pay.Party{Address: tonAcctBounceable, Network: pay.NetworkMainnet, Memo: "12\n34"}},
		{To: &pay.Party{}},
		{},
	}
	for i, o := range cases {
		want := rule.CheckAddress(o)
		if want == nil {
			want = rule.CheckMemo(o)
		}
		got := checkOrderAddress(o)
		if (got == nil) != (want == nil) {
			t.Errorf("case %d: checkOrderAddress 与两条规则的组合不等价: got %v want %v", i, got, want)
		}
	}
}

// 本文件只测一件事：钱会不会被动两次。
//
// 每个用例都断言「签名次数」和「广播次数」，而不只是返回值：链上出款的正确性
// 不在「这一单返回了什么」，而在「说不清的时候本包有没有再去签第二笔」。
// 被测的网关是假的——把广播打成超时、打成 406、打成都读不出结果，这些形态
// 对着真链是没法稳定复现的，而这恰恰是最需要被固定的几支。

const (
	tonFakeAPIKey = "tonapi-key-should-never-appear-in-any-note"
	tonFakeMemo   = "8890"
)

// tonTestClock 是本文件统一的时间原点，让「过期/未过期」这两支可判而不是靠跑多快。
var tonTestClock = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// fakeTONGateway 是可以逐条指定结论的假接入点。
type fakeTONGateway struct {
	mu sync.Mutex

	balance  int64
	status   string
	acctErr  error
	seqnoVal uint32
	seqnoErr error

	sends    [][]byte
	sendResp *tonBroadcast
	sendErr  error

	txs     []tonTx
	txErr   error
	txCalls int

	closed bool
}

func (f *fakeTONGateway) seqno(context.Context, ton.AccountID) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seqnoVal, f.seqnoErr
}

func (f *fakeTONGateway) account(context.Context, ton.AccountID) (*tonAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acctErr != nil {
		return nil, f.acctErr
	}
	status := f.status
	if status == "" {
		status = "active"
	}
	if f.balance == 0 {
		f.balance = 1 << 40 // 默认「钱够」：本文件测的是出资次数，余额另有专门用例
	}
	return &tonAccount{BalanceNanoton: f.balance, Status: status}, nil
}

func (f *fakeTONGateway) sendExtMessage(_ context.Context, payload []byte) (*tonBroadcast, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]byte(nil), payload...)
	f.sends = append(f.sends, cp)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	if f.sendResp != nil {
		return f.sendResp, nil
	}
	return &tonBroadcast{Code: "200", Accepted: true}, nil
}

func (f *fakeTONGateway) transactions(context.Context, ton.AccountID, int) ([]tonTx, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txCalls++
	if f.txErr != nil {
		return nil, f.txErr
	}
	return f.txs, nil
}

func (f *fakeTONGateway) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTONGateway) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// uncertainBroadcast 是「请求可能已经到达网络」那一类：超时、5xx、限流都归它。
func uncertain() *tonBroadcast {
	return &tonBroadcast{Code: "transport: dial tcp timeout", Uncertain: true}
}

// rejectedBroadcast 复刻实测拿到的那条：接入点在投递之前就拒了这条外部消息。
func rejected() *tonBroadcast {
	return &tonBroadcast{Code: "406 lite server error, code 0: cannot apply external message to current state", Rejected: true}
}

// tonSignCounter 包住真实签名，用来数「这一单号到底签了几次」并留住 bounce 位。
type tonSignCounter struct {
	mu      sync.Mutex
	calls   int
	bounces []bool
	seqnos  []uint32
}

func (c *tonSignCounter) wrap(next tonSignFunc) tonSignFunc {
	return func(to ton.AccountID, o *pay.PayOrder, bounce bool, seqno uint32, now time.Time) (*tonPrepared, error) {
		c.mu.Lock()
		c.calls++
		c.bounces = append(c.bounces, bounce)
		c.seqnos = append(c.seqnos, seqno)
		c.mu.Unlock()
		return next(to, o, bounce, seqno, now)
	}
}

func (c *tonSignCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// newTestTONChannel 装一条只对着假网关的通道：私钥、地址、版本都是真算出来的，
// 只有「链上发生了什么」是假的——那正是本文件要控制的唯一变量。
func newTestTONChannel(t *testing.T, gw tonGateway, j ledger.Journal) (*tonChannel, *tonSignCounter) {
	t.Helper()
	key := tonTestKey()
	w, ver, err := resolveTONWallet(key, tonDeriveAddrV5R1, "", pay.NetworkMainnet, nil)
	if err != nil {
		t.Fatal(err)
	}
	led := ledger.New("ton")
	if j != nil {
		led.SetJournal(j)
	}
	c := &tonChannel{
		gw:  gw,
		opt: channelOptions{Name: "ton", Driver: DriverTonAPI},
		chain: &chainConfig{
			HTTPBase:      "https://tonapi.invalid",
			APIKey:        tonFakeAPIKey,
			WalletAddress: tonDeriveAddrV5R1,
			PrivateKey:    hex.EncodeToString(tonTestSeed()),
			Network:       pay.NetworkMainnet,
		},
		led:    led,
		acct:   w.GetAddress(),
		wallet: tonDeriveAddrV5R1,
		w:      w,
		ver:    ver,
		nowFn:  func() time.Time { return tonTestClock },
	}
	counters := &tonSignCounter{}
	c.sign = counters.wrap(c.defaultSign)
	return c, counters
}

func newTONOrder(no string) *pay.PayOrder {
	return &pay.PayOrder{
		OrderNo:  no,
		Currency: pay.CurrencyTON,
		Amount:   1_000_000_000,
		To: &pay.Party{
			Kind:    pay.PartyWallet,
			Network: pay.NetworkMainnet,
			Address: tonAcctBounceable,
			Memo:    tonFakeMemo,
		},
	}
}

// TestUncertainBroadcastNeverGetsASignature 广播结果说不清时，同单号再调用一次也不许签第二笔。
//
// 这是全仓最贵的一支：接入点超时不等于「没发出去」，把它当成失败去重下单，
// 链上那第一笔可能已经把币转出去了。
func TestUncertainBroadcastNeverGetsASignature(t *testing.T) {
	gw := &fakeTONGateway{sendErr: errors.New("dial tcp: i/o timeout")}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-1")

	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatalf("第一次提现应给出回执而不是报错: %v", err)
	}
	if n := sign.count(); n != 1 {
		t.Fatalf("第一次应当只签一笔，实得 %d", n)
	}
	res, err := c.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatalf("第二次调用不应报错: %v", err)
	}
	if n := sign.count(); n != 1 {
		t.Fatalf("广播不确定后同单号又签了一笔（%d 次）——那就是第二次出资", n)
	}
	if gw.sendCount() != 1 {
		t.Fatalf("广播 %d 次：不确定态只能查证，不能重发", gw.sendCount())
	}
	if res.Status == pay.StatusFailed {
		t.Errorf("「没查到」被下沉成了 FAILED，上游据此就会重下一单: %+v", res)
	}
	if res.TxHash == "" {
		t.Error("不确定回执必须带上外部消息 hash，否则对账没有把手")
	}
}

// TestPreparedEntryIsRebroadcastNotResigned 签好了但一条也没交出去时（进程在两次落账之间被杀掉
// 就是这一态），同单号重入必须逐字复用台账里的字节，而不是再签一笔。
func TestPreparedEntryIsRebroadcastNotResigned(t *testing.T) {
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-2")
	prepared, err := c.sign(mustTONAccount(t, o.To.Address), o, true, 5, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	// 手工摆出一条 prepared 条目：等价于「签好并落账，但还没走到广播」。
	if err := c.led.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc, To: o.To.Address,
		Amount: o.Amount, FeeLimit: c.feeReserve(), State: ledger.StatePrepared,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if n := sign.count(); n != 1 {
		t.Errorf("prepared 未过期时应当复用已签好的那一笔，却又走了 %d 次签名路径", n)
	}
	if gw.sendCount() != 1 {
		t.Fatalf("应当把台账里那一笔交出去，实得广播 %d 次", gw.sendCount())
	}
	if string(gw.sends[0]) != string(prepared.Boc) {
		t.Error("广播的字节与台账里记的那笔不是同一份：重发必须逐字复用 RawTx")
	}
	entry, _ := c.led.Get(o.OrderNo)
	if entry.TxID != prepared.MsgHash {
		t.Errorf("复用重发之后台账里的 hash 变了（%s → %s），那对账就追不到原先那一笔", prepared.MsgHash, entry.TxID)
	}
}

// TestRejectedBroadcastAllowsRebuild 接入点在投递前拒了（实测 406 那一类）时，
// 钱确定没动，同单号允许重签一笔新的——这是 rejected 不是终局。
func TestRejectedBroadcastAllowsRebuild(t *testing.T) {
	gw := &fakeTONGateway{sendResp: rejected()}
	c, sign := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-3")

	res, err := c.Withdraw(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed {
		t.Errorf("被拒的一笔应给可重发的 FAILED，实得 %s", res.Status)
	}
	entry, _ := c.led.Get(o.OrderNo)
	if entry.State != ledger.StateRejected {
		t.Errorf("被拒不该记成终局，实得 %s", entry.State)
	}
	gw.sendResp = &tonBroadcast{Code: "200", Accepted: true}
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if sign.count() != 2 {
		t.Errorf("确定没进池的单号应当可以重建，实得签名 %d 次", sign.count())
	}
	if gw.sendCount() != 2 {
		t.Errorf("广播次数应随重建到 2，实得 %d", gw.sendCount())
	}
}

// TestSettledOrderRefusesSecondSpend 已终局的单号再调用必须报 ErrAlreadySettled 且一动不发。
func TestSettledOrderRefusesSecondSpend(t *testing.T) {
	o := newTONOrder("WD-4")
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	prepared, err := c.sign(mustTONAccount(t, o.To.Address), o, true, 4, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{
		Hash: strings.Repeat("cd", 32), UnixTime: tonTestClock.Unix(),
		InMsgHash: prepared.MsgHash, InMsgKind: tonExtInKind, Success: true, VerdictKnown: true,
		TotalFeesNanoton: 5_000_000,
		Outs:             []tonOut{{To: o.To.Address, AmountNanoton: o.Amount}},
	}}
	if err := c.led.Put(&ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}); err != nil {
		t.Fatal(err)
	}

	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusSuccess || res.Fee != 5_000_000 || res.FeeCurrency != pay.CurrencyTON {
		t.Fatalf("链上成功要带手续费与币种，实得 %+v", res)
	}
	if res.TxHash != prepared.MsgHash {
		t.Errorf("成功回执带的必须是被认领的那条消息 hash，实得 %s", res.TxHash)
	}
	if res.Confirmations != nil {
		t.Errorf("TON 侧没有可直接读出的确认数，编造一个 %d 会被上游当成已足够安全", *res.Confirmations)
	}
	if _, err := c.Withdraw(context.Background(), o); !errors.Is(err, ledger.ErrAlreadySettled) {
		t.Errorf("终局后再调用应报已结算，实得 %v", err)
	}
	if sign.count() != 1 || gw.sendCount() != 0 {
		t.Errorf("终局后又动了笔：签名 %d 次、广播 %d 次", sign.count(), gw.sendCount())
	}
}

// TestUnreadableVerdictStaysUnknown 进了块但执行阶段读不出来时必须停在 UNKNOWN 且不落终局。
//
// 判成 FAILED 的下一秒就是上游重下一单，而链上那一笔可能已经把币转出去了。
func TestUnreadableVerdictStaysUnknown(t *testing.T) {
	o := newTONOrder("WD-5")
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	prepared, err := c.sign(mustTONAccount(t, tonAcctBounceable), o, true, 9, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	entry := &ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: tonTestClock.Add(time.Minute).UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}
	if err := c.led.Put(entry); err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{Hash: strings.Repeat("ee", 32), UnixTime: tonTestClock.Unix(),
		InMsgHash: prepared.MsgHash, InMsgKind: tonExtInKind, VerdictKnown: false}}

	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("读不出执行结果必须给 UNKNOWN，实得 %s", res.Status)
	}
	got, _ := c.led.Get(o.OrderNo)
	if got.Terminal() {
		t.Error("读不出结果就被打了终局，这一单再也没机会翻案")
	}
	if sign.count() != 1 {
		t.Errorf("查证路径上不该再签，实得 %d 次", sign.count())
	}
}

// TestExpiryRulingNeedsCoverage 「查不到 + 已过期」只有在读回的列表覆盖到那一刻之前时才判失败。
func TestExpiryRulingNeedsCoverage(t *testing.T) {
	for _, cs := range []struct {
		name string
		txs  []tonTx
		want string
	}{
		{"列表覆盖到过期之前", []tonTx{{Hash: "t1", UnixTime: tonTestClock.Add(-time.Hour).Unix(),
			InMsgHash: "other", InMsgKind: tonExtInKind}}, pay.StatusFailed},
		{"列表一条都没有", []tonTx{}, pay.StatusFailed},
		{"列表没覆盖到那一刻", []tonTx{{Hash: "t2", UnixTime: tonTestClock.Unix(),
			InMsgHash: "other", InMsgKind: tonExtInKind}}, pay.StatusUnknown},
	} {
		gw := &fakeTONGateway{txs: cs.txs}
		c, _ := newTestTONChannel(t, gw, nil)
		o := newTONOrder("WD-" + cs.name)
		entry := &ledger.Entry{OrderNo: o.OrderNo, TxID: strings.Repeat("11", 32),
			RawTx: []byte("boc"), To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
			ExpiresAtMS: tonTestClock.Add(-time.Minute).UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}
		if err := c.led.Put(entry); err != nil {
			t.Fatal(err)
		}
		res, err := c.QueryOrder(context.Background(), o.OrderNo)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != cs.want {
			t.Errorf("%s：期望 %s，实得 %s（%s）", cs.name, cs.want, res.Status, res.RawNote)
		}
	}
}

// TestBouncedTransferIsFailedButSettled 可退回写法打到不可达账户会整笔弹回：钱没到手，费照烧。
func TestBouncedTransferIsFailedButSettled(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-bounce")
	prepared, err := c.sign(mustTONAccount(t, tonAcctBounceable), o, true, 1, tonTestClock)
	if err != nil {
		t.Fatal(err)
	}
	gw.txs = []tonTx{{Hash: "tx-b", UnixTime: tonTestClock.Unix(), InMsgHash: prepared.MsgHash,
		InMsgKind: tonExtInKind, Success: true, VerdictKnown: true, Bounced: true, TotalFeesNanoton: 7}}
	if err := c.led.Put(&ledger.Entry{OrderNo: o.OrderNo, TxID: prepared.MsgHash, RawTx: prepared.Boc,
		To: o.To.Address, Amount: o.Amount, State: ledger.StateBroadcast,
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(), UpdatedAtMS: ledger.NowMS(tonTestClock)}); err != nil {
		t.Fatal(err)
	}
	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusFailed || !strings.Contains(res.RawNote, "退回") {
		t.Fatalf("弹回的一笔应说明钱已退回，实得 %+v", res)
	}
	got, _ := c.led.Get(o.OrderNo)
	if !got.Terminal() {
		t.Error("弹回是终局：重发同一条报文序号已用过，不落终局就会一直挂着等一个不会来的结论")
	}
}

// failingJournal 一次也写不进去：此时必须放弃动钱。
type failingJournal struct{}

func (failingJournal) Put(*ledger.Entry) error { return errors.New("库挂了") }
func (failingJournal) Get(string, string) (*ledger.Entry, bool) {
	return nil, false
}

// TestJournalWriteFailureAbortsBroadcast 台账记不上就不广播——宁可这一单不发。
func TestJournalWriteFailureAbortsBroadcast(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, failingJournal{})
	_, err := c.Withdraw(context.Background(), newTONOrder("WD-6"))
	if !errors.Is(err, ledger.ErrJournalUnavailable) {
		t.Fatalf("记账失败应报台账不可用，实得 %v", err)
	}
	if gw.sendCount() != 0 {
		t.Errorf("没记上台账却把钱交出去了（广播 %d 次）", gw.sendCount())
	}
	// 签名允许发生一次：它在本机、且那一条字节随进程一起丢掉，没有任何路径会把它再交出去
	// （要广播必须先记账）。这一口径与 tron 的链上通道逐字一致，见 tron/double_pay_test.go。
}

// firstPutFailsJournal 只让第一次记账失败：把 prepare 那道闸单独钉住。
type firstPutFailsJournal struct {
	mu   sync.Mutex
	puts int
	seen []*ledger.Entry
}

func (j *firstPutFailsJournal) Put(entry *ledger.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.puts++
	j.seen = append(j.seen, entry)
	if j.puts == 1 {
		return errors.New("第一次落账失败")
	}
	return nil
}

func (j *firstPutFailsJournal) Get(string, string) (*ledger.Entry, bool) { return nil, false }

func TestPrepareMustBeJournaledBeforeBroadcast(t *testing.T) {
	gw := &fakeTONGateway{}
	j := &firstPutFailsJournal{}
	c, _ := newTestTONChannel(t, gw, j)
	_, err := c.Withdraw(context.Background(), newTONOrder("WD-7"))
	if !errors.Is(err, ledger.ErrJournalUnavailable) {
		t.Fatalf("应报台账不可用，实得 %v", err)
	}
	if gw.sendCount() != 0 {
		t.Errorf("签名后先记账那道闸失效了：广播 %d 次", gw.sendCount())
	}
	j.mu.Lock()
	puts, state := j.puts, ledger.State("")
	if len(j.seen) > 0 {
		state = j.seen[0].State
	}
	j.mu.Unlock()
	if puts != 1 || state != ledger.StatePrepared {
		t.Errorf("第一次落账应当是 prepared 那一道，实得 puts=%d state=%s", puts, state)
	}
}

// TestPrecheckBlocksBeforeSign 余额不足、账户未部署、序号读不出时都不该动笔。
func TestPrecheckBlocksBeforeSign(t *testing.T) {
	cases := []struct {
		name  string
		gw    *fakeTONGateway
		want  string
		probe int64
	}{
		{"余额不足", &fakeTONGateway{balance: 1_000_000_000 + 1}, "insufficient", 0},
		{"钱包未部署", &fakeTONGateway{status: "uninit"}, "尚未部署", 0},
		{"钱包冻结", &fakeTONGateway{status: "frozen"}, "不可出资", 0},
		{"序号读不出", &fakeTONGateway{seqnoErr: errors.New("seqno unavailable")}, "", 0},
	}
	for _, cs := range cases {
		c, sign := newTestTONChannel(t, cs.gw, nil)
		_, err := c.Withdraw(context.Background(), newTONOrder("WD-pre-"+cs.name))
		if err == nil {
			t.Errorf("%s：前置闸没拦住", cs.name)
			continue
		}
		if cs.want != "" && !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s：报错文案该指向原因，实得 %v", cs.name, err)
		}
		if sign.count() != 0 || cs.gw.sendCount() != 0 {
			t.Errorf("%s：前置拒单却在后面动了笔（签名 %d、广播 %d）", cs.name, sign.count(), cs.gw.sendCount())
		}
	}
	// 刚好等于金额也不行：TON 上出款币与 gas 币同一种，余额要留出手续费余量。
	gw := &fakeTONGateway{balance: 1_000_000_000}
	c, _ := newTestTONChannel(t, gw, nil)
	if _, err := c.Withdraw(context.Background(), newTONOrder("WD-exact")); err == nil ||
		!strings.Contains(err.Error(), "不足以支付本单") {
		t.Errorf("余额刚好等于本单金额时应当拒，实得 %v", err)
	}
}

// TestBounceFlagComesFromOurParser EQ 与 UQ 两种形态必须落成不同的可退回位。
//
// 这一条防的是 tongo 根包那个 `bytes[0]&0x11 == 0x11` 的判定：它把 UQ 也算成可退回，
// 于是「同一交易所给的两种写法」会变成两条不同的链上报文。
func TestBounceFlagComesFromOurParser(t *testing.T) {
	gw := &fakeTONGateway{}
	c, counters := newTestTONChannel(t, gw, nil)
	for _, cs := range []struct {
		addr string
		want bool
		desc string
	}{
		{tonAcctBounceable, true, "EQ 可退回"},
		{tonAcctNonBounce, false, "UQ 不可退回"},
		{tonAcctRaw, true, "原始形态按可退回处理"},
	} {
		counters.mu.Lock()
		counters.bounces = counters.bounces[:0]
		counters.mu.Unlock()
		o := newTONOrder("WD-bounce-" + cs.desc)
		o.To.Address = cs.addr
		if _, err := c.Withdraw(context.Background(), o); err != nil {
			t.Fatalf("%s：提现失败 %v", cs.desc, err)
		}
		counters.mu.Lock()
		got := append([]bool(nil), counters.bounces...)
		counters.mu.Unlock()
		if len(got) != 1 || got[0] != cs.want {
			t.Errorf("%s：签出去的可退回位是 %v，期望 %v", cs.desc, got, cs.want)
		}
	}
}

// TestReverseLookupBlocksBroadcastAfterJournalLoss 台账丢了先反查：命中只禁止广播。
//
// F2：候选只能证明「本账户向本收端发过同额的一笔」，不能证明就是这一单；
// 因此候选不落账、不查证，结果必须是 UNKNOWN（带候选 txid）。反查不动或
// 取满一页没命中时同样不许发——「查不到」在这里不等于「没有」。
func TestReverseLookupBlocksBroadcastAfterJournalLoss(t *testing.T) {
	o := newTONOrder("WD-8")
	t0 := tonTestClock.Unix()

	t.Run("命中就不广播", func(t *testing.T) {
		gw := &fakeTONGateway{txs: []tonTx{{
			Hash: "tx-hit", UnixTime: t0 - 60, InMsgHash: strings.Repeat("77", 32), InMsgKind: tonExtInKind,
			Success: true, VerdictKnown: true,
			Outs: []tonOut{{To: tonAcctBounceable, AmountNanoton: o.Amount}},
		}}}
		c, sign := newTestTONChannel(t, gw, nil)
		res, err := c.Withdraw(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if gw.sendCount() != 0 {
			t.Errorf("链上已经有同收端同金额的一笔，还是把它交出去了（广播 %d 次）", gw.sendCount())
		}
		if sign.count() != 0 {
			t.Errorf("反查命中后又签了一笔（%d 次）", sign.count())
		}
		if res.Status != pay.StatusUnknown {
			t.Errorf("候选不能说明属于本单，必须回 UNKNOWN，实得 %+v", res)
		}
		if !strings.Contains(res.RawNote, "无法证明") {
			t.Errorf("回执没有说明候选不能归属本单: %q", res.RawNote)
		}
		// 候选绝不能落账：写成 broadcast 后，下一次 query/confirm 会把它结算成 SUCCESS。
		if _, found := c.led.Get(o.OrderNo); found {
			t.Error("反查候选被写进了台账，下一次 confirm 会把别人的转账结算成本单成功")
		}
	})

	t.Run("读不动时放弃广播", func(t *testing.T) {
		gw := &fakeTONGateway{txErr: errors.New("接入点无响应")}
		c, sign := newTestTONChannel(t, gw, nil)
		_, err := c.Withdraw(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "无法确认") {
			t.Fatalf("反查读不动时应放弃广播并说明原因，实得 %v", err)
		}
		if gw.sendCount() != 0 || sign.count() != 0 {
			t.Error("反查读不动却还是动了笔")
		}
	})

	t.Run("取满一页仍未命中按查不到处理", func(t *testing.T) {
		full := make([]tonTx, 0, tonScanLimit)
		for i := 0; i < tonScanLimit; i++ {
			full = append(full, tonTx{Hash: "tx" + string(rune('a'+i%26)), UnixTime: t0 - int64(i),
				InMsgHash: strings.Repeat("9", 32), InMsgKind: tonExtInKind,
				Outs: []tonOut{{To: tonAcctBounceable, AmountNanoton: 1}}})
		}
		gw := &fakeTONGateway{txs: full}
		c, _ := newTestTONChannel(t, gw, nil)
		if _, err := c.Withdraw(context.Background(), o); err == nil ||
			!strings.Contains(err.Error(), "取满一页") {
			t.Fatalf("窗口内取满一页未命中时应报无法确认，实得 %v", err)
		}
		if gw.sendCount() != 0 {
			t.Error("那一笔可能压在后面没读到的页里，这时广播就是双付")
		}
	})
}

// TestSecretsNeverLeakIntoResults 对外摘要里不许出现私钥与接入点凭证。
//
// RawNote 是按「可以公开给日志」设计的字段，而链上错误会原样进来。
func TestSecretsNeverLeakIntoResults(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-9")
	// 先正常走一次，让台账里有一条 broadcast 态的笔；再把读侧换成带凭证的错误原文。
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	gw.txErr = errors.New("GET https://tonapi.invalid: 401 unauthorized, header Authorization: Bearer " + tonFakeAPIKey)
	res, err := c.QueryOrder(context.Background(), o.OrderNo)
	if err != nil {
		t.Fatalf("查证失败应给 UNKNOWN 而不是报错: %v", err)
	}
	if res.Status != pay.StatusUnknown {
		t.Fatalf("读侧故障必须停在 UNKNOWN，实得 %s", res.Status)
	}
	for _, leak := range []string{tonFakeAPIKey, hex.EncodeToString(tonTestSeed())} {
		if strings.Contains(res.RawNote, leak) || strings.Contains(err2String(err), leak) {
			t.Errorf("凭证泄漏进对外字段: %s", leak)
		}
	}
	if !strings.Contains(res.RawNote, "***") {
		t.Errorf("凭证原文被抹除时应当留下 *** 标记，实得摘要：%s", res.RawNote)
	}
	if len([]rune(res.RawNote)) > 160 {
		t.Errorf("摘要没截断，长度 %d", len([]rune(res.RawNote)))
	}

	// 广播那一路同样不许泄漏：接入点拒绝时的原文里常带请求头回显。
	gw2 := &fakeTONGateway{sendResp: &tonBroadcast{
		Code:     "406 header Authorization: Bearer " + tonFakeAPIKey + " rejected",
		Rejected: true}}
	c2, _ := newTestTONChannel(t, gw2, nil)
	res2, err := c2.Withdraw(context.Background(), newTONOrder("WD-9b"))
	if err != nil {
		t.Fatalf("被拒的单应给可重发的 FAILED 而不是报错: %v", err)
	}
	if strings.Contains(res2.RawNote, tonFakeAPIKey) {
		t.Errorf("被拒摘要里带着凭证: %s", res2.RawNote)
	}
	if !strings.Contains(res2.RawNote, "***") {
		t.Errorf("被拒摘要应留下抹除标记: %s", res2.RawNote)
	}
	// 反查失败的原因要能看见（两种「查不到」是两种处置），但同样不许带凭证。
	gw3 := &fakeTONGateway{txErr: errors.New("取满一页未命中；Bearer " + tonFakeAPIKey)}
	c3, _ := newTestTONChannel(t, gw3, nil)
	if _, err := c3.Withdraw(context.Background(), newTONOrder("WD-9c")); err == nil ||
		!strings.Contains(err.Error(), "取满一页") || strings.Contains(err.Error(), tonFakeAPIKey) {
		t.Errorf("反查失败应把原因交出去且不泄漏凭证，实得 %v", err)
	}
}

func err2String(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestQueryOrderUnknownWhenNoLedger 台账没这一单号时给 UNKNOWN，不去链上瞎猜也不给 FAILED。
func TestQueryOrderUnknownWhenNoLedger(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	res, err := c.QueryOrder(context.Background(), "WD-none")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != pay.StatusUnknown || res.TxHash != "" {
		t.Fatalf("没这一单号时应给 UNKNOWN 且不给 hash，实得 %+v", res)
	}
	if _, err := c.QueryOrder(context.Background(), "  "); err == nil {
		t.Error("空单号应直接报错")
	}
}

// TestWithdrawRejectsWrongAssetAndNetwork 币种不是 TON、网络与本通道不符时一律拒。
func TestWithdrawRejectsWrongAssetAndNetwork(t *testing.T) {
	for _, cs := range []struct {
		name string
		mut  func(*pay.PayOrder)
		want string
	}{
		{"发 USDT 的单", func(o *pay.PayOrder) { o.Currency = pay.CurrencyUSDT }, "只发原生 TON"},
		{"跨到测试网", func(o *pay.PayOrder) { o.To.Network = pay.NetworkTestnet; o.To.Address = tonAcctTestnet }, "换网就是换币"},
		{"测试网地址配主网单", func(o *pay.PayOrder) { o.To.Address = tonAcctTestnet }, "测试网地址"},
		{"金额为零", func(o *pay.PayOrder) { o.Amount = 0 }, "金额"},
		{"地址为空", func(o *pay.PayOrder) { o.To.Address = "" }, "地址"},
		{"备注越界", func(o *pay.PayOrder) { o.To.Memo = strings.Repeat("x", maxMemoRunes+1) }, "备注"},
		{"备注带换行", func(o *pay.PayOrder) { o.To.Memo = "a\nb" }, "控制字符"},
	} {
		gw := &fakeTONGateway{}
		c, sign := newTestTONChannel(t, gw, nil)
		o := newTONOrder("WD-bad-" + cs.name)
		cs.mut(o)
		if _, err := c.Withdraw(context.Background(), o); err == nil || !strings.Contains(err.Error(), cs.want) {
			t.Errorf("%s：应被拒且报错含「%s」，实得 %v", cs.name, cs.want, err)
		}
		if sign.count() != 0 || gw.sendCount() != 0 {
			t.Errorf("%s：被拒的单还是动了笔（签名 %d、广播 %d）", cs.name, sign.count(), gw.sendCount())
		}
	}
}

// TestQueryAccountReadsChainBalance 供应商账户查询已删除，账户门面读默认热钱包链上余额。
func TestQueryAccountReadsChainBalance(t *testing.T) {
	const balance int64 = 100_000_000
	c, _ := newTestTONChannel(t, &fakeTONGateway{balance: balance, status: "active"}, nil)
	info, err := c.QueryAccount(context.Background(), &pay.AccountQuery{})
	if err != nil {
		t.Fatalf("链上余额查询失败: %v", err)
	}
	if info.Balance != balance {
		t.Errorf("余额=%d，期望 %d", info.Balance, balance)
	}
	if want := balance - defaultTONFeeReserveNanoton; info.Available != want {
		t.Errorf("可动用=%d，期望扣掉手续费余量后的 %d", info.Available, want)
	}
	if info.Status != pay.AcctActive {
		t.Errorf("状态=%s，期望 active", info.Status)
	}
	if info.Currency != pay.CurrencyTON || strings.TrimSpace(info.MerchantID) == "" {
		t.Errorf("账户快照缺币种/地址: %+v", info)
	}
	if c.Driver() != DriverTonAPI {
		t.Errorf("Driver 应回登记名，实得 %s", c.Driver())
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustTONAccount(t *testing.T, s string) ton.AccountID {
	t.Helper()
	id, ok := tonAccountID(s)
	if !ok {
		t.Fatalf("地址 %s 解不出账户", s)
	}
	return id
}

// TestTonWithdrawRejectsAmountUnits TON 同样只消费 int64 最小单位，字符串金额必须显式拒绝。
func TestTonWithdrawRejectsAmountUnits(t *testing.T) {
	c := &tonChannel{}
	_, err := c.Withdraw(context.Background(), &pay.PayOrder{
		OrderNo: "O-UNITS", AmountUnits: "1000000",
		To: &pay.Party{Kind: pay.PartyWallet, Address: "TAddr1"},
	})
	if err == nil || !strings.Contains(err.Error(), "amount_units") {
		t.Fatalf("TON 未拒绝 AmountUnits: %v", err)
	}
}

type mapJournal struct {
	mu sync.Mutex
	m  map[string]*ledger.Entry
}

func newMapJournal() *mapJournal { return &mapJournal{m: map[string]*ledger.Entry{}} }

func (j *mapJournal) Put(e *ledger.Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := *e
	j.m[e.Chain+"\x00"+e.OrderNo] = &cp
	return nil
}

func (j *mapJournal) Get(chain, orderNo string) (*ledger.Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.m[chain+"\x00"+orderNo]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

func TestSetJournalSwapsStorage(t *testing.T) {
	t.Cleanup(func() { SetJournal(nil) })
	j := newMapJournal()
	if old := SetJournal(j); old == nil {
		t.Fatal("SetJournal 应返回被换下的旧存储")
	}
	entry := &ledger.Entry{OrderNo: "O-J1", Amount: 1, State: ledger.StatePrepared}
	if err := tonLedger.Put(entry); err != nil {
		t.Fatal(err)
	}
	if got, ok := j.Get("ton", "O-J1"); !ok || got.Amount != 1 {
		t.Fatalf("条目没有写进注入的 journal: %+v found=%v", got, ok)
	}
	SetJournal(nil)
	if _, ok := tonLedger.Get("O-J1"); ok {
		t.Fatal("换回进程内实现后仍读得到旧 journal 的条目")
	}
}

// TestConstructorUsesPackageLedger 生产构造必须复用包级台账；每次 Start 新建一本
// 空台账会让同进程内的在途单在重启后消失（F4）。
func TestConstructorUsesPackageLedger(t *testing.T) {
	SetJournal(nil)
	t.Cleanup(func() { SetJournal(nil) })
	ch := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch.Close() })
	if ch.led != tonLedger {
		t.Fatal("通道没有使用包级 tonLedger")
	}
}

// TestJournalSurvivesChannelRestart 台账是包级实例，重建通道后仍能读到之前的条目。
func TestJournalSurvivesChannelRestart(t *testing.T) {
	SetJournal(newMapJournal())
	t.Cleanup(func() { SetJournal(nil) })

	ch1 := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch1.Close() })
	if err := ch1.led.Put(&ledger.Entry{OrderNo: "O-PERSIST", Amount: 7, State: ledger.StatePrepared}); err != nil {
		t.Fatal(err)
	}

	ch2 := mustTONChannel(t, tonInlineCfg())
	t.Cleanup(func() { _ = ch2.Close() })
	got, ok := ch2.led.Get("O-PERSIST")
	if !ok || got.Amount != 7 {
		t.Fatalf("重建通道后台账条目丢失: %+v found=%v", got, ok)
	}
}

func TestErrorAliases(t *testing.T) {
	if ErrOrderAlreadySettled != ledger.ErrAlreadySettled {
		t.Fatal("ErrOrderAlreadySettled 与 ledger.ErrAlreadySettled 不是同一个值")
	}
	if ErrJournalUnavailable != ledger.ErrJournalUnavailable {
		t.Fatal("ErrJournalUnavailable 与 ledger.ErrJournalUnavailable 不是同一个值")
	}
}

// refuse 钉住资金门面在「链路未启动」时的 fail-closed 口径：明确报错，且结果必须为 nil。
//
// 两条都判是因为上游一旦只看「有没有报错」，一个非 nil 的半截回执就会被当成受理凭证——
// 而对出款来说，「被当成已受理」的下半场就是重复打款。
func refuse[T any](t *testing.T, name string, call func() (*T, error)) {
	t.Helper()
	res, err := call()
	if err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
		t.Errorf("%s 应报「TON 通道未启动」，实得 %v", name, err)
	}
	if res != nil {
		t.Errorf("%s 未启动却给出了结果对象: %+v", name, res)
	}
}

// refuseAll 让三个门面对同一份入参走一遍同一口径。
func refuseAll(t *testing.T, ctx context.Context, order *pay.PayOrder) {
	t.Helper()
	refuse(t, "TonWithdraw", func() (*pay.PayResult, error) { return TonWithdraw(ctx, order) })
	refuse(t, "TonQuery", func() (*pay.PayResult, error) { return TonQuery(ctx, order.OrderNo) })
	refuse(t, "TonAccount", func() (*pay.AccountInfo, error) { return TonAccount(ctx, nil) })
}

// TestPayoutNoCfgRefusesEveryFacade 没有 ton 段、连配置快照都没有时，三个门面一律被拒。
func TestPayoutNoCfgRefusesEveryFacade(t *testing.T) {
	prev := tonNativeRoute.RunCtx()
	defer tonNativeRoute.SetRunCtx(prev)

	order := &pay.PayOrder{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}
	for _, cfg := range []*config.Cfg{nil, {}, {Ton: &config.TonConfig{}}} {
		refuseAll(t, corectx.WithCfg(context.Background(), cfg), order)
	}
	// ctx 为 nil 时回落到包级运行上下文；这里没有启动过通道，仍然拒绝。
	refuseAll(t, nil, order)
}

// TestPayoutInterfaceStaysWired 三个门面仍是包外可见的调用面，且未启动时一律就地拒绝。
func TestPayoutInterfaceStaysWired(t *testing.T) {
	var _ func(context.Context, *pay.PayOrder) (*pay.PayResult, error) = TonWithdraw
	var _ func(context.Context, string) (*pay.PayResult, error) = TonQuery
	var _ func(context.Context, *pay.AccountQuery) (*pay.AccountInfo, error) = TonAccount

	o := &pay.PayOrder{OrderNo: "O1", Amount: 10, To: &pay.Party{Address: tonAcctBounceable}}
	cases := []struct {
		name string
		run  func() error
	}{
		{"Withdraw", func() error { _, e := TonWithdraw(nil, o); return e }},
		{"Query", func() error { _, e := TonQuery(nil, "O1"); return e }},
		{"Account", func() error { _, e := TonAccount(nil, &pay.AccountQuery{}); return e }},
	}
	for _, cs := range cases {
		if err := cs.run(); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
			t.Errorf("%s 应报「TON 通道未启动」，实得 %v", cs.name, err)
		}
	}
	// 订单为 nil 也一样拒绝：不能因为参数不合法就先返回 nil 结果让上游以为已受理。
	if _, err := TonWithdraw(nil, nil); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
		t.Errorf("空订单的拒绝口径不符: %v", err)
	}
}

// TestTonStartWithoutConfigLeavesChannelDown 装配入口对空配置只留日志、不点亮通道。
func TestTonStartWithoutConfigLeavesChannelDown(t *testing.T) {
	prev := tonNativeRoute.RunCtx()
	defer func() {
		tonNativeRoute.SetRunCtx(prev)
		TonStop(context.Background())
	}()

	orders := []*pay.PayOrder{{OrderNo: "O1", Amount: 1, To: &pay.Party{Address: tonAcctBounceable}}}
	cases := []struct {
		name string
		cfg  *config.Cfg
	}{
		{"无 ton 段", &config.Cfg{}},
		{"ton 段存在但没开", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{}}}},
		{"jetton 被填了", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Jetton: tonAcctBounceable}}}},
		{"network 填了 TonAPI 的自述名", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Network: "testnet-v4"}}}},
		{"开了但驱动名不对", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Driver: "generic_md5"}}}},
		{"开了但没接入点", &config.Cfg{Ton: &config.TonConfig{
			Native: &config.TonNativeConfig{
				Enable: "on", Driver: DriverTonAPI}}}},
	}
	for _, cs := range cases {
		TonStart(corectx.WithCfg(context.Background(), cs.cfg))
		if _, err := TonWithdraw(nil, orders[0]); err == nil || !strings.Contains(err.Error(), "TON 通道未启动") {
			t.Errorf("%s：通道不该起来，实得 %v", cs.name, err)
		}
	}
}

// TestExampleConfigLeavesTonChannelDown 示例配置本身配不出「已启动」的 TON 通道。
func TestExampleConfigLeavesTonChannelDown(t *testing.T) {
	raw, err := os.ReadFile("../config/example.json")
	if err != nil {
		t.Fatalf("读取 example.json 失败: %v", err)
	}
	var cfg config.Cfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("example.json 反序列化失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example.json 未通过 Validate: %v", err)
	}
	if cfg.Ton == nil || cfg.Ton.Native == nil {
		t.Fatal("示例缺 ton.native 段")
	}
	if ch, ok := newTONNativeClient(&cfg); ok || ch != nil {
		t.Fatal("示例配置装配出了 TON 通道：示例不该是「复制示例即出款」的形态")
	}
}

func TestWithdrawRejectsPerOrderAsset(t *testing.T) {
	cases := []struct {
		name string
		from *pay.Party
		want string
	}{
		{
			name: "按单私钥",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, PrivateKey: pay.Secret("abcd")},
			want: "按单传入出款私钥",
		},
		{
			name: "按单合约",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, Contract: "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"},
			want: "按单指定合约",
		},
		{
			name: "按单网络",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonDeriveAddrV5R1, Network: pay.NetworkTestnet},
			want: "出款方却写着",
		},
		{
			name: "非钱包性质",
			from: &pay.Party{Kind: pay.PartyMerchant, Account: "merchant-1"},
			want: "From 性质",
		},
		{
			name: "非配置钱包",
			from: &pay.Party{Kind: pay.PartyWallet, Address: tonAcctBounceable},
			want: "本仓只持有那一个钱包的私钥",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeTONGateway{}
			c, sign := newTestTONChannel(t, gw, nil)
			o := newTONOrder("O-PER-ORDER")
			o.From = tc.from
			_, err := c.Withdraw(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应拒且报错含 %q，实得 %v", tc.want, err)
			}
			if sign.count() != 0 || gw.sendCount() != 0 {
				t.Fatalf("被拒的单还是动了笔（签名 %d、广播 %d）", sign.count(), gw.sendCount())
			}
		})
	}
}

// TestPreparePinsFrom 台账必须记下出款钱包，后续同一单号不得换钱包。
func TestPreparePinsFrom(t *testing.T) {
	c, _ := newTestTONChannel(t, &fakeTONGateway{}, nil)
	o := newTONOrder("O-FROM")
	if _, err := c.Withdraw(context.Background(), o); err != nil {
		t.Fatalf("出款失败: %v", err)
	}
	entry, found := c.led.Get("O-FROM")
	if !found || entry.From != c.wallet {
		t.Fatalf("台账没有钉住 From: %+v found=%v", entry, found)
	}

	// 旧条目/持久台账里记了另一个钱包：同一单号再来时必须拒，不能签第二笔。
	other := newTONOrder("O-FROM-OTHER")
	if err := c.led.Put(&ledger.Entry{
		OrderNo: "O-FROM-OTHER", From: tonAcctBounceable, To: other.To.Address,
		Amount: other.Amount, State: ledger.StatePrepared,
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Withdraw(context.Background(), other)
	if err == nil || !strings.Contains(err.Error(), "不得换钱包") {
		t.Fatalf("同一单号换钱包必须拒，实得 %v", err)
	}
}

// TestWithdrawTrimsOrderNo 锁键与台账键同源：首尾空白的同一单号只能签一次。
func TestWithdrawTrimsOrderNo(t *testing.T) {
	gw := &fakeTONGateway{}
	c, sign := newTestTONChannel(t, gw, nil)
	if _, err := c.Withdraw(context.Background(), newTONOrder(" WD-TRIM ")); err != nil {
		t.Fatalf("第一次出款失败: %v", err)
	}
	if _, err := c.Withdraw(context.Background(), newTONOrder("WD-TRIM")); err != nil {
		t.Fatalf("第二次出款失败: %v", err)
	}
	if sign.count() != 1 || gw.sendCount() != 1 {
		t.Fatalf("同一单号被签/广播了多次：签名 %d、广播 %d", sign.count(), gw.sendCount())
	}
	if _, ok := c.led.Get("WD-TRIM"); !ok {
		t.Fatal("台账里没有 trim 后的单号")
	}
}

// 本文件只钉 TON 侧的「接线正确性」：分派种类的取值。门面机器本身（分派登记、
// 重启关旧、订单补全、未启动拒绝）的行为基线在 chainroute/route_test.go 只写一份，
// 四链共用；TON 侧那三条重启/分派用例已上移到那里。

// TestKindTONValue 配置段名、注入钩子键与台账链维度都从 Kind 派生，值不能漂移。
func TestKindTONValue(t *testing.T) {
	if string(pay.KindTON) != "ton.native" {
		t.Fatalf("pay.KindTON=%q，期望 ton.native", pay.KindTON)
	}
}

// gatedSeqnoGateway 让第一次 seqno 读停在闸上，并模拟「受理成功即推进 seqno」。
// 这样两笔不同订单的并发出款能确定性地暴露「是否共读了同一个序号」。
type gatedSeqnoGateway struct {
	*fakeTONGateway
	firstEntered chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int32
}

func (g *gatedSeqnoGateway) seqno(_ context.Context, _ ton.AccountID) (uint32, error) {
	n := g.calls.Add(1)
	if n == 1 {
		close(g.firstEntered)
		<-g.releaseFirst
	}
	g.fakeTONGateway.mu.Lock()
	defer g.fakeTONGateway.mu.Unlock()
	return g.fakeTONGateway.seqnoVal, g.fakeTONGateway.seqnoErr
}

func (g *gatedSeqnoGateway) sendExtMessage(ctx context.Context, payload []byte) (*tonBroadcast, error) {
	res, err := g.fakeTONGateway.sendExtMessage(ctx, payload)
	if err == nil && res != nil && res.Accepted {
		g.fakeTONGateway.mu.Lock()
		g.fakeTONGateway.seqnoVal++
		g.fakeTONGateway.mu.Unlock()
	}
	return res, err
}

func (c *tonSignCounter) seqnoSnapshot() []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint32, len(c.seqnos))
	copy(out, c.seqnos)
	return out
}

// TestDifferentOrdersSerializeOnWalletSeqno F5：不同单号并发出款时，第二单必须
// 等第一单完成「seqno → 签名 → 广播」后再读序号，不能两单签同一个 seqno。
func TestDifferentOrdersSerializeOnWalletSeqno(t *testing.T) {
	gw := &gatedSeqnoGateway{
		fakeTONGateway: &fakeTONGateway{},
		firstEntered:   make(chan struct{}),
		releaseFirst:   make(chan struct{}),
	}
	c, sign := newTestTONChannel(t, gw, nil)

	aDone := make(chan struct{})
	bDone := make(chan struct{})
	go func() {
		defer close(aDone)
		_, _ = c.Withdraw(context.Background(), newTONOrder("WD-LOCK-A"))
	}()
	<-gw.firstEntered

	go func() {
		defer close(bDone)
		_, _ = c.Withdraw(context.Background(), newTONOrder("WD-LOCK-B"))
	}()

	// B 应该卡在 tonWalletMu 上，而不是去读第二个 seqno。
	select {
	case <-bDone:
		t.Fatal("B 在第一单未放行时就完成了")
	case <-time.After(150 * time.Millisecond):
	}
	if got := gw.calls.Load(); got != 1 {
		t.Fatalf("第二单没被钱包锁挡住：seqno 读了 %d 次", got)
	}

	close(gw.releaseFirst)
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Fatal("A 未完成")
	}
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("B 未完成")
	}

	seqnos := sign.seqnoSnapshot()
	if len(seqnos) != 2 || seqnos[0] != 0 || seqnos[1] != 1 {
		t.Fatalf("两单没有拿到连续且不同的 seqno: %v", seqnos)
	}
}

// TestPreparedRebroadcastTakesWalletLock prepared 分支不签名，但重发同一条报文
// 也会占用钱包序号；不拿钱包锁会让新单同时读到同一个 seqno。
func TestPreparedRebroadcastTakesWalletLock(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	o := newTONOrder("WD-PREP-LOCK")
	if err := c.led.Put(&ledger.Entry{
		OrderNo: o.OrderNo, TxID: "prepared-hash", RawTx: []byte{1, 2, 3},
		From: c.wallet, To: o.To.Address, Amount: o.Amount,
		State:       ledger.StatePrepared,
		ExpiresAtMS: tonTestClock.Add(time.Minute).UnixMilli(),
		UpdatedAtMS: tonTestClock.UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	tonWalletMu.Lock()
	locked := true
	defer func() {
		if locked {
			tonWalletMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Withdraw(context.Background(), o)
	}()
	select {
	case <-done:
		t.Fatal("prepared 重发没有等待钱包锁")
	case <-time.After(150 * time.Millisecond):
	}
	if gw.sendCount() != 0 {
		t.Fatalf("钱包锁被占着时仍然广播了 %d 次", gw.sendCount())
	}
	tonWalletMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放钱包锁后 prepared 重发没有完成")
	}
	if gw.sendCount() != 1 {
		t.Fatalf("prepared 重发次数=%d，期望 1", gw.sendCount())
	}
}

// TestQueryOrderDoesNotTakeWalletLock 查单是只读路径，不能因为钱包正在出款而被串行。
func TestQueryOrderDoesNotTakeWalletLock(t *testing.T) {
	gw := &fakeTONGateway{}
	c, _ := newTestTONChannel(t, gw, nil)
	if err := c.led.Put(&ledger.Entry{
		OrderNo: "WD-QUERY-LOCK", TxID: "query-hash", From: c.wallet,
		To: tonAcctBounceable, Amount: 1_000_000_000, State: ledger.StateBroadcast,
	}); err != nil {
		t.Fatal(err)
	}

	tonWalletMu.Lock()
	defer tonWalletMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryOrder(context.Background(), "WD-QUERY-LOCK")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("查单失败: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("查单被钱包锁阻塞")
	}
}

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
