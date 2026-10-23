package ton

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tonkeeper/tongo/boc"
	"github.com/tonkeeper/tongo/tlb"
	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/wallet"

	"touchgocore/pay"
)

// 本文件只管两件事：把配置里的 hex 私钥变成能签名的钱包，以及把一笔转账签成
// 可以广播出去的外部消息。不碰网络、不碰台账——那两侧的错都不该在这里发生。

// tonWalletLifetime 是签好的外部消息在链上的存活窗口。
//
// 它是幂等的一部分：过了这个时刻，这一笔不可能再上链，同单号才有资格重建。
// 取 2 分钟而不是 tongo 的 3 分钟缺省——窗口越长，卡住的单号要等越久才能重试。
const tonWalletLifetime = 2 * time.Minute

// defaultTONFeeReserveNanoton 是本单之外必须还留在热钱包里的余量（0.01 TON）。
//
// TON 上出款用的币与付 gas 用的币是同一种，所以「余额刚好等于本单金额」的发法
// 会在链上执行时失败而照样烧掉费。留一个余量比算准便宜。可由 chain.fee_limit 覆盖。
const defaultTONFeeReserveNanoton int64 = 10_000_000

// tonScanLimit 是一次取多少条近期交易。
//
// 它同时是「反查能不能覆盖到那一笔」的分母：取满还没命中就按查不到处理（见 ton_query.go），
// 因为热钱包被多人共用时，后面的页里可能正压着我们那一笔。
const tonScanLimit = 30

// tonScanWindow 是「台账查不到这一单号」时向链上反查的时间窗。
//
// 取 72 小时而不是消息有效期（两分钟）：出款调度可能在故障后很久才重试，
// 那一刻要防的是「上一次其实已经上链了」。窗口取窄等于把双付风险留给自己。
const tonScanWindow = 72 * time.Hour

// tonDetectVersions 是自动探测钱包形态时尝试的候选。
//
// 只列 tongo 的 wallet.VerifySignature 能覆盖的版本（V3R1/V3R2/V4R1/V4R2/V5R1）：
// 探出来之后还要能自检签好的报文，探测面比自检面宽就等于「签完发出去、但没人能证明
// 签名对」。V5Beta、walletTg 与 highload 系列要用的话，得先给它们补上同形态的自检。
var tonDetectVersions = []wallet.Version{wallet.V5R1, wallet.V4R2, wallet.V3R2, wallet.V4R1, wallet.V3R1}

// tonPrivateKey 解析配置里的 hex 私钥。
//
// tongo 不自带 hex 助手（它的 SeedToPrivateKey 只吃 12 词以上助记词，
// 见 wallet/seed.go），所以两种常见导出形态在这里各自认一次：
// 64 字节是 Tonkeeper/tonweb 那一种（前 32 字节种子 + 后 32 字节公钥），
// 32 字节是纯种子。后半段必须与前半段推出来的公钥逐字相等——不相等就是拼接时串了位，
// 那种私钥签出来的报文在链上只会以「验签不过」收场，而报错要等到第一笔出款。
func tonPrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("链上私钥不是十六进制串")
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		// 比对要用「由种子重算出的公钥」而不是 PrivateKey.Public()：后者直接回显末 32 字节，
		// 拿它比自己恒等，等于没有检查。重算才真的回答「这两半是不是同一把钥匙」。
		derived := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]).Public().(ed25519.PublicKey)
		if !derived.Equal(ed25519.PublicKey(b[ed25519.SeedSize:])) {
			return nil, errors.New("链上私钥的后半段公钥与前半段种子推出来的不一致")
		}
		return ed25519.PrivateKey(b), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	default:
		return nil, fmt.Errorf("链上私钥长度 %d 字节，既不是 32 字节种子也不是 64 字节完整密钥", len(b))
	}
}

// tonAccountID 把任一合法形态的地址解析成链上账户标识（用户友好形态会剥掉 bounce/testnet 位）。
func tonAccountID(s string) (ton.AccountID, bool) {
	id, err := ton.ParseAccountID(strings.TrimSpace(s))
	if err != nil {
		return ton.AccountID{}, false
	}
	return id, true
}

// sameTONAddress 判两个地址字符串是不是同一个链上账户。
//
// 必须解析后再比而不是比字符串：同一个账户有 EQ（可退回）、UQ（不可退回）、
// 0:十六进制三种写法，交易所给哪一种取决于它自己的习惯。直接比字符串会把
// 「完全相同的账户」读成两个账户，从而在反查时永远查不到自己发的那一笔。
func sameTONAddress(a, b string) bool {
	ia, ok1 := tonAccountID(a)
	ib, ok2 := tonAccountID(b)
	return ok1 && ok2 && ia == ib
}

// tonGlobalID 按网络标识给出链的全局号：它是钱包 ID 的一部分，填错就是换了一条链。
func tonGlobalID(network string) int32 {
	if pay.IsTestnet(network) {
		return wallet.TestnetGlobalID
	}
	return wallet.MainnetGlobalID
}

// resolveTONWallet 找出与配置地址一致的钱包形态，并交出可直接签名的钱包与其版本名。
//
// 这一步同时是派生地址核对：拿私钥推出公钥，逐个候选算出地址，只有一字不差地等于
// chain.wallet_address 的那个才认。对不上就是「配了 A 的地址、实际用 B 的私钥出款」，
// 这种错在链上不留任何可查痕迹，只能在装配期拒掉（口径同 tron/onchain.go）。
//
// wantVersion 留空即自动探测；给了名字就只试那一个——探测不出时不猜第二个，
// 因为「试出来的那个恰好能签但链上不是它」是不可逆的。
func resolveTONWallet(key ed25519.PrivateKey, walletAddress, wantVersion, network string, sub *uint32) (*wallet.Wallet, wallet.Version, error) {
	target, ok := tonAccountID(walletAddress)
	if !ok {
		return nil, 0, fmt.Errorf("chain.wallet_address %s 解析不出链上账户（要 48 位用户友好地址或 0: 原始形态）", walletAddress)
	}
	if target.Workchain != 0 {
		return nil, 0, fmt.Errorf("热钱包在分片 %d 上，本通道只支持 basechain(0) 的钱包账户", target.Workchain)
	}
	pub := key.Public().(ed25519.PublicKey)
	globalID := tonGlobalID(network)

	candidates := tonDetectVersions
	if wantVersion != "" {
		ver, err := wallet.VersionFromString(wantVersion)
		if err != nil {
			return nil, 0, fmt.Errorf("chain 侧的钱包版本 %s 不是 tongo 认识的名字（可用：v5R1 v4R2 v4R1 v3R2 v3R1）", wantVersion)
		}
		ok := false
		for _, c := range tonDetectVersions {
			if c == ver {
				ok = true
			}
		}
		if !ok {
			return nil, 0, fmt.Errorf("钱包版本 %s 不在本通道支持范围：它的签名报文没有可自检的路径，不接（见 tonDetectVersions 的注释）", wantVersion)
		}
		candidates = []wallet.Version{ver}
	}

	for _, ver := range candidates {
		derived, err := wallet.GenerateWalletAddress(pub, ver, &globalID, 0, sub)
		if err != nil {
			continue
		}
		if derived != target {
			continue
		}
		w, err := wallet.New(key, ver, nil, wallet.WithNetworkGlobalID(globalID), wallet.WithWorkchain(0), walletSubWalletOption(sub))
		if err != nil {
			return nil, 0, fmt.Errorf("按 %s 组装钱包失败: %w", ver.ToString(), err)
		}
		if w.GetAddress() != target {
			// 走到这里说明 tongo 自己两条路径（GenerateWalletAddress 与 New 内部的 generateAddress）
			// 对同一个公钥给了不同答案——那是依赖内部不一致，绝不能带着这种钱包动钱。
			return nil, 0, fmt.Errorf("%s 钱包的派生地址与配置地址不一致（tongo 内部两算法给出不同结果）", ver.ToString())
		}
		return &w, ver, nil
	}
	if sub != nil {
		return nil, 0, fmt.Errorf("私钥派生不出 chain.wallet_address %s（试过 %s，sub_wallet_id=%d）——私钥与地址不是同一个账户，或 sub_wallet_id 配错",
			walletAddress, tonVersionNames(candidates), *sub)
	}
	return nil, 0, fmt.Errorf("私钥派生不出 chain.wallet_address %s（试过 %s，默认 sub_wallet_id）——私钥与地址不是同一个账户，或该钱包用了非默认 sub_wallet_id，需在配置里点名",
		walletAddress, tonVersionNames(candidates))
}

// walletSubWalletOption 把「没配 sub_wallet_id」与「配了 0」分开：传 nil 就是让 tongo 用它
// 该版本的默认值，传指针才是显式覆盖。把 0 当默认送进 V3/V4 会得到另一个地址。
func walletSubWalletOption(sub *uint32) wallet.Option {
	if sub == nil {
		// 空操作选项：让调用链保持单一形状，不在外面再分叉一次。
		return func(*wallet.Options) {}
	}
	return wallet.WithSubWalletID(*sub)
}

func tonVersionNames(vs []wallet.Version) string {
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, v.ToString())
	}
	return strings.Join(names, "/")
}

// tonPrepared 是「签好了、还没交出去」的那一笔。
type tonPrepared struct {
	// Boc 是可广播的外部消息字节（进台账，同单号重发时逐字复用）。
	Boc []byte
	// MsgHash 是这条外部消息的 hash（十六进制，小写）——查证与反查都靠它认领。
	MsgHash string
	Seqno   uint32
	// ValidUntil 是这条消息的链上有效期。台账的过期判定与广播前的重试窗口都读它。
	ValidUntil time.Time
}

// signTONTransfer 本地组包并签名：私钥不出本机，签名不发生在任何网络请求里。
//
// 三个容易被漏掉的点都在这里做实（都是探针实测出来的，不是推测）：
//  1. V5 的报文必须显式带 opcode（wallet.V5MsgTypeSignedExternal）。CreateMessageBody 不会替
//     调用方补：漏了它，签名、序列化、验签一路全绿，只有链上会拒——因为操作码是 0。
//     （walletTg 的 TgMsgType 零值恰好就是 external，所以不另设。）
//  2. ValidUntil 必须由调用方给死值而不是留给 tongo 现算：台账要记的就是签名那一刻
//     定下的那个时刻，事后「猜它大概是一分钟后」会让过期判定不可复现。
//  3. 本消息的原始 hash 与归一化 hash 必须相等：本通道不带 StateInit（热钱包必须已部署，
//     见 precheckFunds），此时归一化不改形状，两者相等（实测逐字节相同）。不相等说明
//     我们交出去的是另一种形态的消息，而按 hash 认领会永远认领不到。
func signTONTransfer(w *wallet.Wallet, ver wallet.Version, self ton.AccountID, seqno uint32, to ton.AccountID, amount int64, memo string, bounce bool, validUntil time.Time) (*tonPrepared, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("TON 出款金额必须为正，实得 %d 纳吨", amount)
	}
	if amount > int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("TON 出款金额 %d 超出链上可用的grams宽度", amount)
	}
	body, err := w.CreateMessageBody(
		wallet.MessageConfig{Seqno: seqno, ValidUntil: validUntil, V5MsgType: wallet.V5MsgTypeSignedExternal},
		wallet.SimpleTransfer{
			Amount:  tlb.Grams(uint64(amount)),
			Address: to,
			// 备注交给链上的 comment 字段：TonAPI 与钱包合约都按 text comment 读它。
			Comment:    memo,
			Bounceable: bounce,
		})
	if err != nil {
		return nil, fmt.Errorf("TON 组包失败: %w", err)
	}
	ext, err := ton.CreateExternalMessage(self, body, nil, tlb.VarUInteger16{})
	if err != nil {
		return nil, fmt.Errorf("TON 外部消息组装失败: %w", err)
	}
	cell := boc.NewCell()
	if err := tlb.Marshal(cell, ext); err != nil {
		return nil, fmt.Errorf("TON 外部消息序列化失败: %w", err)
	}
	// 自检用**另一份** cell：wallet.VerifySignature 内部要 tlb.Unmarshal 这条消息，
	// 那会消耗 cell 的读游标；拿同一份去算 hash 与序列化 BOC 就会读到被啃过的形状。
	proofCell := boc.NewCell()
	if err := tlb.Marshal(proofCell, ext); err != nil {
		return nil, fmt.Errorf("TON 外部消息自检序列化失败: %w", err)
	}
	if err := verifyTONBody(ver, body, proofCell, w.GetPublicKey()); err != nil {
		return nil, err
	}
	raw, err := cell.Hash256()
	if err != nil {
		return nil, fmt.Errorf("TON 外部消息算不出 hash: %w", err)
	}
	if norm := ext.Hash(true); [32]byte(norm) != raw {
		return nil, fmt.Errorf("TON 外部消息的原始 hash 与归一化 hash 不一致（本通道只在不带 StateInit 时出款，出现不一致说明报文形态变了）")
	}
	payload, err := cell.ToBocCustom(false, false, false, 0)
	if err != nil {
		return nil, fmt.Errorf("TON 外部消息转 BOC 失败: %w", err)
	}
	if len(payload) == 0 {
		return nil, errors.New("TON 外部消息转出的 BOC 为空")
	}
	return &tonPrepared{
		Boc:        payload,
		MsgHash:    strings.ToLower(hex.EncodeToString(raw[:])),
		Seqno:      seqno,
		ValidUntil: validUntil,
	}, nil
}

// verifyTONBody 在广播之前自证报文成形，两道检查各管一种错（都是探针实测出来的形态）：
//
//  1. 操作码。V5 的报文体前 32 bit 就是操作码，实测正常值 0x7369676e（"sign"）。
//     漏填 wallet.V5MsgTypeSignedExternal 时签名、序列化、验签一路全绿——签名覆盖的正是
//     这个错的操作码，只有链上会以 exit code 收场。所以这一条必须单独钉。
//     V3/V4 的操作码恒为 0 且报文体以签名开头，没有这个可漏填的字段，故不查。
//  2. 签名。对已组装好的外部消息 cell 验：tongo 的 VerifySignature 对 V3/V4 也要先解出
//     一条 tlb.Message 再取报文体（实测直接喂报文体给它的 V3R2 分支会报 "not enough refs"），
//     所以统一喂外部消息的 cell 而不是报文体。换错公钥会真的报错，这道闸是活的。
func verifyTONBody(ver wallet.Version, body, extCell *boc.Cell, pub ed25519.PublicKey) error {
	if ver == wallet.V5R1 {
		raw, err := body.ToBoc()
		if err != nil {
			return fmt.Errorf("自检时报文文体序列化失败: %w", err)
		}
		head, err := boc.DeserializeSingleRootBoc(raw)
		if err != nil {
			return fmt.Errorf("自检时报文文体反序列化失败: %w", err)
		}
		op, err := head.ReadUint(32)
		if err != nil {
			return fmt.Errorf("报文文体读不出操作码: %w", err)
		}
		if uint32(op) != uint32(wallet.V5MsgTypeSignedExternal) {
			return fmt.Errorf("V5 报文的操作码是 %x，不是 signed-external %x：签名会照盖，链上照拒",
				uint32(op), uint32(wallet.V5MsgTypeSignedExternal))
		}
	}
	if err := wallet.VerifySignature(ver, extCell, pub); err != nil {
		return fmt.Errorf("已签名报文自检不过（%s）: %w", ver.ToString(), err)
	}
	return nil
}
