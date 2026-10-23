package ton

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tonkeeper/tongo/ton"
	"github.com/tonkeeper/tongo/wallet"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// tonSignFunc 把「这一单号」变成一条签好的外部消息。
//
// 它是注入点而不是内部细节：双付用例要数「这一单号到底签了几次」，
// 签名藏在通道里就只能靠广播次数间接推断，而广播次数在不确定态恰恰不可信（口径同 tron）。
type tonSignFunc func(to ton.AccountID, o *pay.PayOrder, bounce bool, seqno uint32, now time.Time) (*tonPrepared, error)

// tonChannel 是一条会直接动 TON 公链的资金通道：出款在本地签名、经接入点广播，
// 结论按外部消息 hash 从链上读回。
//
// 与 TRON 那条链上通道相比有两处真实差异，不是风格不同：
//   - 出款用的币与付 gas 用的币是同一种（原生 TON），所以前置闸只读一次余额，
//     而且要的是「本单金额 + 手续费余量」两项之和；TRON 那边代币与 TRX 是两种资产。
//   - 链上没有 refBlock 那种「同一份原始交易重发会被节点去重」的机制，防重放靠的是
//     钱包合约里的 seqno 单调递增。好处很实在：同一单号即便被重发两次同一条报文，
//     第二条也必然因为序号已用而被拒——所以「签好了没交出去」重建时逐字复用 RawTx 是安全的。
type tonChannel struct {
	gw     tonGateway
	opt    channelOptions
	chain  *chainConfig
	led    *ledger.Ledger
	acct   ton.AccountID
	wallet string
	w      *wallet.Wallet
	ver    wallet.Version
	sign   tonSignFunc // 签名注入点，装配时指向 defaultSign；测试用它数「签了几次」
	nowFn  func() time.Time
}

// tonWalletMu 串行所有「读 seqno → 签名 → 记账 → 广播」的路径。
//
// 必须是包级而不是挂在 tonChannel 上：route.Start 重复执行时旧 client 可能还有
// 在途单，新 client 同时服务新单；每客户端一把锁会让两条路径读到同一个 seqno。
// 只保护「会消耗 seqno 的动作」：查单、查余额与公链读都不拿它。
//
// 残余风险：TonAPI 索引滞后时，已受理广播后的下一次 seqno 仍可能读到旧值，
// 表现为后一单被节点拒绝（不会双付）。这里不引入高风险的高水位 seqno。
var tonWalletMu sync.Mutex

// newTonOnchainChannel 按已解析好的配置装配一条链上通道。
//
// 任何一步缺项都返回错误而不是半成品：出款链路的配置错必须在启动时看得见。
func newTonOnchainChannel(opt channelOptions) (chainChannel, error) {
	chain := opt.Chain
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]的驱动 %s 需要 ton.native.chain（接入点与热钱包）", opt.Name, opt.Driver)
	}
	walletAddress := strings.TrimSpace(chain.WalletAddress)
	if walletAddress == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address 为空，出款主体必须能在配置里读出来", opt.Name)
	}
	if strings.TrimSpace(chain.PrivateKey) == "" {
		// 空私钥不放行：半成品的链上通道只会在第一笔出款时炸（口径同 tron 的链上通道）。
		return nil, fmt.Errorf("通道[%s]未注入链上私钥（util.CallPayChainMsg+SDK 段名）", opt.Name)
	}
	// jetton 在这里不重复判：本包的 chainConfig 根本没有合约位，而「配了 jetton 就拒」
	// 已经由装配入口的 tonNetworkOf 看着配置侧那个字段把关。共用的 ChainOptions
	// 带过一个恒空的 Contract 字段，那段判空永远不会触发——那是一道看起来有、
	// 实际从不生效的死闸，比没有更坏。
	// 地址用两条独立实现各解一次，再核对它们说的是同一个账户：
	// 本包自己解 base64+CRC16（拿 bounce/testnet 位），tongo 解出链上字节。
	// 两者不一致时最省事的解释是配置里那位地址被人手抖改过一个字符。
	flags, ok := parseTONAddress(walletAddress)
	if !ok {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address %s 不合法（既不是 48 位用户友好地址或 CRC16 不过，也不是 0: 开头的原始形态）", opt.Name, walletAddress)
	}
	acct, ok := tonAccountID(walletAddress)
	if !ok {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address %s 解不出链上账户", opt.Name, walletAddress)
	}
	if int8(acct.Workchain) != flags.Workchain {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address 在两套实现里分片号不一致（本包 %d，tongo %d）", opt.Name, flags.Workchain, acct.Workchain)
	}
	if flags.HasTestnetFlag && flags.Testnet != pay.IsTestnet(chain.Network) {
		return nil, fmt.Errorf("通道[%s]的 network=%s，但 chain.wallet_address 是%s地址：换网就是换币，不在装配期替他做决定",
			opt.Name, chain.Network, tonNetLabel(flags.Testnet))
	}
	key, err := tonPrivateKey(chain.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]的链上私钥不可用: %w", opt.Name, err)
	}
	w, ver, err := resolveTONWallet(key, walletAddress, chain.WalletVersion, chain.Network, chain.SubWalletID)
	if err != nil {
		// 派生地址核对失败就是「配了 A 的地址、实际用 B 的私钥出款」，链上不留痕迹，只能在这里拒。
		return nil, fmt.Errorf("通道[%s]装配失败: %w", opt.Name, err)
	}
	gw, err := newTonAPIGateway(chain)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]的接入点不可用: %w", opt.Name, err)
	}
	c := &tonChannel{
		gw:     gw,
		opt:    opt,
		chain:  chain,
		led:    tonLedger,
		acct:   acct,
		wallet: walletAddress,
		w:      w,
		ver:    ver,
		nowFn:  time.Now,
	}
	c.sign = c.defaultSign
	return c, nil
}

// tonNetLabel 把「测试网/主网」说成人话，用在拒单文案里。
func tonNetLabel(testnet bool) string {
	if testnet {
		return "测试网"
	}
	return "主网"
}

// Driver 返回驱动标记。
//
// 它不在 tonChainChannel 接口里：驱动名只用于日志与错误定位，登记表消失之后
// 没有人再靠它找实现，留着接口方法只会让「窄接口」变成一个可以往上堆无关方法的开口。
func (c *tonChannel) Driver() string { return c.opt.Driver }

// Close 释放接入点。TON 侧走普通 HTTP，close 只清空闲连接，不打断在途请求。
func (c *tonChannel) Close() error { return c.gw.close() }

// Withdraw 直接上链出款：读序号、本地签名、广播，并把结果按「钱有没有动」而不是
// 「调用有没有返回」归一。
//
// 三条口径贯穿整个实现（与 tron 的链上通道同源）：
//  1. 先落台账再动钱——记不上台账就放弃广播（宁可这一单不发，不可发了查不到）；
//  2. 同一单号永不签第二笔——除「签好了但确定没交给节点」这一种情形；
//  3. 说不清就是 UNKNOWN 并交出消息 hash，等对账，绝不当成失败去重下单。
func (c *tonChannel) Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error) {
	// 锁键与台账键必须同源：台账 Get/Put 用 trim 后的单号，锁若还用原值，
	// "WD-1" 与 "WD-1 " 会各拿一把锁、同时看不到台账，各自签一笔。
	if o != nil {
		cp := *o
		cp.OrderNo = strings.TrimSpace(o.OrderNo)
		o = &cp
	}
	if err := pay.CheckOrder(o); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.AmountUnits) != "" {
		return nil, fmt.Errorf("TON 通道金额请用 amount（9 位最小单位），不支持 amount_units")
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
	orderNo := o.OrderNo
	var res *pay.PayResult
	err := c.led.WithLock(orderNo, func() error {
		var e error
		res, e = c.withdrawLocked(ctx, o, orderNo)
		return e
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkAsset 钉住「这一单发的确实是本通道配置的那张网、那种币」。
func (c *tonChannel) checkAsset(o *pay.PayOrder) error {
	if net := strings.TrimSpace(o.To.Network); net != "" && !strings.EqualFold(net, strings.TrimSpace(c.chain.Network)) {
		return fmt.Errorf("通道[%s]配的是 %s 网络，这一单却写着 %s：换网就是换币，不在这里替他做决定",
			c.opt.Name, c.chain.Network, net)
	}
	if cur := strings.ToUpper(strings.TrimSpace(o.Currency)); cur != "" && cur != pay.CurrencyTON {
		return fmt.Errorf("通道[%s]只发原生 TON，这一单的币种是 %s", c.opt.Name, cur)
	}
	return nil
}

// checkFrom 钉住「这一单的出款方只能是本通道的热钱包」。
//
// From 留空表示用配置的默认出款方（就是 c.wallet）；填了就必须与它指向同一个账户。
// TON 的同一个账户有多种写法（用户友好 EQ…/UQ…、原始 0:hex），逐字比会把同一账户
// 误判成两个，所以用 sameTONAddress 归一后再比；解不出账户的形态直接拒。
//
// 本包收到非空 PrivateKey / Contract 就地拒绝，不能静默忽略——上游以为换了钱包
// 或币种，实际仍按配置出款，资金归属就错了。TON 的按单私钥比 EVM/SOL 多一层：
// 同一个私钥在不同 wallet_version / sub_wallet_id 下会派生出不同地址，裸私钥
// 无法唯一确定出款账户，因此必须由配置把这三项一起钉死。
func (c *tonChannel) checkFrom(o *pay.PayOrder) error {
	if o == nil || o.From == nil {
		return nil
	}
	if k := o.From.KindOf(); k != "" && k != pay.PartyWallet {
		return fmt.Errorf("通道[%s]只能从链上钱包出款，这一单的 From 性质是 %s", c.opt.Name, k)
	}
	if strings.TrimSpace(o.From.PrivateKey.Reveal()) != "" {
		return fmt.Errorf("通道[%s]暂不支持按单传入出款私钥（From.PrivateKey）：TON 钱包地址还取决于 wallet_version / sub_wallet_id，无法只从裸私钥推导；请使用配置热钱包", c.opt.Name)
	}
	if strings.TrimSpace(o.From.Contract) != "" {
		return fmt.Errorf("通道[%s]只发原生 TON，不支持按单指定合约/jetton", c.opt.Name)
	}
	if net := strings.TrimSpace(o.From.Network); net != "" && !strings.EqualFold(net, strings.TrimSpace(c.chain.Network)) {
		return fmt.Errorf("通道[%s]配的是 %s 网络，这一单的出款方却写着 %s", c.opt.Name, c.chain.Network, net)
	}
	from := strings.TrimSpace(o.From.Address)
	if from == "" || from == c.wallet {
		return nil
	}
	if sameTONAddress(from, c.wallet) {
		return nil
	}
	return fmt.Errorf("通道[%s]的默认出款方是热钱包 %s，这一单却指定了 %s：本仓只持有那一个钱包的私钥",
		c.opt.Name, c.wallet, from)
}

func (c *tonChannel) withdrawLocked(ctx context.Context, o *pay.PayOrder, orderNo string) (*pay.PayResult, error) {
	entry, found := c.led.Get(orderNo)
	now := c.nowFn()

	// 同一单号二次提交时收端必须一致：单号是幂等键，拿同一个号发往另一个地址，
	// 要么是上游把两笔单搞混了，要么是想借幂等位改收款方——两种都不能往下走。
	if found && !sameTONAddress(entry.To, o.To.Address) {
		return nil, fmt.Errorf("订单号 %s 已在台账里且收款地址是 %s，这一单却写着 %s：同一幂等键不得改收款方",
			orderNo, entry.To, strings.TrimSpace(o.To.Address))
	}
	// TON 只有配置热钱包；台账里记过的 From 必须仍指向同一个账户（EQ/UQ/0: 归一比较）。
	// 空值表示旧条目没有这一维，不在这里补判。
	if found && strings.TrimSpace(entry.From) != "" && !sameTONAddress(entry.From, c.wallet) {
		return nil, fmt.Errorf("订单号 %s 已在台账里且出款钱包是 %s，这一单却写着 %s：同一幂等键不得换钱包",
			orderNo, entry.From, c.wallet)
	}

	switch {
	case found && entry.Terminal():
		return nil, fmt.Errorf("%w（order_no=%s msg=%s）", ErrOrderAlreadySettled, orderNo, entry.TxID)
	case found && entry.State == ledger.StateBroadcast:
		// 已经交给过接入点：唯一允许的动作是查证，任何重建都是第二笔钱。
		return c.confirm(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared && !entry.Expired(now):
		// 签好了却没交出去（或交出去前就失败）：把同一份 BOC 再广播一次。
		// 钱包合约按 seqno 单调去重，重发同一条不可能变成第二笔。
		// 仍要拿钱包锁：新单若同时读到同一个 seqno，会签出一条注定被拒的报文。
		tonWalletMu.Lock()
		defer tonWalletMu.Unlock()
		return c.broadcastEntry(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared:
		// prepared 且已过链上有效期：这一笔不可能再上链，也没有第二笔的风险，重建。
	case found:
		// rejected 态：接入点从没让这一笔进池，钱没动过，允许同单号重建重发。
	}

	if !found {
		// 没有台账（多半是重启把进程内实现清了）：先向链上反查。
		// 命中只禁止广播，不把候选当本单结论：候选 txid 交出去供对账，
		// 状态保持 UNKNOWN，绝不写台账、绝不调用 confirm。
		candidate, err := c.lookupLedgerByChain(ctx, o, now)
		if err != nil {
			// 查不动不等于没发生：此时广播就是赌一次双付。
			// 原因要跟着报错一起交出去（「取满一页没命中」和「接入点没答上来」是两种处置），
			// 但先过一遍抹除——链上错误的原文可能带着凭证。
			return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "journal_unverified",
				Msg: "无法确认这一单是否已上过链，已放弃广播: " + c.redact(err.Error()), Retryable: true}
		}
		if candidate != nil {
			return c.unknown(candidate, "链上反查命中同收端同金额候选，但无法证明属于本单；已停止广播，请对账或注入持久台账"), nil
		}
	}

	feeReserve := c.feeReserve()
	if err := c.precheckFunds(ctx, o, feeReserve); err != nil {
		return nil, err
	}
	to, ok := tonAccountID(o.To.Address)
	if !ok {
		// CheckChainRules 已经判过形态，走到这里解不出只剩一种可能：两套实现对同一串
		// 的判定不一致。宁可拒单，也不要把「本包认得」当成「链上认得」。
		return nil, fmt.Errorf("TON 收款地址 %s 本包认得但链上解不出账户", strings.TrimSpace(o.To.Address))
	}
	// 可退回位取本包的解析结果而不是 tongo 根包的 ParseAddress：后者用 bytes[0]&0x11==0x11
	// 判可退回，而不可退回形态（0x51）同样满足这个条件，会把 UQ 地址读成可退回（源码实测）。
	// 缺省方向取「可退回」：打到不可接收的账户时，可退回的写法把钱整笔弹回本账户
	// （只损失手续费），而不可退回的写法让这笔钱停在一个没人能解出私钥的账户里——
	// 两者都烧费，但只有一个还追得回来。只有对方明确给了 UQ/0Q 形态才走不可退回。
	flags, _ := parseTONAddress(strings.TrimSpace(o.To.Address))
	bounce := true
	if flags != nil && flags.HasBounceFlag && !flags.Bounceable {
		bounce = false
	}
	// 从这里到广播交出必须串行：不同单号并发时，第二个签名者若在前一笔上链前
	// 读到同一个 seqno，会签出一条永远不可能执行的报文。precheck 与反查在锁外，
	// 它们只读且耗时，没必要把整条钱包串起来。
	tonWalletMu.Lock()
	defer tonWalletMu.Unlock()
	seqno, err := c.gw.seqno(ctx, c.acct)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "seqno_unread",
			Msg: "读不出钱包序号: " + c.redact(err.Error()), Retryable: true}
	}
	prepared, err := c.sign(to, o, bounce, seqno, now)
	if err != nil {
		return nil, err
	}
	entry, err = c.prepare(o, prepared)
	if err != nil {
		return nil, err
	}
	return c.broadcastEntry(ctx, entry, now)
}

// defaultSign 组装并本地签好这一笔（不碰网络）。
func (c *tonChannel) defaultSign(to ton.AccountID, o *pay.PayOrder, bounce bool, seqno uint32, now time.Time) (*tonPrepared, error) {
	validUntil := now.Add(tonWalletLifetime)
	prepared, err := signTONTransfer(c.w, c.ver, c.acct, seqno, to, o.Amount,
		strings.TrimSpace(o.To.Memo), bounce, validUntil)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "sign_failed",
			Msg: c.redact(err.Error()), Retryable: false}
	}
	return prepared, nil
}

// prepare 把签好的一笔写进台账；记账失败即放弃广播。
func (c *tonChannel) prepare(o *pay.PayOrder, prepared *tonPrepared) (*ledger.Entry, error) {
	entry := &ledger.Entry{
		OrderNo:  strings.TrimSpace(o.OrderNo),
		TxID:     prepared.MsgHash,
		RawTx:    prepared.Boc,
		From:     c.wallet,
		To:       strings.TrimSpace(o.To.Address),
		Amount:   o.Amount,
		FeeLimit: c.feeReserve(),
		State:    ledger.StatePrepared,
		// ExpiresAtMS 是这条外部消息的链上有效期：过了它就不可能再上链，
		// 同单号才有资格重签一条新序号的。
		ExpiresAtMS: prepared.ValidUntil.UnixMilli(),
		UpdatedAtMS: ledger.NowMS(c.nowFn()),
	}
	// 记账排在广播之前：反过来就会留下「链上有一笔、本地查不到」的状态，那正是双付的温床。
	if err := c.led.Put(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// broadcastEntry 广播台账里这一笔。RawTx 逐字不变，因此同一单号重复走到这里也只有一笔钱。
func (c *tonChannel) broadcastEntry(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	entry.State = ledger.StateBroadcast
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.led.Put(entry); err != nil {
		return nil, err
	}
	res, err := c.gw.sendExtMessage(ctx, entry.RawTx)
	if err != nil {
		// 请求已经可能到达网络：状态停在 broadcast，结论交给按消息 hash 的查证。
		return c.unknown(entry, "广播结果未知: "+c.redact(err.Error())), nil
	}
	switch {
	case res.Rejected:
		// 接入点在投递之前就拒了（报文不合法、当前状态不接受这条外部消息）：这一笔不可能上链。
		// 它不算终局——单独记一个状态，让同单号后续可以重建重发（见 ton_query.go）。
		return c.markRejected(entry, res.Code, now)
	default:
		// 受理了，或说不清：两者都只能去按消息 hash 查证，绝不在此处再签一笔。
		return c.confirm(ctx, entry, now)
	}
}

// markRejected 记下「接入点没让这一笔进池」并给出可重发的失败回执。
//
// 用独立状态而不是删条目：删掉会让下一次调用看不见「这一单号已经试过了」，
// 留下状态既能允许重建，也能在日志里数出这一单号被拒过几次。
func (c *tonChannel) markRejected(entry *ledger.Entry, code string, now time.Time) (*pay.PayResult, error) {
	entry.State = ledger.StateRejected
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.led.Put(entry); err != nil {
		return nil, err
	}
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusFailed, Amount: entry.Amount,
		RawNote: c.note("接入点拒绝此笔（未发生资金动作，可重发）: " + code),
	}, nil
}

// feeReserve 取配置值，缺省按链上缺省。
//
// 单位是纳吨（TON 原生币最小单位），配置键复用 chain.fee_limit：那个键的语义一直是
// 「该链原生币最小单位计的手续费上限」，TRON 上是 sun，这里就是纳吨。
func (c *tonChannel) feeReserve() int64 {
	if c.chain.FeeLimitSun > 0 {
		return c.chain.FeeLimitSun
	}
	return defaultTONFeeReserveNanoton
}

// precheckFunds 在签名之前把「发出去必然失败」的几种情况拦下来。
//
// TON 上出款币与 gas 币同一种，所以只看一个余额：本单金额 + 手续费余量。
// 余额刚好等于金额的发法会在链上执行时失败而照样烧费——失败还倒贴，比就地拒单更难解释。
func (c *tonChannel) precheckFunds(ctx context.Context, o *pay.PayOrder, feeReserve int64) error {
	acct, err := c.gw.account(ctx, c.acct)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "读不出热钱包状态: " + c.redact(err.Error()), Retryable: true}
	}
	switch acct.Status {
	case "active":
	case "uninit", "nonexist":
		// 未部署的钱包发不出去：本通道不代为部署（部署要附 StateInit，那会改变外部消息的
		// 形态，也让「按 hash 认领」多一条要区分的分支）。先把热钱包建起来再开通道。
		return &pay.ProviderError{Channel: c.opt.Name, Code: "wallet_not_deployed",
			Msg: fmt.Sprintf("热钱包 %s 在链上尚未部署（status=%s），本通道只从已部署账户出款", c.wallet, acct.Status), Retryable: false}
	default:
		return &pay.ProviderError{Channel: c.opt.Name, Code: "wallet_not_spendable",
			Msg: fmt.Sprintf("热钱包 %s 的链上状态是 %s，不可出资", c.wallet, acct.Status), Retryable: false}
	}
	need := o.Amount + feeReserve
	if need < o.Amount { // 加法溢出：配了一个荒谬的金额时不能让它绕回余额判定
		return &pay.ProviderError{Channel: c.opt.Name, Code: "amount_overflow",
			Msg: fmt.Sprintf("本单金额 %d 纳吨已超出可判定范围", o.Amount), Retryable: false}
	}
	if acct.BalanceNanoton < need {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_balance",
			Msg:       fmt.Sprintf("热钱包余额 %d 纳吨不足以支付本单 %d 加手续费余量 %d", acct.BalanceNanoton, o.Amount, feeReserve),
			Retryable: false}
	}
	return nil
}

// unknown 给一条「说不清」的回执：带上消息 hash，让对账有把手。
func (c *tonChannel) unknown(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusUnknown, Amount: entry.Amount, RawNote: c.note(note),
	}
}

// note 是对外摘要的唯一出口：先抹凭证再截断。
//
// 顺序不能反——截断可能正好把一个凭证切在中间留下片段，而抹除必须看得到整串。
func (c *tonChannel) note(s string) string { return clipTON(c.redact(s)) }

// clipTON 截断过长的链上摘要（日志不该整段吞下一条 BOC 报错）。
func clipTON(s string) string {
	r := []rune(s)
	const max = 160
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// redact 把凭证从对外文案里摘掉。
//
// 链上错误会原样进 PayResult.RawNote，而 RawNote 是按「可以公开给日志」设计的字段：
// 接入点的一句报错里带出 API Key（例如把请求头回显在 401 文本里），这条纪律就当众破了。
// 私钥与 BOC 同理——它们不该以任何形式出现在返回值里。
func (c *tonChannel) redact(s string) string {
	for _, secret := range []string{c.chain.APIKey, c.chain.PrivateKey} {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// 编译期钉子：本通道必须满足 pay 的统一调用面与门面窄接口，
// 缺方法要在构建期就看见而不是装配期。
var _ pay.Channel = (*tonChannel)(nil)
var _ chainChannel = (*tonChannel)(nil)
