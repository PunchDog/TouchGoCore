package bsc

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"touchgocore/ledger"
	"touchgocore/pay"
)

// 本文件是 USDT-BEP20 的链上出款机器：组 ERC-20 transfer、本地签名、广播。
//
// 它与 tron/onchain.go 同一个骨架（先记账后广播、同一单号永不签第二笔、
// 说不清就是 UNKNOWN），差异全部来自 EVM 本身：
//
//   - **nonce**：TRON 的交易有有效期，EVM 的没有；交易靠账户 nonce 排序，因此
//     「能否上链」这件事可以靠已落块 nonce 判定——这是 EVM 白送的一条确定性判据
//     （见 query.go 的「nonce 已被占」分支）。
//   - **gas 计价**：TRON 用 fee_limit 封顶烧掉多少 TRX，EVM 是 gas_limit × gas_price。
//     后者只能靠「gas_price 上限」来闸，所以配置里那个 gas_price_gwei 是下限，
//     而节点报价超过它 2 倍时本通道拒单（见 gasPrice）。
//   - **金额精度**：USDT-BEP20 是 18 位小数。PayOrder 同时支持 int64 Amount
//     与十进制字符串 AmountUnits；本链统一用 big.Int 组 transfer、预检与台账，
//     超过 int64 的金额必须走 AmountUnits（trade data 仍是精确大整数）。
const (
	// defaultBSCDecimals 是 USDT-BEP20 的小数位。配置留空时用它，
	// 但装配期一定会拿合约自己的 decimals() 复核（见 newBscOnchainChannel）。
	defaultBSCDecimals = 18
	// defaultTransferGasLimit 是 ERC-20 transfer 的兜底 gas 上限。
	// 真实用量约 51k（目标账户已持币时更低），估算不出来时按这个数发。
	defaultTransferGasLimit = 100_000
	// minTransferGasLimit 是 gas 上限的地板：低于它几乎必然 out of gas。
	minTransferGasLimit = 21_000
	// gasPriceCeilingFactor 是节点报价相对配置下限的容忍倍数。超过即拒单：
	// 没有上限的 gas 价等于把「本单可能烧掉多少钱」交给节点说了算。
	gasPriceCeilingFactor = 2
	// scanWindow 是「台账查不到这一单号」时向链上反查的时间窗。
	//
	// 与 tron 取同一个量级：旧台账丢失后要防的是「上一次其实已经上链了」，
	// 窗口取窄等于把双付风险留给自己。
	scanWindow = 72 * time.Hour
	// logBlockSpan 是一次 eth_getLogs 覆盖的区块跨度。
	//
	// 取 2000 而不是整窗：绝大多数 BSC 服务商对 getLogs 的区块跨度有硬上限
	// （超出直接报错），按 2000 分段才能稳定读完 72 小时。
	logBlockSpan = 2000
	// maxLogChunks 是反查最多扫多少段。超出即拒绝广播：
	// 「没扫完」与「扫完没命中」是两件事，前者不能当成「没发过」。
	maxLogChunks = 400
)

// onchainChannel 是一条直接动 BSC 的资金通道。
type onchainChannel struct {
	rpc    *bscRPC
	sign   *signer
	opt    channelOptions
	chain  *chainConfig
	ledger *ledger.Ledger
	// token 是 BEP20 合约地址（已过格式校验）。
	token common.Address
	nowFn func() time.Time

	// sendMu 把「取 nonce → 签名 → 广播」串成一段。它不是性能优化而是正确性要求：
	// 同一热钱包的两笔交易若各拿到同一个 nonce，链上只会执行其中一笔，
	// 另一笔变成死单——而两边的上游都以为自己的单发出去了。
	sendMu sync.Mutex
	// nonces 是「每个出款地址本地已知被占用过的下一个 nonce」。
	//
	// 它只覆盖节点集群短暂不同步的那一小段：广播成功后 pendingNonce 立刻应该 +1，
	// 但负载均衡后面的节点可能还没看见。**失败时不推进**——推进一个从未进池的
	// nonce 会在链上留一个空洞，后续交易全部堆在它后面等（EVM 的 nonce 必须连续）。
	// 按地址分桶是必须的：按单私钥允许多个热钱包，共用一个 uint64 会让 A 钱包
	// 的缓存去推进 B 钱包的 nonce。
	nonces map[common.Address]uint64
}

// newBscOnchainChannel 装配一条 BEP20 出款通道。
//
// 装配期把五件事钉死：合约地址、合约自己的 decimals、链 ID、私钥派生地址、gas 参数。
// 这五样里任何一样要在第一笔出款时才暴露，代价都是「钱已经动了一部分」。
func newBscOnchainChannel(opt channelOptions) (*onchainChannel, error) {
	chain := opt.Chain
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]缺少 bsc.usdt.chain（RPC 与热钱包地址）", opt.Name)
	}
	contract := strings.TrimSpace(opt.Contract)
	if contract == "" {
		return nil, fmt.Errorf("通道[%s]未配置代币合约（bsc.usdt.contract），链上出款拒绝", opt.Name)
	}
	if !IsValidEVMAddress(contract) {
		return nil, fmt.Errorf("通道[%s]的代币合约地址 %s 不合法：应为 0x 开头的 40 位十六进制", opt.Name, contract)
	}
	if strings.TrimSpace(chain.WalletAddress) == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.wallet_address 为空，出款主体必须能在配置里读出来", opt.Name)
	}
	decimals := opt.Decimals
	if decimals <= 0 {
		decimals = defaultBSCDecimals
	}
	sgn, err := newSigner(chain.PrivateKey, big.NewInt(chain.ChainID), chain.WalletAddress)
	if err != nil {
		return nil, fmt.Errorf("通道[%s]：%w", opt.Name, err)
	}
	rpc, err := newBSCRPC(chain, opt.Name)
	if err != nil {
		return nil, err
	}
	c := &onchainChannel{
		rpc:    rpc,
		sign:   sgn,
		opt:    opt,
		chain:  chain,
		ledger: bscLedger,
		token:  common.HexToAddress(contract),
		nowFn:  time.Now,
		nonces: make(map[common.Address]uint64),
	}
	c.opt.Contract = contract
	c.opt.Decimals = decimals
	if err := c.verifyChain(context.Background()); err != nil {
		rpc.Close()
		return nil, err
	}
	if err := c.verifyDecimals(context.Background()); err != nil {
		rpc.Close()
		return nil, err
	}
	return c, nil
}

// verifyChain 要求节点回报的 chainId 与配置一致。
//
// 对不上就是「配置写着主网、RPC 连着 Chapel」——签名用的 v 由配置的 chain ID 决定，
// 节点按自己的链 ID 校验，结果不是签名被拒（至少失败得明白），
// 而是把主网单发进了测试网（或者反过来），钱在另一条链上，谁也找不回来。
func (c *onchainChannel) verifyChain(ctx context.Context) error {
	onchain, err := c.rpc.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("通道[%s]读不出节点的 chainId：%v", c.opt.Name, err)
	}
	if onchain == nil || onchain.Int64() != c.chain.ChainID {
		return fmt.Errorf("通道[%s]的 chain.chain_id=%d 与节点回报的 %v 不一致：换网就是换币，拒绝装配",
			c.opt.Name, c.chain.ChainID, onchain)
	}
	return nil
}

// verifyDecimals 拿合约自己的 decimals() 复核配置的小数位。
//
// 这是本通道最要紧的一道装配闸：USDT 在 TRON 是 6 位、在 BSC 是 18 位，
// 把 6 抄进 BSC 的配置，Amount 的 10^12 倍误差不会报错、只会把钱多发出去。
// 合约自己说的位数是唯一可信来源，配置只是「我以为」。
func (c *onchainChannel) verifyDecimals(ctx context.Context) error {
	got, err := c.rpc.ContractDecimals(ctx, c.token)
	if err != nil {
		// 读不出来就不启动：宁可这条通道不工作，也不能带着一个未经核对的小数位出款。
		return fmt.Errorf("通道[%s]读不出合约 %s 的 decimals：%v", c.opt.Name, c.token.Hex(), err)
	}
	if got != c.opt.Decimals {
		return fmt.Errorf("通道[%s]的 bsc.usdt.decimals=%d 与合约 %s 的 decimals()=%d 不一致：小数位错一位就是 10^n 倍",
			c.opt.Name, c.opt.Decimals, c.token.Hex(), got)
	}
	return nil
}

// Close 释放接入点连接（供通道停启用）。
func (c *onchainChannel) Close() error { return c.rpc.Close() }

// Withdraw 直接上链出款。
//
// 三条口径与 tron 逐字一致：
//  1. 先落台账再动钱——记不上台账就放弃广播；
//  2. 同一单号永不签第二笔——除「签好了但确定没交给节点」这一种情形；
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
	a, err := c.assetOf(o)
	if err != nil {
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
	var res *pay.PayResult
	err = c.ledger.WithLock(strings.TrimSpace(o.OrderNo), func() error {
		var e error
		res, e = c.withdrawLocked(ctx, o, a)
		return e
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// orderAsset 是这一单实际使用的出款账户。
//
// 合约与网络仍钉配置：本轮只放开按单私钥/地址，不放开按单换币。per-contract
// 的 decimals 必须逐张合约核对，否则 10^n 的倍率错误不会报错、只会多付。
type orderAsset struct {
	wallet common.Address
	sign   *signer
	secret string
}

// assetOf 解析这一单的出款私钥与地址：From 给了按单私钥就用它，否则回落配置热钱包。
//
// 按单私钥派生出的地址必须与 From.Address 相等；只给地址时必须等于配置热钱包,
// 因为本进程没有第二个钱包的私钥。合约/网络与配置不一致直接拒，不做静默覆盖。
func (c *onchainChannel) assetOf(o *pay.PayOrder) (orderAsset, error) {
	if o == nil {
		return orderAsset{}, fmt.Errorf("通道[%s]的订单为空", c.opt.Name)
	}
	a := orderAsset{wallet: c.sign.Address(), sign: c.sign}
	if o.From == nil {
		return a, nil
	}
	if k := o.From.KindOf(); k != "" && k != pay.PartyWallet {
		return orderAsset{}, fmt.Errorf("通道[%s]只能从链上钱包出款，这一单的 From 性质是 %s", c.opt.Name, k)
	}
	if keyHex := strings.TrimSpace(o.From.PrivateKey.Reveal()); keyHex != "" {
		sgn, err := newSignerFromKey(keyHex, big.NewInt(c.chain.ChainID))
		if err != nil {
			// 错误里不带私钥原文：解析失败的文案可能回显输入，而这是资金密钥。
			return orderAsset{}, fmt.Errorf("通道[%s]的按单出款私钥不可用", c.opt.Name)
		}
		addr := strings.TrimSpace(o.From.Address)
		if addr == "" {
			addr = sgn.Address().Hex()
		}
		if !IsValidEVMAddress(addr) {
			return orderAsset{}, fmt.Errorf("通道[%s]的 from.address %q 不是合法的 EVM 地址", c.opt.Name, addr)
		}
		if common.HexToAddress(addr) != sgn.Address() {
			return orderAsset{}, fmt.Errorf("通道[%s]的按单私钥派生地址与 From.Address 不是同一个账户", c.opt.Name)
		}
		a.wallet = sgn.Address()
		a.sign = sgn
		a.secret = keyHex
	} else if addr := strings.TrimSpace(o.From.Address); addr != "" {
		if !IsValidEVMAddress(addr) {
			return orderAsset{}, fmt.Errorf("通道[%s]的 from.address %q 不是合法的 EVM 地址", c.opt.Name, addr)
		}
		if common.HexToAddress(addr) != c.sign.Address() {
			return orderAsset{}, fmt.Errorf("通道[%s]只持有配置热钱包 %s 的私钥，这一单的出款方 %s 无法签名",
				c.opt.Name, c.sign.Address().Hex(), addr)
		}
	}
	if contract := strings.TrimSpace(o.From.Contract); contract != "" && !strings.EqualFold(contract, c.opt.Contract) {
		return orderAsset{}, fmt.Errorf("通道[%s]只发合约 %s，这一单却指定了 %s：同一通道不得按单换币", c.opt.Name, c.opt.Contract, contract)
	}
	if net := strings.TrimSpace(o.From.Network); net != "" && !strings.EqualFold(net, c.opt.Network) {
		return orderAsset{}, fmt.Errorf("通道[%s]配的是 %s 网络，这一单的出款方却写着 %s", c.opt.Name, c.opt.Network, net)
	}
	return a, nil
}

// entryWallet 取台账这一笔实际使用的出款地址。
//
// 旧台账没有 From 列值时回落配置热钱包；有值时按字节解析，坏值不能被当成零地址
// 去查 nonce（那会把「读坏了」变成「另一个账户的 nonce 越过了本单」）。
func (c *onchainChannel) entryWallet(entry *ledger.Entry) (common.Address, error) {
	if entry != nil {
		if v := strings.TrimSpace(entry.From); v != "" {
			if !IsValidEVMAddress(v) {
				return common.Address{}, fmt.Errorf("台账里的出款地址 %q 不是合法 EVM 地址", v)
			}
			return common.HexToAddress(v), nil
		}
	}
	return c.sign.Address(), nil
}

// checkAsset 钉住「这一单发的确实是本通道配置的那张合约、那张网」。
func (c *onchainChannel) checkAsset(o *pay.PayOrder) error {
	if net := strings.TrimSpace(o.To.Network); net != "" && !strings.EqualFold(net, c.opt.Network) {
		return fmt.Errorf("通道[%s]配的是 %s 网络的合约 %s，这一单却写着 %s：换网就是换币，不在这里替他做决定",
			c.opt.Name, c.opt.Network, c.opt.Contract, net)
	}
	if cur := strings.ToUpper(strings.TrimSpace(o.Currency)); cur != "" && cur != pay.CurrencyUSDT {
		return fmt.Errorf("通道[%s]只发 USDT-BEP20（合约 %s），这一单的币种是 %s", c.opt.Name, c.opt.Contract, cur)
	}
	return nil
}

// checkDecimals 交叉校验「Amount 这个整数对应几位小数」。
//
// 填了就必须与配置逐字相等；留 0 表示不声明，按配置解释。
// 只比配置不比合约：装配期已经拿合约的 decimals() 与配置核对过一次，
// 这里再问一次链只会把「配置错了」与「链读不出来」混成同一个错误。
func (c *onchainChannel) checkDecimals(o *pay.PayOrder) error {
	if o.Decimals == 0 || o.Decimals == c.opt.Decimals {
		return nil
	}
	return fmt.Errorf("通道[%s]的小数位是 %d，这一单声明的 decimals=%d：位数取错的后果是 10^n 倍的不可逆多付",
		c.opt.Name, c.opt.Decimals, o.Decimals)
}

func (c *onchainChannel) withdrawLocked(ctx context.Context, o *pay.PayOrder, a orderAsset) (*pay.PayResult, error) {
	orderNo := strings.TrimSpace(o.OrderNo)
	entry, found := c.ledger.Get(orderNo)
	now := c.nowFn()
	to := common.HexToAddress(strings.TrimSpace(o.To.Address))

	// 同一单号二次提交时收端必须一致（按字节比，理由见 sameRecipient）。
	if found && !sameRecipient(entry.To, to) {
		return nil, fmt.Errorf("订单号 %s 已在台账里且收款地址是 %s，这一单却写着 %s：同一幂等键不得改收款方",
			orderNo, entry.To, to.Hex())
	}
	// 按单资产也要钉住：同一单号换钱包/合约重签，等于把同一笔钱从另一个账户
	// 或另一种代币再发一次。空值表示旧台账没有这一维，不在这里补判。
	if found {
		if recorded := strings.TrimSpace(entry.From); recorded != "" {
			from, err := c.entryWallet(entry)
			if err != nil {
				return nil, err
			}
			if from != a.wallet {
				return nil, fmt.Errorf("订单号 %s 已在台账里且出款钱包是 %s，这一单却写着 %s：同一幂等键不得换钱包",
					orderNo, from.Hex(), a.wallet.Hex())
			}
		}
		if recorded := strings.TrimSpace(entry.Contract); recorded != "" && !strings.EqualFold(recorded, c.opt.Contract) {
			return nil, fmt.Errorf("订单号 %s 已在台账里且合约是 %s，这一单却写着 %s：同一幂等键不得换币",
				orderNo, recorded, c.opt.Contract)
		}
	}

	switch {
	case found && entry.Terminal():
		return nil, fmt.Errorf("%w（order_no=%s tx=%s）", ErrOrderAlreadySettled, orderNo, entry.TxID)
	case found && entry.State == ledger.StateBroadcast:
		// 已经交给过节点：唯一允许的动作是查证，任何重建都是第二笔钱。
		return c.confirm(ctx, entry, now)
	case found && entry.State == ledger.StatePrepared:
		// 签好了却没交出去（或交出去前就失败）：把同一份 RawTx 再广播一次。
		// EVM 的交易没有链上有效期，因此这里没有 tron 那套「prepared 且过期则重建」：
		// raw tx 永远可重广播，按哈希天然去重，不会产生第二笔。
		return c.broadcastEntry(ctx, entry, now)
	case found:
		// rejected 态：节点从没让这一笔进过池，钱没动过，允许同单号重建重发。
	}

	if !found {
		// 没有台账（多半是重启把进程内实现清了）：先向链上反查，命中就禁止广播。
		if hit, err := c.lookupLedgerByChain(ctx, o, a, now); err != nil {
			// 查不动不等于没发生：此时广播就是赌一次双付。
			return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "journal_unverified",
				Msg: "无法确认这一单是否已上过链，已放弃广播", Retryable: true}
		} else if hit != nil {
			return c.confirm(ctx, hit, now)
		}
	}

	return c.send(ctx, o, to, a, now)
}

// sameRecipient 判断台账里的收端与本单的收端是不是同一个地址。
//
// EVM 地址的同一性按字节判而不是按字符串判：同一个地址有 EIP-55 校验和形态、
// 全小写、全大写三种合法写法，逐字比会把「同一地址换个写法」误报成「改了收款方」，
// 那会把一次合法的重试拦在门外（而重试恰恰是资金链路最常见的调用形态）。
// （TRON 那侧相反：base58 大小写不同就是另一个地址，所以那边必须逐字比。）
func sameRecipient(recorded string, to common.Address) bool {
	if strings.TrimSpace(recorded) == "" {
		return true
	}
	return common.HexToAddress(recorded) == to
}

// send 在钱包级互斥里完成「取 nonce → 估算 gas → 预检 → 签名 → 记账 → 广播」。
func (c *onchainChannel) send(ctx context.Context, o *pay.PayOrder, to common.Address, a orderAsset, now time.Time) (*pay.PayResult, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	amount, err := o.AmountBig()
	if err != nil {
		return nil, err
	}
	gasPrice, err := c.gasPrice(ctx)
	if err != nil {
		return nil, err
	}
	data := erc20TransferData(to, amount)
	gasLimit, err := c.gasLimit(ctx, data, a.wallet)
	if err != nil {
		return nil, err
	}
	if err := c.precheckFunds(ctx, amount, gasLimit, gasPrice, a.wallet); err != nil {
		return nil, err
	}
	nonce, err := c.allocateNonce(ctx, a.wallet)
	if err != nil {
		return nil, err
	}
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: gasPrice,
		Gas:      gasLimit,
		To:       &c.token,
		Value:    big.NewInt(0),
		Data:     data,
	})
	entry, err := c.prepare(o, tx, to, a, amount, now)
	if err != nil {
		return nil, err
	}
	return c.broadcastEntry(ctx, entry, now)
}

// gasPrice 定这一单的 gas 价：配置值是下限，节点报价超过它的 2 倍即拒单。
//
// 用遗留交易（type 0）而不是 EIP-1559：BSC 的 base fee 机制与以太坊不同，
// 一笔 legacy 交易只有一个 gas 价，本单最多烧掉多少钱在签名那一刻就是确定的。
// EIP-1559 的 maxFee/maxPriorityFee 会把「实际烧多少」交回给节点。
func (c *onchainChannel) gasPrice(ctx context.Context) (*big.Int, error) {
	suggested, err := c.rpc.SuggestGasPrice(ctx)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "gasprice_unread",
			Msg: "读不出节点 gas 价: " + c.redact(err.Error()), Retryable: true}
	}
	if suggested == nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "gasprice_unread",
			Msg: "节点没有返回 gas 价", Retryable: true}
	}
	floor := gweiToWei(c.chain.GasPriceGwei)
	if floor.Sign() <= 0 {
		// 没配下限：用节点价，但这条日志要留——没有上限的 gas 价等于没有闸。
		return suggested, nil
	}
	ceiling := new(big.Int).Mul(floor, big.NewInt(gasPriceCeilingFactor))
	if suggested.Cmp(ceiling) > 0 {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "gas_price_spike",
			Msg: fmt.Sprintf("节点 gas 价 %s wei 超过配置下限 %s wei 的 %d 倍，本单拒发（价格回落后可原单重试）",
				suggested, floor, gasPriceCeilingFactor), Retryable: true}
	}
	if suggested.Cmp(floor) < 0 {
		return floor, nil
	}
	return suggested, nil
}

// gasLimit 定 gas 上限：配置优先，但要挡住「配置小于估算值」这种必然 out of gas 的写法。
func (c *onchainChannel) gasLimit(ctx context.Context, data []byte, from common.Address) (uint64, error) {
	estimated, err := c.rpc.EstimateGas(ctx, ethereum.CallMsg{
		From: from,
		To:   &c.token,
		Data: data,
	})
	if err != nil {
		// 估算失败通常意味着这一笔必然失败（余额不足、合约拒绝、地址被拉黑）：
		// 这里报出去，比签了名广播出去再烧掉 gas 好。
		return 0, &pay.ProviderError{Channel: c.opt.Name, Code: "gas_estimate_failed",
			Msg: "节点估算 gas 失败（这一笔很可能本就发不出去）: " + c.redact(err.Error()), Retryable: false}
	}
	// 在估算值上留 20% 余量：状态在「估算」与「落块」之间可能变化
	// （收款方刚被转出过币、存储槽从冷变热一类），贴着估算值发容易 out of gas。
	need := estimated + estimated/5
	if need < minTransferGasLimit {
		need = minTransferGasLimit
	}
	if need < defaultTransferGasLimit {
		// 兜底值只在小额 transfer 上生效：ERC-20 transfer 到已持币账户约 51k，
		// 到新账户要写 storage，可能超过 60k。估算值更低时说明这一笔确实更便宜，
		// 但仍按 100k 发——gas 上限只是许可，未用掉的部分不会扣钱。
		need = defaultTransferGasLimit
	}
	if c.chain.GasLimit > 0 {
		if c.chain.GasLimit < estimated {
			return 0, fmt.Errorf("通道[%s]的 chain.gas_limit=%d 低于节点估算的 %d：按配置发必然 out of gas",
				c.opt.Name, c.chain.GasLimit, estimated)
		}
		return c.chain.GasLimit, nil
	}
	return need, nil
}

// allocateNonce 取这一单的 nonce。
//
// 以节点的 pendingNonce 为准，用本地缓存兜住「节点集群还没同步到上一笔」的窗口。
// 缓存只增不减：链上 nonce 只会往前走，回头只会撞上已落块的交易。
func (c *onchainChannel) allocateNonce(ctx context.Context, from common.Address) (uint64, error) {
	pending, err := c.rpc.PendingNonce(ctx, from)
	if err != nil {
		return 0, &pay.ProviderError{Channel: c.opt.Name, Code: "nonce_unread",
			Msg: "读不出账户 nonce: " + c.redact(err.Error()), Retryable: true}
	}
	if next, ok := c.nonces[from]; ok && next > pending {
		return next, nil
	}
	return pending, nil
}

// adoptNonce 记下「这个地址的 nonce 已经被链上接受了」。
//
// 只在节点明确受理（含 already known）时推进：失败时推进一个从未进池的 nonce，
// 会在链上留一个空洞，后面每一笔都要等它被填上。
func (c *onchainChannel) adoptNonce(from common.Address, nonce uint64) {
	if c.nonces == nil {
		c.nonces = make(map[common.Address]uint64)
	}
	c.nonces[from] = nonce + 1
}

// precheckFunds 在签名之前把「发出去必然失败」的两种情况拦下来。
//
// 顺序有原因：先看本单币种够不够，再看 gas 币够不够——后者不够时这一笔同样会失败，
// 而失败也会烧掉估算与广播的往返，还可能让上游误以为是「链上已经动过」。
func (c *onchainChannel) precheckFunds(ctx context.Context, amount *big.Int, gasLimit uint64, gasPrice *big.Int, from common.Address) error {
	bal, err := c.rpc.TokenBalance(ctx, c.token, from)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if amount == nil || amount.Sign() <= 0 {
		return fmt.Errorf("出款金额必须为正整数最小单位")
	}
	if bal == nil || bal.Cmp(amount) < 0 {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_token",
			Msg:       fmt.Sprintf("出款账户 USDT 余额 %s 不足以支付本单 %s（最小单位）", bigString(bal), amount.String()),
			Retryable: false}
	}
	need := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), gasPrice)
	native, err := c.rpc.NativeBalance(ctx, from)
	if err != nil {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "account_unread",
			Msg: "读不出原生币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if native == nil || native.Cmp(need) < 0 {
		return &pay.ProviderError{Channel: c.opt.Name, Code: "insufficient_gas",
			Msg: fmt.Sprintf("出款账户 BNB 余额 %s wei 低于本单 gas 上限 %s wei：gas 收在原生币上，USDT 够也发不出去",
				bigString(native), need.String()), Retryable: false}
	}
	return nil
}

// prepare 签名并把这一笔写进台账；记账失败即放弃广播。
func (c *onchainChannel) prepare(o *pay.PayOrder, tx *types.Transaction, to common.Address, a orderAsset, amount *big.Int, now time.Time) (*ledger.Entry, error) {
	signed, err := a.sign.Sign(tx)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "sign_failed", Msg: c.redactWith(a.secret, err.Error()), Retryable: false}
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("已签名交易无法序列化进台账: %w", err)
	}
	entry := &ledger.Entry{
		OrderNo:     strings.TrimSpace(o.OrderNo),
		TxID:        txHash(signed),
		RawTx:       raw,
		From:        a.wallet.Hex(),
		To:          to.Hex(),
		Contract:    c.opt.Contract,
		Amount:      saturateInt64(amount),
		AmountUnits: amount.String(),
		FeeLimit:    int64(signed.Gas()),
		State:       ledger.StatePrepared,
		UpdatedAtMS: ledger.NowMS(now),
		// ExpiresAtMS 留 0：EVM 的交易没有链上有效期，它的「死期」由 nonce 被占用决定，
		// 那件事只能查出来（见 query.go），不能按时间算出来。
	}
	// 记账排在广播之前：反过来就会留下「链上有一笔、本地查不到」的状态，那正是双付的温床。
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	return entry, nil
}

// broadcastEntry 广播台账里这一笔。RawTx 逐字不变，因此同一单号重复走到这里也只有一笔钱。
func (c *onchainChannel) broadcastEntry(ctx context.Context, entry *ledger.Entry, now time.Time) (*pay.PayResult, error) {
	raw := entry.RawTx
	if len(raw) == 0 {
		return nil, fmt.Errorf("台账里的已签名交易为空（order_no=%s）", entry.OrderNo)
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, fmt.Errorf("台账里的已签名交易不可还原（order_no=%s）: %w", entry.OrderNo, err)
	}
	entry.State = ledger.StateBroadcast
	entry.UpdatedAtMS = ledger.NowMS(now)
	if err := c.ledger.Put(entry); err != nil {
		return nil, err
	}
	if _, err := c.rpc.SendRaw(ctx, raw); err != nil {
		switch classifySendError(err) {
		case rejectBeforePool:
			// 节点在入池前就拒了（余额不足、gas 低于节点底线、签名不合法一类）：
			// 这一笔不可能上链。它不是终局——单独记一个状态，让同单号后续可以重建重发。
			return c.markRejected(entry, c.redact(err.Error()), now)
		case rejectAlreadyKnown:
			// 节点说它已经见过这一笔：钱可能已经在路上，只能去查证。
			if from, ferr := c.entryWallet(entry); ferr == nil {
				c.adoptNonce(from, tx.Nonce())
			}
			return c.confirm(ctx, entry, now)
		case rejectNonceUsed:
			// nonce 位已被占用：占用它的可能正是本单上一次广播的那一笔，
			// 也可能是同钱包另一单。两种情况都只能按哈希查证，绝不在此处重签。
			return c.confirm(ctx, entry, now)
		default:
			// 请求已经可能到达节点：状态停在 broadcast，结论交给按哈希的查证。
			return c.unknown(entry, "广播结果未知: "+c.redact(err.Error())), nil
		}
	}
	// 节点受理：本地缓存推进一位，覆盖「集群还没同步」的那一小段窗口。
	if from, ferr := c.entryWallet(entry); ferr == nil {
		c.adoptNonce(from, tx.Nonce())
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
		Status: pay.StatusFailed, Amount: entry.Amount, AmountUnits: entry.AmountUnits,
		RawNote: c.note("节点拒绝入池 " + reason + "（未发生资金动作，可重发）"),
	}, nil
}

// QueryAccount 读热钱包在合约上的 USDT 余额。
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
	bal, err := c.rpc.TokenBalance(ctx, c.token, c.sign.Address())
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if bal == nil {
		bal = new(big.Int)
	}
	info := &pay.AccountInfo{
		MerchantID: c.sign.Address().Hex(),
		Currency:   pay.CurrencyUSDT,
		Balance:    saturateInt64(bal),
		Available:  saturateInt64(bal),
		Status:     pay.AcctActive,
	}
	info.BalanceUnits = bal.String()
	info.AvailableUnits = bal.String()
	if !bal.IsInt64() {
		// 18 位小数下这个分支很常见（1 个 USDT 就是 1e18）。int64 字段饱和，
		// 精确值在 BalanceUnits/AvailableUnits；水位判断必须读后者。
		info.RawNote = "余额超出 int64 可表达范围，已按上限饱和（精确值见 balance_units/available_units）"
	}
	if native, err := c.rpc.NativeBalance(ctx, c.sign.Address()); err == nil && native != nil {
		info.RawNote = strings.TrimSpace(info.RawNote + " gas 余额 " + native.String() + " wei")
	}
	return info, nil
}

// QueryOnchainAccount 按查询里的地址/合约读链上余额（缺省用配置热钱包与合约）。
//
// 与 QueryAccount 同源（都是 balanceOf + 原生币余额），区别只在地址可指定：
// whatapp 按单携带热钱包时，水位要看的是那一单实际出款地址，而不是配置默认值。
func (c *onchainChannel) QueryOnchainAccount(ctx context.Context, q *pay.OnchainAccountQuery) (*pay.AccountInfo, error) {
	addr := c.sign.Address()
	token := c.token
	currency := pay.CurrencyUSDT
	if q != nil {
		if v := strings.TrimSpace(q.Address); v != "" {
			if !common.IsHexAddress(v) {
				return nil, fmt.Errorf("通道[%s]链上余额查询的地址不是合法 EVM 地址", c.opt.Name)
			}
			addr = common.HexToAddress(v)
		}
		if v := strings.TrimSpace(q.Contract); v != "" {
			if !common.IsHexAddress(v) {
				return nil, fmt.Errorf("通道[%s]链上余额查询的合约不是合法 EVM 地址", c.opt.Name)
			}
			token = common.HexToAddress(v)
		}
		if v := strings.TrimSpace(q.Currency); v != "" {
			currency = strings.ToUpper(v)
		}
	}
	if currency != pay.CurrencyUSDT {
		return nil, fmt.Errorf("通道[%s]只发 USDT-BEP20，链上余额查询不支持币种 %s", c.opt.Name, currency)
	}
	bal, err := c.rpc.TokenBalance(ctx, token, addr)
	if err != nil {
		return nil, &pay.ProviderError{Channel: c.opt.Name, Code: "balance_unread",
			Msg: "读不出代币余额: " + c.redact(err.Error()), Retryable: true}
	}
	if bal == nil {
		bal = new(big.Int)
	}
	info := &pay.AccountInfo{
		MerchantID:     addr.Hex(),
		Currency:       pay.CurrencyUSDT,
		Balance:        saturateInt64(bal),
		Available:      saturateInt64(bal),
		BalanceUnits:   bal.String(),
		AvailableUnits: bal.String(),
		Status:         pay.AcctActive,
	}
	if !bal.IsInt64() {
		info.RawNote = "余额超出 int64 可表达范围，已按上限饱和（精确值见 balance_units/available_units）"
	}
	if native, err := c.rpc.NativeBalance(ctx, addr); err == nil && native != nil {
		info.RawNote = strings.TrimSpace(info.RawNote + " gas 余额 " + native.String() + " wei")
	}
	return info, nil
}

// unknown 给一条「说不清」的回执：带上 txid，让对账有把手。
func (c *onchainChannel) unknown(entry *ledger.Entry, note string) *pay.PayResult {
	return &pay.PayResult{
		OrderNo: entry.OrderNo, TradeNo: entry.TxID, TxHash: entry.TxID,
		Status: pay.StatusUnknown, Amount: entry.Amount, AmountUnits: entry.AmountUnits, RawNote: c.note(note),
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
// 两种秘密都会顺着 RPC 错误进 RawNote：私钥（不该以任何形式出现）与 RPC 基址
// （服务商的密钥常常就写在路径或查询串里）。整串替换而不是只换密钥段——
// 只换一段的话，剩下的 host 与路径仍然把接入点暴露在日志里。
func (c *onchainChannel) redact(s string) string {
	return c.redactWith("", s)
}

// redactWith 在全局脱敏之外再抹掉这一单的按单私钥。
func (c *onchainChannel) redactWith(secret, s string) string {
	if s == "" {
		return s
	}
	for _, v := range []string{secret, c.chain.PrivateKey, c.chain.RPC} {
		if v != "" && strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

// gweiToWei 把配置里的 gwei 价换成 wei。
//
// 用 big.Float 而不是 float64 直接乘 1e9：后者在 gwei 值较大时会丢掉个位 wei，
// 而 gas 价差一个 wei 在 21000 gas 上就是 21000 分之一的误差——不影响安全，
// 但会让「配置 1 gwei，实际发出 999999999 wei」这种日志差异变得无法解释。
func gweiToWei(gwei float64) *big.Int {
	if gwei <= 0 {
		return new(big.Int)
	}
	f := new(big.Float).SetFloat64(gwei)
	f.Mul(f, big.NewFloat(1e9))
	out, _ := f.Int(nil)
	if out == nil {
		return new(big.Int)
	}
	return out
}

// saturateInt64 把大整数收窄到 int64，超出即饱和（方向只向上）。
//
// 负数一律归 0：本函数的输入都是余额、手续费这类无符号量，一个负值只可能来自
// 「读到了不该读的字节」，把它当余额报下去会让下游把一次读错当成欠款。
func saturateInt64(v *big.Int) int64 {
	if v == nil || v.Sign() < 0 {
		return 0
	}
	if v.IsInt64() {
		return v.Int64()
	}
	return int64(^uint64(0) >> 1)
}

// bigString 在 nil 时给出 "0"，免得日志里出现 "<nil>"。
func bigString(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}

// entryAmount 取台账里的精确金额：优先 AmountUnits，旧条目回落 int64 Amount。
func entryAmount(entry *ledger.Entry) (*big.Int, error) {
	if entry == nil {
		return nil, fmt.Errorf("出款台账条目为空")
	}
	if s := strings.TrimSpace(entry.AmountUnits); s != "" {
		v, err := pay.ParseAmountUnits(s)
		if err != nil {
			return nil, fmt.Errorf("出款台账金额不可解析: %w", err)
		}
		return v, nil
	}
	if entry.Amount <= 0 {
		return nil, fmt.Errorf("出款台账缺少金额")
	}
	return big.NewInt(entry.Amount), nil
}
