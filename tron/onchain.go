package tron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/client/transaction"
	sdkkeys "github.com/fbsobreira/gotron-sdk/pkg/keys"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"touchgocore/ledger"
	"touchgocore/pay"
	"touchgocore/util"
	"touchgocore/vars"
)

// defaultTRC20FeeLimitSun 是 TRC20 转账的缺省手续费上限：65 TRX（sun）。
//
// 收款地址未激活时，一笔 USDT 转账的能量需求会接近这个量级。留空不拦而按更小的数发出去，
// 表现是 ret=FAILED 而 TRX 已经烧掉——失败还倒贴，比就地拒单更难解释。
const defaultTRC20FeeLimitSun int64 = 65_000_000

// scanWindow 是「台账查不到这一单号」时向链上反查的时间窗。
//
// 取 72 小时而不是交易有效期（约 1 分钟）：出款调度可能在故障后很久才重试，
// 那一刻要防的是「上一次其实已经上链了」。窗口取窄等于把双付风险留给自己。
const scanWindow = 72 * time.Hour

// signFunc 把未签名交易签成可广播的交易。
//
// 它是注入点而不是内部细节：双付用例要数「这一单号到底签了几次」，
// 签名藏在网关里就只能靠广播次数间接推断，而广播次数在不确定态恰恰不可信。
type signFunc func(tx *core.Transaction) (*core.Transaction, error)

// orderAsset 是「这一单到底用哪个钱包、哪张合约、哪张网、哪把钥匙」的解析结果。
//
// 配置里的 wallet/contract/private_key 只是缺省值：whatapp 按单从 withdraw_type
// 带完整出款参数（地址+私钥+合约+网络），所以同一进程要能服务多个热钱包。
// 解析与校验必须在签名之前完成——地址与私钥不匹配时，组包已经算过一遍 gas。
type orderAsset struct {
	wallet   string
	contract string
	network  string
	sign     signFunc
	// secret 只用于错误文案脱敏，绝不进日志。它来自按单私钥；配置私钥由
	// c.redact 另行处理。
	secret string
}

// onchainChannel 是一条会直接动公链的资金通道。
//
// 三件事按「钱从哪儿动」分工，而不是按驱动一刀切：
//   - Withdraw 与 QueryOrder 走链（本地签名广播、按 txid 读回事实）；
//   - QueryAccount 仍走被引用段的 HTTP 供应商实现（配了供应商才可用）；
//   - QueryOnchainAccount 读热钱包在链上的余额，供水位门禁这类「只关心还剩多少币」
//     的场景使用，与供应商侧额度是两套口径。
//
// 出款资产按单解析：配置里的 wallet/contract/private_key 是缺省值，PayOrder.From
// 可以携带完整参数（地址+私钥+合约+网络），以支持 whatapp 的多线路热钱包。
type onchainChannel struct {
	gw       chainGateway
	sign     signFunc // 配置私钥；可为 nil（按单提供）
	opt      channelOptions
	chain    *chainConfig
	ledger   *ledger.Ledger
	wallet   string         // 配置热钱包；可为空（按单提供）
	supplier accountQuerier // 可为 nil：只上链不接供应商时，账户查询就地拒绝
	nowFn    func() time.Time
}

func newOnchainChannel(opt channelOptions) (*onchainChannel, error) {
	chain := opt.Chain
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]的驱动 %s 需要 tron.usdt.chain（节点与热钱包地址）", opt.Name, opt.Driver)
	}
	if strings.TrimSpace(chain.Node) == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.node 为空，出款不知道该广播给哪个节点", opt.Name)
	}
	// 热钱包地址与私钥是缺省值而不是装配前提：whatapp 按单从 withdraw_type 携带
	// 完整出款参数，配置里不填也必须能起来。填了就必须自洽——派生地址与地址
	// 逐字相等，否则就是「按 A 的地址记账、用 B 的私钥出款」。
	wallet := strings.TrimSpace(chain.WalletAddress)
	keyHex := strings.TrimSpace(chain.PrivateKey)
	var signer signFunc
	if keyHex != "" {
		key, err := sdkkeys.GetPrivateKeyFromHex(keyHex)
		if err != nil {
			return nil, fmt.Errorf("通道[%s]的链上私钥不可用", opt.Name)
		}
		derived := address.BTCECPrivkeyToAddress(key).String()
		if wallet == "" {
			wallet = derived
		}
		if derived != wallet {
			return nil, fmt.Errorf("通道[%s]的私钥派生地址与 chain.wallet_address 不是同一个账户", opt.Name)
		}
		signer = softSigner(key)
	}
	gw, err := buildGateway(chain, opt)
	if err != nil {
		return nil, err
	}
	c := &onchainChannel{
		gw:     gw,
		sign:   signer,
		opt:    opt,
		chain:  chain,
		ledger: tronLedger,
		wallet: wallet,
		nowFn:  time.Now,
	}
	// 供应商侧可选：没配 base_url 与端点表时不硬要——纯自托管部署确实没有供应商账户查询链路。
	if strings.TrimSpace(opt.BaseURL) != "" && opt.Endpoint != nil {
		// 交出去的那一份必须摘掉 Chain：账户客户端不消费链上接入点，两者各取所需。
		supOpt := opt
		supOpt.Chain = nil
		sup, err := newAccountClient(supOpt)
		if err != nil {
			return nil, fmt.Errorf("通道[%s]装配供应商侧失败（账户查询仍需要它）: %w", opt.Name, err)
		}
		c.supplier = sup
	}
	return c, nil
}

// softSigner 用本地私钥离线签名：签名只在本机发生，私钥不出门。
func softSigner(key *btcec.PrivateKey) signFunc {
	return func(tx *core.Transaction) (*core.Transaction, error) {
		return transaction.SignTransaction(tx, key)
	}
}

// buildGateway 建接入点：gRPC 是正门，REST 只做读侧兜底。
func buildGateway(chain *chainConfig, opt channelOptions) (chainGateway, error) {
	grpcGW, err := newGRPCGateway(chain.Node, chain.APIKey, chain.Timeout, chain.PlaintextNode)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(chain.HTTPBase) == "" {
		// 没配 REST 时提醒一句：读侧兜底与重启后的台账反查都只有接入点的扩展读接口能做，
		// 缺它不是静默少一层保护，而是日志里少一行、双付风险多一分。
		vars.Info("链上通道[%s]未配置 chain.http_base：读侧兜底与出款台账反查不可用", opt.Name)
		return grpcGW, nil
	}
	httpGW, err := newHTTPGateway(chain.HTTPBase, chain.APIKey, chain.Timeout, chain.MaxRetry)
	if err != nil {
		_ = grpcGW.close()
		return nil, err
	}
	return &tieredGateway{primary: grpcGW, reader: httpGW}, nil
}

// tieredGateway 把写操作钉在 gRPC 上，读操作先走 gRPC、失败再走 REST。
type tieredGateway struct {
	primary chainGateway
	reader  chainGateway
}

func (t *tieredGateway) buildTransfer(ctx context.Context, from, to, contract string, amount, feeLimit int64) (*core.Transaction, error) {
	return t.primary.buildTransfer(ctx, from, to, contract, amount, feeLimit)
}

func (t *tieredGateway) broadcast(ctx context.Context, tx *core.Transaction) (*broadcastResult, error) {
	return t.primary.broadcast(ctx, tx)
}

func (t *tieredGateway) lookupTx(ctx context.Context, txID string) (*txReceipt, error) {
	res, err := t.primary.lookupTx(ctx, txID)
	if err == nil {
		return res, nil
	}
	if !isReadFallbackWorthwhile(err) {
		return nil, err
	}
	return t.reader.lookupTx(ctx, txID)
}

func (t *tieredGateway) tokenBalance(ctx context.Context, addr, contract string) (int64, error) {
	v, err := t.primary.tokenBalance(ctx, addr, contract)
	if err == nil {
		return v, nil
	}
	if !isReadFallbackWorthwhile(err) {
		return 0, err
	}
	return t.reader.tokenBalance(ctx, addr, contract)
}

func (t *tieredGateway) account(ctx context.Context, addr string) (*accountState, error) {
	v, err := t.primary.account(ctx, addr)
	if err == nil {
		return v, nil
	}
	if !isReadFallbackWorthwhile(err) {
		return nil, err
	}
	return t.reader.account(ctx, addr)
}

func (t *tieredGateway) headBlock(ctx context.Context) (int64, error) {
	v, err := t.primary.headBlock(ctx)
	if err == nil {
		return v, nil
	}
	if !isReadFallbackWorthwhile(err) {
		return 0, err
	}
	return t.reader.headBlock(ctx)
}

// outboundTransfers 只有读侧接口能做：全节点 gRPC 没有转账历史列表。
func (t *tieredGateway) outboundTransfers(ctx context.Context, addr, contract string, sinceMS int64) ([]outboundTransfer, error) {
	return t.reader.outboundTransfers(ctx, addr, contract, sinceMS)
}

func (t *tieredGateway) close() error {
	err := t.primary.close()
	if err2 := t.reader.close(); err == nil {
		err = err2
	}
	return err
}

// isReadFallbackWorthwhile 判断读侧错误是否值得换一条读路。
//
// 只认「接入点没答上来」这一类（传输、超时、未连接）：业务性的读失败（回执解析不了、
// 余额读不出）换条路照样读不出名堂，回落只是把一次故障变成两次并拖长出款。
func isReadFallbackWorthwhile(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{"deadline", "unavailable", "connection", "transport", "timeout", "eof", "not connected"} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// Driver 返回驱动标记。
//
// 它不在 chainChannel 接口里：驱动名只用于日志与错误定位，登记表消失之后
// 没有人再靠它找实现，留着接口方法只会让「窄接口」变成一个可以往上堆无关方法的开口。
func (c *onchainChannel) Driver() string { return c.opt.Driver }

// MerchantID 返回配置里的商户号；链上通道没有商户概念，它只是这段配置的定位信息。
func (c *onchainChannel) MerchantID() string { return c.opt.MerchantID }

// Close 释放接入点连接（供通道停启用）。
func (c *onchainChannel) Close() error { return c.gw.close() }

// QueryAccount 商户账户的余额与额度是供应商侧的事实。
//
// 链上确实能读到「这个热钱包还有多少 USDT」，但那是另一个口径（没有可用/冻结之分），
// 顺手拿它冒充 AccountInfo 会让下游把 gas 账户当成出款额度。链上余额请走
// QueryOnchainAccount。
func (c *onchainChannel) QueryAccount(ctx context.Context, q *pay.AccountQuery) (*pay.AccountInfo, error) {
	if c.supplier == nil {
		return nil, fmt.Errorf("通道[%s]是链上出款通道，未配置供应商侧账户查询链路", c.opt.Name)
	}
	return c.supplier.QueryAccount(ctx, q)
}

// QueryOnchainAccount 读热钱包在链上的 USDT 余额与 TRX gas 余额。
//
// 它只服务「还剩多少钱」这一类门禁（如分发器水位），不返回可用/冻结/授信：
// 那些是供应商侧的账本事实，链上读不出。读不到余额时返回可重试错误，让调用方
// 推迟而不是把「读失败」当成「余额为 0」——后者会把正常单全部挡死。
func (c *onchainChannel) QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error) {
	addr := strings.TrimSpace(c.wallet)
	contract := strings.TrimSpace(c.chain.Contract)
	currency := pay.CurrencyUSDT
	if q != nil {
		if v := strings.TrimSpace(q.Address); v != "" {
			addr = v
		}
		if v := strings.TrimSpace(q.Contract); v != "" {
			contract = v
		}
		if v := strings.TrimSpace(q.Currency); v != "" {
			currency = strings.ToUpper(v)
		}
	}
	if currency != pay.CurrencyUSDT {
		return nil, fmt.Errorf("通道[%s]只发 USDT-TRC20，链上余额查询不支持币种 %s", c.opt.Name, currency)
	}
	if addr == "" {
		return nil, fmt.Errorf("通道[%s]链上余额查询缺少地址（配置与查询都没给）", c.opt.Name)
	}
	if !IsValidTRONAddress(addr) {
		return nil, fmt.Errorf("通道[%s]链上余额查询的地址不是合法 TRON 地址", c.opt.Name)
	}
	if contract == "" {
		return nil, fmt.Errorf("通道[%s]链上余额查询缺少代币合约（配置与查询都没给）", c.opt.Name)
	}
	if !IsValidTRONAddress(contract) {
		return nil, fmt.Errorf("通道[%s]链上余额查询的合约不是合法 TRON 地址", c.opt.Name)
	}
	tok, err := c.gw.tokenBalance(ctx, addr, contract)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	acct, err := c.gw.account(ctx, addr)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "读不出原生币余额: " + c.redact(err.Error()), Retryable: true}
	}
	return &pay.AccountInfo{
		Currency:  pay.CurrencyUSDT,
		Balance:   tok,
		Available: tok,
		Status:    pay.AcctActive,
		RawNote:   fmt.Sprintf("链上余额 gas=%d sun", acct.BalanceSun),
	}, nil
}

// Withdraw 直接上链出款：组交易、本地签名、广播，并把结果按「钱有没有动」而不是「调用有没有返回」归一。
//
// 三条口径贯穿整个实现：
//  1. 先落台账再动钱——记不上台账就放弃广播（宁可这一单不发，不可发了查不到）；
//  2. 同一单号永不签第二笔——除「签好了但确定没交给节点」这一种情形；
//  3. 说不清就是 UNKNOWN 并交出 txid，等对账，绝不当成失败去重下单。
func (c *onchainChannel) Withdraw(ctx context.Context, o *pay.PayOrder) (*pay.PayResult, error) {
	if err := pay.CheckOrder(o); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.AmountUnits) != "" {
		return nil, fmt.Errorf("TRON 通道金额请用 amount（6 位最小单位），不支持 amount_units")
	}
	// 收款方性质先过：银行代付/商户号收款本包发不了，必须在地址解析之前就给出
	// 「走错了通道」而不是「地址不合法」。
	if err := pay.CheckTarget(o, pay.WithdrawTarget{Wallet: true}); err != nil {
		return nil, err
	}
	asset, err := c.assetOf(o)
	if err != nil {
		return nil, err
	}
	if err := checkOrderAddress(o); err != nil {
		return nil, err
	}
	if err := c.checkAsset(o, asset); err != nil {
		return nil, err
	}
	var res *pay.PayResult
	err = c.ledger.WithLock(o.OrderNo, func() error {
		var e error
		res, e = c.withdrawLocked(ctx, o, asset)
		return e
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// assetOf 解析这一单的出款资产：钱包、私钥、合约、网络。
//
// 优先级只有一条：PayOrder.From 给了就用 From，没给才回落配置。按单私钥必须与
// From.Address 派生结果逐字相等；From 只给地址时，该地址必须是配置热钱包——
// 本进程没有第二个钱包的私钥，写别的地址出去只会在签名那一步失败。
//
// 旧库把代币标准（trc20）填在 chain_network 里，这里按空处理：它不是网络，
// 不该参与 To.Network 的交叉校验；真正的网络由配置缺省值或 From.Network 决定。
func (c *onchainChannel) assetOf(o *pay.PayOrder) (orderAsset, error) {
	if o == nil {
		return orderAsset{}, errors.New("订单为空")
	}
	wallet := strings.TrimSpace(c.wallet)
	contract := strings.TrimSpace(c.chain.Contract)
	network := strings.TrimSpace(c.chain.Network)
	signer := c.sign
	secret := ""
	if o.From != nil {
		if k := o.From.KindOf(); k != "" && k != pay.PartyWallet {
			return orderAsset{}, fmt.Errorf("通道[%s]只能从链上钱包出款，这一单的 From 性质是 %s", c.opt.Name, k)
		}
		if keyHex := strings.TrimSpace(o.From.PrivateKey.Reveal()); keyHex != "" {
			key, err := sdkkeys.GetPrivateKeyFromHex(keyHex)
			if err != nil {
				// 错误里不带私钥原文：解析失败的文案可能回显输入，而这是资金密钥。
				return orderAsset{}, fmt.Errorf("通道[%s]的按单出款私钥不可用", c.opt.Name)
			}
			derived := address.BTCECPrivkeyToAddress(key).String()
			addr := strings.TrimSpace(o.From.Address)
			if addr == "" {
				addr = derived
			}
			if addr != derived {
				return orderAsset{}, fmt.Errorf("通道[%s]的按单私钥派生地址与 From.Address 不是同一个账户", c.opt.Name)
			}
			wallet = addr
			signer = softSigner(key)
			secret = keyHex
		} else if addr := strings.TrimSpace(o.From.Address); addr != "" {
			if signer == nil || wallet == "" || addr != wallet {
				return orderAsset{}, fmt.Errorf("通道[%s]只持有配置热钱包的私钥，这一单的出款方 %s 无法签名", c.opt.Name, addr)
			}
		}
		if v := strings.TrimSpace(o.From.Contract); v != "" {
			contract = v
		}
		if v := strings.TrimSpace(o.From.Network); v != "" {
			if !isTokenStandard(v) {
				network = v
			}
		}
	}
	if signer == nil {
		return orderAsset{}, fmt.Errorf("通道[%s]缺少出款私钥（配置未注入，From 也没提供）", c.opt.Name)
	}
	if wallet == "" {
		return orderAsset{}, fmt.Errorf("通道[%s]缺少出款地址（配置未填，From 也没提供）", c.opt.Name)
	}
	if contract == "" {
		return orderAsset{}, fmt.Errorf("通道[%s]缺少代币合约（配置与 From 都没给），链上出款拒绝", c.opt.Name)
	}
	// 节点属于哪张网是装配期事实；订单再声明一张不同的网，说明部署或线路配错了。
	// 两边都不填时不做判断：纯按单部署允许网络只写在 From 里。
	if cfgNet := strings.TrimSpace(c.chain.Network); cfgNet != "" && network != "" && !strings.EqualFold(cfgNet, network) {
		return orderAsset{}, fmt.Errorf("通道[%s]的节点属于 %s 网络，这一单的出款方却写着 %s：换网就是换币",
			c.opt.Name, cfgNet, network)
	}
	return orderAsset{wallet: wallet, contract: contract, network: network, sign: signer, secret: secret}, nil
}

// checkAsset 钉住「这一单发的确实是这一单声明的合约与网络」。
//
// 合约/网络来自 assetOf（From 优先、配置兜底），所以这里校验的是解析后的资产，
// 不再假设配置单一热钱包。
func (c *onchainChannel) checkAsset(o *pay.PayOrder, a orderAsset) error {
	if net := strings.TrimSpace(o.To.Network); net != "" {
		if a.network == "" || !strings.EqualFold(net, a.network) {
			return fmt.Errorf("通道[%s]这一单的合约 %s 属于 %q 网络，收款方却写着 %s：换网就是换币，不在这里替他做决定",
				c.opt.Name, a.contract, a.network, net)
		}
	}
	if cur := strings.ToUpper(strings.TrimSpace(o.Currency)); cur != "" && cur != pay.CurrencyUSDT {
		return fmt.Errorf("通道[%s]只发 USDT-TRC20（合约 %s），这一单的币种是 %s", c.opt.Name, a.contract, cur)
	}
	return nil
}

func (c *onchainChannel) withdrawLocked(ctx context.Context, o *pay.PayOrder, a orderAsset) (*pay.PayResult, error) {
	orderNo := strings.TrimSpace(o.OrderNo)
	entry, found := c.ledger.Get(orderNo)
	now := c.nowFn()

	// 同一单号二次提交时收端必须一致：单号是幂等键，拿同一个号发往另一个地址，
	// 要么是上游把两笔单搞混了，要么是想借幂等位改收款方——两种都不能往下走。
	// 逐字比（不做大小写归一）：TRON 的 base58 大小写不同就是另一个地址。
	if found && strings.TrimSpace(entry.To) != strings.TrimSpace(o.To.Address) {
		return nil, fmt.Errorf("订单号 %s 已在台账里且收款地址是 %s，这一单却写着 %s：同一幂等键不得改收款方",
			orderNo, entry.To, strings.TrimSpace(o.To.Address))
	}
	// 按单资产同样要钉住：同一单号换钱包/合约重签，等于把同一笔钱从另一个账户
	// 或另一种代币再发一次。空值表示旧台账没有这一维，不在这里补判。
	if found && entry.From != "" && strings.TrimSpace(entry.From) != a.wallet {
		return nil, fmt.Errorf("订单号 %s 已在台账里且出款钱包是 %s，这一单却写着 %s：同一幂等键不得换钱包",
			orderNo, entry.From, a.wallet)
	}
	if found && entry.Contract != "" && strings.TrimSpace(entry.Contract) != a.contract {
		return nil, fmt.Errorf("订单号 %s 已在台账里且合约是 %s，这一单却写着 %s：同一幂等键不得换币",
			orderNo, entry.Contract, a.contract)
	}

	switch {
	case found && entry.Terminal():
		return nil, fmt.Errorf("%w（order_no=%s tx=%s）", ErrOrderAlreadySettled, orderNo, entry.TxID)
	case found && entry.State == ledger.StateBroadcast:
		// 已经交给过节点：唯一允许的动作是查证，任何重建都是第二笔钱。
		return c.confirm(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared && !entry.Expired(now):
		// 签好了却没交出去（或交出去前就失败）：把同一份 RawTx 再广播一次。
		// 交易号由 raw_data 决定，重发同一份在节点侧天然去重，不会产生第二笔。
		return c.broadcastEntry(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared:
		// prepared 且已过链上有效期：这一笔不可能再上链，也没有第二笔的风险，重建。
	case found:
		// rejected 态：节点从没让这一笔进过池，钱没动过，允许同单号重建重发。
	}

	if !found {
		// 没有台账（多半是重启把进程内实现清了）：先向链上反查。
		// 命中只禁止广播，不把候选当本单结论：候选 txid 交出去供对账，
		// 状态保持 UNKNOWN，绝不写台账、绝不调用 confirm。
		candidate, err := c.lookupLedgerByChain(ctx, o, a, now)
		if err != nil {
			// 查不动不等于没发生：此时广播就是赌一次双付。
			return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "journal_unverified",
				Msg: "无法确认这一单是否已上过链，已放弃广播", Retryable: true}
		}
		if candidate != nil {
			return c.unknown(candidate, "链上反查命中同址同额候选，但无法证明属于本单；已停止广播，请对账或注入持久台账"), nil
		}
	}

	feeLimit := c.feeLimit()
	if err := c.precheckFunds(ctx, o, a, feeLimit); err != nil {
		return nil, err
	}
	tx, err := c.gw.buildTransfer(ctx, a.wallet, strings.TrimSpace(o.To.Address), a.contract, o.Amount, feeLimit)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "build_rejected", Msg: c.redactWith(a.secret, err.Error()), Retryable: true}
	}
	entry, err = c.prepare(o, tx, a)
	if err != nil {
		return nil, err
	}
	return c.broadcastEntry(ctx, entry, now)
}

// prepare 签名并把这一笔写进台账；记账失败即放弃广播。
func (c *onchainChannel) prepare(o *pay.PayOrder, tx *core.Transaction, a orderAsset) (*ledger.Entry, error) {
	signed, err := a.sign(tx)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "sign_failed", Msg: c.redactWith(a.secret, err.Error()), Retryable: false}
	}
	txID, err := txIDOf(signed)
	if err != nil {
		return nil, err
	}
	raw, err := proto.Marshal(signed)
	if err != nil {
		return nil, fmt.Errorf("已签名交易无法序列化进台账: %w", err)
	}
	entry := &ledger.Entry{
		OrderNo:     strings.TrimSpace(o.OrderNo),
		TxID:        txID,
		RawTx:       raw,
		From:        a.wallet,
		To:          strings.TrimSpace(o.To.Address),
		Contract:    a.contract,
		Amount:      o.Amount,
		FeeLimit:    c.feeLimit(),
		State:       ledger.StatePrepared,
		ExpiresAtMS: signed.GetRawData().GetExpiration(),
		UpdatedAtMS: ledger.NowMS(c.nowFn()),
	}
	// 记账排在广播之前：反过来就会留下「链上有一笔、本地查不到」的状态，那正是双付的温床。
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// broadcastEntry 广播台账里这一笔。RawTx 逐字不变，因此同一单号重复走到这里也只有一笔钱。
func (c *onchainChannel) broadcastEntry(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	tx := &core.Transaction{}
	if err := proto.Unmarshal(entry.RawTx, tx); err != nil {
		return nil, fmt.Errorf("台账里的已签名交易不可还原（order_no=%s）: %w", entry.OrderNo, err)
	}
	entry.State = ledger.StateBroadcast
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	res, err := c.gw.broadcast(ctx, tx)
	if err != nil {
		// 请求已经可能到达节点：状态停在 broadcast，结论交给按 txid 的查证。
		return c.unknown(entry, "广播结果未知: "+c.redact(err.Error())), nil
	}
	switch {
	case res.Rejected:
		// 节点在入池之前就拒了（签名/TAPOS/过期/超限一类）：这一笔不可能上链。
		// 它不算终局——单独记一个状态，让同单号后续可以重建重发（见 query.go）。
		return c.markRejected(entry, res.Code, now)
	default:
		// 受理了，或说不清：两者都只能去按 txid 查证，绝不在此处再签一笔。
		return c.confirm(ctx, entry, now)
	}
}

// markRejected 记下「节点没让这一笔进池」并给出可重发的失败回执。
//
// 用独立状态而不是删条目：删掉会让下一次调用看不见「这一单号已经试过了」，
// 留下状态既能允许重建，也能在日志里数出这一单号被拒过几次。
func (c *onchainChannel) markRejected(entry *ledger.Entry, code string, now time.Time) (*pay.PayResult, error) {
	entry.State = ledger.StateRejected
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusFailed, Amount: entry.Amount,
		RawNote: c.note("节点拒绝入池 code=" + code + "（未发生资金动作，可重发）"),
	}, nil
}

// feeLimit 取配置值，缺省按链上缺省。
func (c *onchainChannel) feeLimit() int64 {
	if c.chain.FeeLimitSun > 0 {
		return c.chain.FeeLimitSun
	}
	return defaultTRC20FeeLimitSun
}

// precheckFunds 在签名之前把「发出去必然失败」的几种情况拦下来。
//
// 顺序是有原因的：先看本单币种够不够，再看 gas 币够不够——后者不够时这一样会失败
// 且可能已经烧掉能量，所以宁可本地拒掉。
func (c *onchainChannel) precheckFunds(ctx context.Context, o *pay.PayOrder, a orderAsset, feeLimit int64) error {
	tokenBal, err := c.gw.tokenBalance(ctx, a.wallet, a.contract)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread", Msg: "读不出代币余额: " + c.redactWith(a.secret, err.Error()), Retryable: true}
	}
	if tokenBal < o.Amount {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_token",
			Msg: fmt.Sprintf("热钱包 USDT 余额 %d 不足以支付本单 %d（最小单位）", tokenBal, o.Amount), Retryable: false}
	}
	acct, err := c.gw.account(ctx, a.wallet)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread", Msg: "读不出原生币余额: " + c.redactWith(a.secret, err.Error()), Retryable: true}
	}
	if acct.BalanceSun < feeLimit {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_gas",
			Msg: fmt.Sprintf("热钱包 TRX 余额 %d sun 低于本单手续费上限 %d sun：gas 收在原生币上，USDT 够也发不出去",
				acct.BalanceSun, feeLimit), Retryable: false}
	}
	return nil
}

// unknown 给一条「说不清」的回执：带上 txid，让对账有把手。
func (c *onchainChannel) unknown(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusUnknown, Amount: entry.Amount, RawNote: c.note(note),
	}
}

// note 是对外摘要的唯一出口：先抹凭证再截断。
//
// 顺序不能反——截断可能正好把一个凭证切在中间留下片段，而抹除必须看得到整串。
func (c *onchainChannel) note(s string) string { return util.Clip(c.redact(s)) }

// redact 把凭证从对外文案里摘掉。
//
// 链上错误会原样进 PayResult.RawNote，而 RawNote 是按「可以公开给日志」设计的字段：
// 节点或接入点的一句报错里带出 API Key（例如把请求头回显在 401 文本里），
// 这条纪律就当众破了。私钥同理——它不该以任何形式出现在返回值里。
func (c *onchainChannel) redact(s string) string {
	for _, secret := range []string{c.chain.APIKey, c.chain.PrivateKey} {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
}

// redactWith 在配置凭证之外再抹掉这一单的按单私钥。
//
// 按单私钥不在配置里，c.redact 看不到它；而组包/签名错误有可能回显输入。
func (c *onchainChannel) redactWith(secret, s string) string {
	s = c.redact(s)
	if secret != "" && strings.Contains(s, secret) {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}
