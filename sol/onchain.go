package sol

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件是 USDT-SPL 的链上出款机器：组 transferChecked、本地签名、广播。
//
// 它与 tron/onchain.go、bsc/onchain.go 同一个骨架（先记账后广播、同一单号永不
// 签第二笔、说不清就是 UNKNOWN），差异全部来自 Solana 本身：
//
//   - **没有 nonce**：交易靠 blockhash 定身份与寿命。blockhash 的有效期约 150 个
//     区块，过期之后任何持有它的已签名交易都不可能再上链——这是 SOL 白送的
//     一条确定性判据（见 query.go 的 deadByBlockhash）。
//   - **没有 gas 价**：手续费 = 每个签名 5000 lamports + 可选的优先费
//     （compute unit price × compute unit limit）。因此这里没有「gas 价上限」，
//     只有「优先费配置值」；不配就不附 ComputeBudget 指令。
//   - **余额挂在 ATA 上**：转出方与转入方都要先按 (wallet, mint) 派生 associated
//     token account；目标 ATA 不存在时附一条幂等创建指令，否则整笔会失败。
//   - **金额精度**：USDT-SPL 是 6 位小数，PayOrder.Amount 是 int64 最小单位，
//     两者在同一量级上不会溢出；但 mint 自己的 decimals 必须在装配期与配置核对。
const (
	// defaultSOLDecimals 是 USDT-SPL 的小数位。配置留空时用它，
	// 但装配期一定会拿 mint 账户里的 decimals 复核（见 verifyMint）。
	defaultSOLDecimals = 6
	// signatureFeeLamports 是每个签名的基础手续费。Solana 按签名数收，
	// 与计算单元无关；本通道每笔只有一个签名（热钱包付钱并授权转账）。
	signatureFeeLamports = 5000
	// computeUnitLimit 是附带优先费时申请的 CU 上限。
	//
	// 一笔 ATA 创建（约 25k）+ transferChecked（约 5k）远低于这个值，
	// 取 200k 是为了给「目标账户状态在模拟与落块之间变化」留余量。
	// 优先费按「price × 这个上限」封顶，因此它同时也是本单手续费上限的乘数。
	computeUnitLimit = 200_000
	// mintDecimalsOffset 是 SPL mint 账户布局里 decimals 的字节偏移：
	// mint_authority(4+32) + supply(8) 之后就是它。
	mintDecimalsOffset = 44
	// minMintAccountLen 是能读出 decimals 的最小账户长度（82 字节的 mint 布局）。
	minMintAccountLen = 82
)

// onchainChannel 是一条直接动 Solana 的资金通道。
type onchainChannel struct {
	rpc    *solRPC
	sign   *signer
	opt    channelOptions
	chain  *chainConfig
	ledger *ledger.Ledger
	// mint 是 SPL 代币的 mint 地址（已过格式与链上账户校验）。
	mint  solana.PublicKey
	nowFn func() time.Time
}

// newSolOnchainChannel 装配一条 SPL 出款通道。
//
// 装配期把四件事钉死：mint 地址、mint 自己的 decimals、私钥派生地址、RPC 可用性。
// 这四样里任何一样要在第一笔出款时才暴露，代价都是「钱已经动了一部分」。
func newSolOnchainChannel(opt channelOptions) (*onchainChannel, error) {
	chain := opt.Chain
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]缺少 sol.usdt.chain（RPC 与热钱包地址）", opt.Name)
	}
	mintStr := strings.TrimSpace(opt.Mint)
	if mintStr == "" {
		return nil, fmt.Errorf("通道[%s]未配置代币 mint（sol.usdt.contract），链上出款拒绝", opt.Name)
	}
	if !IsValidSolanaAddress(mintStr) {
		return nil, fmt.Errorf("通道[%s]的 mint 地址 %s 不合法：应为 Base58 编码且解出恰好 32 字节", opt.Name, mintStr)
	}
	mint, err := solana.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]的 mint 地址 %s 不可解析: %w", opt.Name, mintStr, err)
	}
	if strings.TrimSpace(chain.WalletAddress) == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address 为空，出款主体必须能在配置里读出来", opt.Name)
	}
	decimals := opt.Decimals
	if decimals <= 0 {
		decimals = defaultSOLDecimals
	}
	if decimals != defaultSOLDecimals {
		// 本通道只发 USDT-SPL。把 6 写死在这里与 plan 的资损闸同源：
		// 换一张 9 位小数的代币进来，Amount 的解释就整体错 1000 倍。
		return nil, fmt.Errorf("通道[%s]的 decimals=%d 与本通道固定的小数位 %d 不一致：本通道只发 USDT-SPL",
			opt.Name, decimals, defaultSOLDecimals)
	}
	sgn, err := newSigner(chain.PrivateKey, chain.WalletAddress)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]：%w", opt.Name, err)
	}
	rp, err := newSolRPC(chain, opt.Name)
	if err != nil {
		return nil, err
	}
	c := &onchainChannel{
		rpc:    rp,
		sign:   sgn,
		opt:    opt,
		chain:  chain,
		ledger: solLedger,
		mint:   mint,
		nowFn:  time.Now,
	}
	c.opt.Mint = mintStr
	c.opt.Decimals = decimals
	if err := c.verifyMint(context.Background()); err != nil {
		rp.Close()
		return nil, err
	}
	return c, nil
}

// verifyMint 拿 mint 账户自己的 decimals 复核配置的小数位，并确认它是 SPL Token 的 mint。
//
// 这是本通道最要紧的一道装配闸：USDT 在 TRON 是 6 位、在 BSC 是 18 位、在 SOL 是 6 位，
// 把别处的位数抄进来，Amount 的 10^n 倍误差不会报错、只会把钱多发出去。
// mint 账户自己说的位数是唯一可信来源，配置只是「我以为」。
func (c *onchainChannel) verifyMint(ctx context.Context) error {
	acct, err := c.rpc.AccountInfo(ctx, c.mint)
	if err != nil {
		// 读不出来就不启动：宁可这条通道不工作，也不能带着一个未经核对的小数位出款。
		return fmt.Errorf("通道[%s]读不出 mint %s 的账户：%v", c.opt.Name, c.opt.Mint, err)
	}
	if acct == nil {
		return fmt.Errorf("通道[%s]的 mint %s 在链上不存在", c.opt.Name, c.opt.Mint)
	}
	if !acct.Owner.Equals(solana.TokenProgramID) {
		// Token-2022 的 mint 走另一套转账指令与账户扩展，本包没有实现；
		// 把它当成经典 SPL 发出去，节点会拒，而错误发生在签完名之后。
		return fmt.Errorf("通道[%s]的 mint %s 由程序 %s 拥有，不是 SPL Token 程序 %s（本通道只支持经典 SPL）",
			c.opt.Name, c.opt.Mint, acct.Owner, solana.TokenProgramID)
	}
	data := acct.Data.GetBinary()
	if len(data) < minMintAccountLen {
		return fmt.Errorf("通道[%s]的 mint %s 账户数据只有 %d 字节，读不出 decimals", c.opt.Name, c.opt.Mint, len(data))
	}
	if got := int(data[mintDecimalsOffset]); got != c.opt.Decimals {
		return fmt.Errorf("通道[%s]的 sol.usdt.decimals=%d 与 mint %s 的 decimals=%d 不一致：小数位错一位就是 10^n 倍",
			c.opt.Name, c.opt.Decimals, c.opt.Mint, got)
	}
	return nil
}

// Close 释放接入点连接（供通道停启用）。
func (c *onchainChannel) Close() error { return c.rpc.Close() }

// Withdraw 直接上链出款。
//
// 三条口径与 tron / bsc 逐字一致：
//  1. 先落台账再动钱——记不上台账就放弃广播；
//  2. 同一单号永不签第二笔——除「签好了但确定没交给节点且 blockhash 已失效」这一种情形；
//  3. 说不清就是 UNKNOWN 并交出 txid，等对账，绝不当成失败去重下单。
func (c *onchainChannel) Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error) {
	if err := pay.CheckOrder(o); err != nil {
		return nil, err
	}
	// 收款方性质先过：银行代付/商户号收款本包发不了，必须在地址解析之前就给出
	// 「走错了通道」而不是「地址不合法」。
	if err := pay.CheckTarget(o, pay.WithdrawTarget{Wallet: true}); err != nil {
		return nil, err
	}
	if err := c.checkFrom(o); err != nil {
		return nil, err
	}
	if err := checkOrderAddress(o); err != nil {
		return nil, err
	}
	if err := c.checkAsset(o); err != nil {
		return nil, err
	}
	if err := c.checkDecimals(o); err != nil {
		return nil, err
	}
	if err := c.checkAmount(o); err != nil {
		return nil, err
	}
	var res *pay.PayResult
	err := c.ledger.WithLock(strings.TrimSpace(o.OrderNo), func() error {
		var e error
		res, e = c.withdrawLocked(ctx, o)
		return e
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkFrom 钉住「这一单的出款方只能是本通道的热钱包」。
func (c *onchainChannel) checkFrom(o *pay.PayOrder) error {
	if o == nil || o.From == nil {
		return nil
	}
	from := strings.TrimSpace(o.From.Address)
	if from == "" {
		return nil
	}
	if !IsValidSolanaAddress(from) {
		return fmt.Errorf("通道[%s]的 from.address %q 不是合法的 Solana 地址", c.opt.Name, from)
	}
	pk, err := solana.PublicKeyFromBase58(from)
	if err != nil {
		return fmt.Errorf("通道[%s]的 from.address %q 不可解析: %w", c.opt.Name, from, err)
	}
	if pk.Equals(c.sign.Address()) {
		return nil
	}
	return fmt.Errorf("通道[%s]的默认出款方是热钱包 %s，这一单却指定了 %s：本仓只持有那一个钱包的私钥",
		c.opt.Name, c.sign.Address(), from)
}

// checkAsset 钉住「这一单发的确实是本通道配置的那个 mint、那张网」。
func (c *onchainChannel) checkAsset(o *pay.PayOrder) error {
	if net := strings.ToLower(strings.TrimSpace(o.To.Network)); net != "" && net != strings.ToLower(c.opt.Network) {
		return fmt.Errorf("通道[%s]配的是 %s 网络的 mint %s，这一单却写着 %s：换网就是换币，不在这里替他做决定",
			c.opt.Name, c.opt.Network, c.opt.Mint, net)
	}
	if cur := strings.ToUpper(strings.TrimSpace(o.Currency)); cur != "" && cur != pay.CurrencyUSDT {
		return fmt.Errorf("通道[%s]只发 USDT-SPL（mint %s），这一单的币种是 %s", c.opt.Name, c.opt.Mint, cur)
	}
	return nil
}

// checkDecimals 交叉校验「Amount 这个整数对应几位小数」。
//
// 填了就必须与配置逐字相等；留 0 表示不声明，按配置解释。
// 只比配置不比 mint：装配期已经拿 mint 的 decimals 与配置核对过一次，
// 这里再问一次链只会把「配置错了」与「链读不出来」混成同一个错误。
func (c *onchainChannel) checkDecimals(o *pay.PayOrder) error {
	if o.Decimals == 0 || o.Decimals == c.opt.Decimals {
		return nil
	}
	return fmt.Errorf("通道[%s]的小数位是 %d，这一单声明的 decimals=%d：位数取错的后果是 10^n 倍的不可逆多付",
		c.opt.Name, c.opt.Decimals, o.Decimals)
}

// checkAmount 钉住金额的取值域。
//
// pay.CheckOrder 已经挡过 <=0，这里再挡一次是因为本函数是「进签名机之前」的最后
// 一道：SPL 的转账金额是 u64，而 int64 的正数区间天然落在里面，真正可能出错的
// 只有「调用方绕过了 CheckOrder」这一种，而那一种必须在这里变成拒单而不是回绕。
func (c *onchainChannel) checkAmount(o *pay.PayOrder) error {
	if o == nil {
		return fmt.Errorf("通道[%s]的订单为空，没有金额可校验", c.opt.Name)
	}
	if o.Amount <= 0 {
		return fmt.Errorf("通道[%s]的金额必须为正整数最小单位（实得 %d）", c.opt.Name, o.Amount)
	}
	return nil
}

func (c *onchainChannel) withdrawLocked(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error) {
	orderNo := strings.TrimSpace(o.OrderNo)
	entry, found := c.ledger.Get(orderNo)
	now := c.nowFn()
	to, err := solana.PublicKeyFromBase58(strings.TrimSpace(o.To.Address))
	if err != nil {
		// 走到这里地址已过 checkOrderAddress，正常不会有错；留着是为了不让
		// 「解析失败零值地址」悄悄进到签名机。
		return nil, fmt.Errorf("订单号 %s 的收款地址不可解析: %w", orderNo, err)
	}

	// 同一单号二次提交时收端必须一致（按解码后的字节比，理由见 sameRecipient）。
	if found && !sameRecipient(entry.To, to) {
		return nil, fmt.Errorf("订单号 %s 已在台账里且收款地址是 %s，这一单却写着 %s：同一幂等键不得改收款方",
			orderNo, entry.To, to)
	}

	switch {
	case found && entry.Terminal():
		return nil, fmt.Errorf("%w（order_no=%s tx=%s）", ErrOrderAlreadySettled, orderNo, entry.TxID)
	case found && entry.State == ledger.StateBroadcast:
		// 已经交给过节点：唯一允许的动作是查证，任何重建都是第二笔钱。
		return c.confirm(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared:
		// 签好了却没交出去（或交出去前就失败）。Solana 的已签名交易有链上寿命，
		// 因此这里比 EVM 多一个分支：blockhash 过期即这一笔永远上不了链，
		// 可以同单号重建；仍然有效就把同一份字节交出去。
		return c.resumePrepared(ctx, entry, o, to, now)
	case found:
		// rejected 态：节点从没让这一笔进过池，钱没动过，允许同单号重建重发。
	}

	if !found {
		// 没有台账（多半是重启把进程内实现清了）：先向链上反查，命中就禁止广播。
		if hit, err := c.lookupLedgerByChain(ctx, o, now); err != nil {
			// 查不动不等于没发生：此时广播就是赌一次双付。
			return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "journal_unverified",
				Msg: "无法确认这一单是否已上过链，已放弃广播", Retryable: true}
		} else if hit != nil {
			return c.confirm(ctx, hit, now)
		}
	}

	return c.send(ctx, o, to, now)
}

// sameRecipient 判断台账里的收端与本单的收端是不是同一个地址。
//
// 按解码后的 32 字节比而不是按字符串比：Base58 对同一段字节只有一种编码，
// 但台账里的值可能带空白、也可能来自人工补录。按字节比既不会把同一地址
// 误报成「改了收款方」，也不会把两个不同地址读成同一个。
func sameRecipient(recorded string, to solana.PublicKey) bool {
	recorded = strings.TrimSpace(recorded)
	if recorded == "" {
		return true
	}
	prev, err := solana.PublicKeyFromBase58(recorded)
	if err != nil {
		return false
	}
	return prev.Equals(to)
}

// resumePrepared 处置「已签名但还没交给节点」的那一笔。
func (c *onchainChannel) resumePrepared(ctx context.Context, entry *ledger.Entry, o *pay.PayOrder, to solana.PublicKey, now time.Time) (*pay.PayResult, error) {
	tx, err := decodeRawTx(entry)
	if err != nil {
		return nil, err
	}
	valid, err := c.rpc.IsBlockhashValid(ctx, tx.Message.RecentBlockhash)
	if err != nil {
		// 状态读不出来就不能重建：万一 blockhash 还有效、而这笔其实已经被谁
		// 广播过，重签就是第二笔钱。交给上游稍后重试查证。
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "blockhash_unverified",
			Msg: "已签名交易的 blockhash 状态读不出来，暂不重发: " + c.redact(err.Error()), Retryable: true}
	}
	if valid {
		// 台账顺序是 prepared → broadcast → send，因此 prepared 态可以断言
		// 「这一笔从未交给节点」；把同一份已签名字节交出去不可能是第二笔。
		return c.broadcastEntry(ctx, entry, now)
	}
	// blockhash 已失效且这一笔从未交给节点：它永远上不了链，可以同单号重建。
	if err := c.expirePrepared(entry, now); err != nil {
		return nil, err
	}
	return c.send(ctx, o, to, now)
}

// send 完成「预检 → 组包 → 签名 → 记账 → 广播」。
//
// 每次签名都重取 blockhash：复用旧值只会在广播时收到 BlockhashNotFound，
// 而那一步的错误分不清「本地组包太慢」与「节点没收到」。
func (c *onchainChannel) send(ctx context.Context, o *pay.PayOrder, to solana.PublicKey, now time.Time) (*pay.PayResult, error) {
	src, err := c.hotWalletATA()
	if err != nil {
		return nil, err
	}
	dst, _, err := solana.FindAssociatedTokenAddress(to, c.mint)
	if err != nil {
		return nil, fmt.Errorf("派生收款方 ATA 失败: %w", err)
	}
	if err := c.precheckFunds(ctx, src, o.Amount); err != nil {
		return nil, err
	}
	instructions, err := c.buildInstructions(ctx, to, src, dst, o.Amount)
	if err != nil {
		return nil, err
	}
	if err := c.precheckFee(ctx); err != nil {
		return nil, err
	}
	hash, lastValid, err := c.rpc.LatestBlockhash(ctx)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "blockhash_unread",
			Msg: "读不出最新 blockhash: " + c.redact(err.Error()), Retryable: true}
	}
	_ = lastValid // 台账没有「有效期槽位」字段；权威判据是 IsBlockhashValid，不按估算值判死。

	builder := solana.NewTransactionBuilder().SetRecentBlockHash(hash).SetFeePayer(c.sign.Address())
	for _, inst := range instructions {
		builder = builder.AddInstruction(inst)
	}
	tx, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("组包失败: %w", err)
	}
	if err := c.sign.Sign(tx); err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "sign_failed", Msg: c.redact(err.Error()), Retryable: false}
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("已签名交易无法序列化进台账: %w", err)
	}
	entry := &ledger.Entry{
		OrderNo:     strings.TrimSpace(o.OrderNo),
		TxID:        txSignature(tx),
		RawTx:       raw,
		To:          to.String(),
		Contract:    c.opt.Mint,
		Amount:      o.Amount,
		FeeLimit:    c.feeCeiling(),
		State:       ledger.StatePrepared,
		UpdatedAtMS: ledger.NowMS(now),
		// ExpiresAtMS 留 0：Solana 的交易没有时间戳式有效期，它的「死期」由
		// blockhash 是否还在窗口内决定，那件事只能问节点（IsBlockhashValid），
		// 不能按时间算出来。
	}
	// 记账排在广播之前：反过来就会留下「链上有一笔、本地查不到」的状态，那正是双付的温床。
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	return c.broadcastEntry(ctx, entry, now)
}

// buildInstructions 组这一笔的指令表：优先费（可选）→ 目标 ATA 创建（按需）→ transferChecked。
//
// 用 transferChecked 而不是 transfer：后者不带 decimals，代币程序按 mint 自己解释
// 金额，一旦配置与 mint 不一致，链上不会报错、只会按另一个指数动钱。
func (c *onchainChannel) buildInstructions(ctx context.Context, to, src, dst solana.PublicKey, amount int64) ([]solana.Instruction, error) {
	var out []solana.Instruction
	if price := c.chain.PriorityFeeMicroLamports; price > 0 {
		out = append(out,
			computebudget.NewSetComputeUnitLimitInstruction(computeUnitLimit).Build(),
			computebudget.NewSetComputeUnitPriceInstruction(price).Build(),
		)
	}
	exists, err := c.destinationExists(ctx, dst)
	if err != nil {
		// 读不出来按「不存在」处理：CreateIdempotent 对已存在的 ATA 是 no-op，
		// 多附一条只多花一点 CU，不会造成第二笔转账；反过来漏附则会因
		// 「目标 ATA 不存在」让整笔失败——那是一次白烧手续费的失败。
		exists = false
	}
	if !exists {
		create, err := associatedtokenaccount.NewCreateIdempotentInstructionBuilder().
			SetPayer(c.sign.Address()).
			SetWallet(to).
			SetMint(c.mint).
			ValidateAndBuild()
		if err != nil {
			return nil, fmt.Errorf("组 ATA 幂等创建指令失败: %w", err)
		}
		out = append(out, create)
	}
	xfer, err := token.NewTransferCheckedInstruction(
		uint64(amount), uint8(c.opt.Decimals), src, c.mint, dst, c.sign.Address(), nil,
	).ValidateAndBuild()
	if err != nil {
		return nil, fmt.Errorf("组 transferChecked 指令失败: %w", err)
	}
	return append(out, xfer), nil
}

// destinationExists 判断收款方的 ATA 是否已经存在。
//
// 只回答「要不要附创建指令」：账户存在但不是本 mint 的 token account 时，
// transferChecked 会在链上拒掉（mint 不匹配），这里不替链上程序做判断。
func (c *onchainChannel) destinationExists(ctx context.Context, dst solana.PublicKey) (bool, error) {
	acct, err := c.rpc.AccountInfo(ctx, dst)
	if err != nil {
		if errors.Is(err, rpc.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return acct != nil, nil
}

// precheckFunds 在签名之前把「发出去必然失败」的情况拦下来。
func (c *onchainChannel) precheckFunds(ctx context.Context, src solana.PublicKey, amount int64) error {
	bal, err := c.tokenBalance(ctx, src)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if bal < uint64(amount) {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_token",
			Msg:       fmt.Sprintf("热钱包 USDT 余额 %d 不足以支付本单 %d（最小单位）", bal, amount),
			Retryable: false}
	}
	return nil
}

// precheckFee 确认热钱包的原生 SOL 够付这一笔的手续费上限。
//
// 手续费收在原生币上：USDT 够而 SOL 为 0 时，签名与广播都会成功、链上执行会失败，
// 而失败也照样烧掉手续费。这笔账必须在签名之前算。
func (c *onchainChannel) precheckFee(ctx context.Context) error {
	need := uint64(c.feeCeiling())
	native, err := c.rpc.NativeBalance(ctx, c.sign.Address())
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "读不出原生币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if native < need {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_gas",
			Msg: fmt.Sprintf("热钱包 SOL 余额 %d lamports 低于本单手续费上限 %d lamports：手续费收在原生币上，USDT 够也发不出去",
				native, need), Retryable: false}
	}
	return nil
}

// feeCeiling 是本单可能烧掉的手续费上限（lamports）。
//
// 它同时进台账的 FeeLimit 与广播前的余额预检：两处必须是同一个数，否则
// 「预检通过但实际不够」会变成一次白烧手续费的失败。
func (c *onchainChannel) feeCeiling() int64 {
	fee := uint64(signatureFeeLamports)
	if p := c.chain.PriorityFeeMicroLamports; p > 0 {
		if p > (math.MaxUint64-signatureFeeLamports)/computeUnitLimit {
			return math.MaxInt64
		}
		fee += p * computeUnitLimit
	}
	if fee > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(fee)
}

// broadcastEntry 广播台账里这一笔。RawTx 逐字不变，因此同一单号重复走到这里也只有一笔钱。
func (c *onchainChannel) broadcastEntry(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	tx, err := decodeRawTx(entry)
	if err != nil {
		return nil, err
	}
	entry.State = ledger.StateBroadcast
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	if _, err := c.rpc.SendTransaction(ctx, tx); err != nil {
		switch classifySendError(err) {
		case sendAlreadyKnown:
			// 节点说它已经见过这一笔：钱可能已经在路上，只能去查证。
			return c.confirm(ctx, entry, now)
		case sendBlockhashGone:
			// 这一笔的 blockhash 已不在窗口内：它不可能再上链。
			// 但「不可能再上链」的结论要由查证给出（万一它早就落块了）。
			return c.confirm(ctx, entry, now)
		case sendBeforePool:
			// 节点在预检/模拟阶段就拒了：这一笔不可能上链，钱没动过。
			// 它不是终局——单独记一个状态，让同单号后续可以重建重发。
			return c.markRejected(entry, c.redact(err.Error()), now)
		default:
			// 请求已经可能到达节点：状态停在 broadcast，结论交给按签名查证。
			return c.unknown(entry, "广播结果未知: "+c.redact(err.Error())), nil
		}
	}
	return c.confirm(ctx, entry, now)
}

// markRejected 记下「节点没让这一笔进池」并给出可重发的失败回执。
func (c *onchainChannel) markRejected(entry *ledger.Entry, reason string, now time.Time) (*pay.PayResult, error) {
	entry.State = ledger.StateRejected
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusFailed, Amount: entry.Amount,
		RawNote: c.note("节点拒绝入池 " + reason + "（未发生资金动作，可重发）"),
	}, nil
}

// expirePrepared 把「已签名但 blockhash 已失效」的那一笔标成 rejected。
//
// 只改状态、不出结论：调用方紧接着会用同一单号重建一笔新的，
// 此时给上游一个 FAILED 会诱使它把这张单当成已失败而另开新单——那才是双付。
func (c *onchainChannel) expirePrepared(entry *ledger.Entry, now time.Time) error {
	entry.State = ledger.StateRejected
	entry.UpdatedAtMS = ledger.NowMS(now)
	return c.ledger.Put(entry)
}

// QueryAccount 读热钱包在 mint 上的 USDT 余额。
//
// 与 tron 那侧的口径差别：那边读的是供应商账本里的额度，这边读的是链上事实。
// 链上没有「可用 / 冻结 / 授信」之分，因此 Available 与 Balance 同值，其余留 0；
// 把它们编出一个数来，只会让下游把链上余额当成供应商授信。
func (c *onchainChannel) QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	if q == nil {
		q = &pay.AccountQuery{}
	}
	if cur := strings.ToUpper(strings.TrimSpace(q.Currency)); cur != "" && cur != pay.CurrencyUSDT {
		return nil, fmt.Errorf("通道[%s]只有 USDT 一个币种，查不了 %s", c.opt.Name, cur)
	}
	ata, err := c.hotWalletATA()
	if err != nil {
		return nil, err
	}
	bal, err := c.tokenBalance(ctx, ata)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	info := &pay.AccountInfo{
		MerchantID: c.sign.Address().String(),
		Currency:   pay.CurrencyUSDT,
		Balance:    saturateUint64(bal),
		Available:  saturateUint64(bal),
		Status:     pay.AcctActive,
	}
	if bal > math.MaxInt64 {
		info.RawNote = "余额超出 int64 可表达范围，已按上限饱和（链上真实值更大）"
	}
	if native, err := c.rpc.NativeBalance(ctx, c.sign.Address()); err == nil {
		info.RawNote = strings.TrimSpace(info.RawNote + " gas 余额 " + strconv.FormatUint(native, 10) + " lamports")
	}
	return info, nil
}

// unknown 给一条「说不清」的回执：带上 txid，让对账有把手。
func (c *onchainChannel) unknown(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusUnknown, Amount: entry.Amount, RawNote: c.note(note),
	}
}

// note 是对外摘要的唯一出口：先抹凭证再截断（顺序不能反，理由见 tron 同名函数）。
func (c *onchainChannel) note(s string) string { return clip(c.redact(s)) }

// clip 把摘要截到 160 个字符：RPC 的报错可能很长，日志不该整段吞下。
func clip(s string) string {
	r := []rune(s)
	const max = 160
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// redact 把凭证从对外文案里摘掉。
//
// 三种秘密都会顺着 RPC 错误进 RawNote：私钥（不该以任何形式出现）、RPC 基址
// （服务商的密钥常常就写在路径或查询串里）、APIKey（以请求头送出的访问令牌）。
// 整串替换而不是只换密钥段——只换一段的话，剩下的 host 与路径仍然把接入点
// 暴露在日志里。
func (c *onchainChannel) redact(s string) string {
	if s == "" {
		return s
	}
	for _, secret := range []string{c.chain.PrivateKey, c.chain.APIKey, c.chain.RPC} {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}
