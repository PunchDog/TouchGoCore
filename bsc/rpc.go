package bsc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// 本文件是 BSC 侧的 JSON-RPC 接入点：把一条链真正需要的八个动作各包一层。
//
// 用 ethclient 而不是手写 JSON-RPC：这些方法（chainId / pendingNonce / gasPrice /
// estimateGas / sendRawTransaction / receipt / eth_call / blockNumber）在 BSC 上
// 与以太坊逐字同形，手写一遍只会多出一份「字段名拼错、返回体漏解」的私货。
// 真正属于本仓的知识是 ERC-20 的调用数据怎么拼、以及哪些错误算「没进池」，
// 那两样留在本文件下半部分。
//
// 读写分治的口径与 tron 一致：读侧可重试（幂等），写侧一次都不重试——重发同一份
// raw tx 虽然按哈希去重，但「第一次到底进没进池」一旦说不清，就只能交给按哈希查证，
// 不能在传输层自己加一次赌注。

// ERC-20 的函数选择器（前 4 字节 Keccak-256），以及 Transfer 事件的 topic0。
//
// 写成常量而不是从 ABI 现推：本通道只调这两个函数，而 ABI 解析器要么引一整个
// abigen 工具链，要么手写一套和这两个常量等价的东西。
var (
	// erc20TransferSelector 是 transfer(address,uint256)。
	erc20TransferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}
	// erc20BalanceOfSelector 是 balanceOf(address)。
	erc20BalanceOfSelector = []byte{0x70, 0xa8, 0x82, 0x31}
	// erc20DecimalsSelector 是 decimals()。装配期拿它复核配置的小数位。
	erc20DecimalsSelector = []byte{0x31, 0x3c, 0xe5, 0x67}
	// transferEventTopic 是 Transfer(address,address,uint256) 的签名哈希。
	// 查单靠它认「这一笔确实是本合约的转账」，反查靠它当第一个 topic 过滤。
	transferEventTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
)

// 读侧重试的退避基数与上限。三条链的出款门面共用同一个量级：
// 一次 RPC 抖动不该变成一单「结果未知」。
const (
	rpcBackoffBase = 100 * time.Millisecond
	rpcBackoffMax  = time.Second
)

// bscRPC 是一个已经拨通的 BSC 接入点。
type bscRPC struct {
	name     string
	client   *ethclient.Client
	timeout  time.Duration
	maxRetry int
}

// newBSCRPC 拨通接入点。
//
// 拨号期就探一次 chainId：基址写错、网络不通、把 WS 地址填进 RPC 这几类问题
// 在装配期报出来，比等第一笔出款再报省一次「钱有没有动」的判断。
func newBSCRPC(chain *chainConfig, name string) (*bscRPC, error) {
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]缺少 chain 段（RPC 与热钱包）", name)
	}
	addr := strings.TrimSpace(chain.RPC)
	if addr == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.rpc 为空，出款不知道该广播给哪个节点", name)
	}
	if !looksLikeRPCURL(addr) {
		return nil, fmt.Errorf("通道[%s]的 chain.rpc %q 不是 http/https/ws/wss 形态的接入点", name, addr)
	}
	timeout := chain.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := ethclient.DialContext(dialCtx, addr)
	if err != nil {
		// 错误文案可能带出基址里的路径或查询串（服务商的密钥常常就在这里），
		// 而这句话会顺着「通道不启动」的日志出去，所以只留结论不留原文。
		return nil, fmt.Errorf("通道[%s]的 chain.rpc 拨号失败：接入点不可达或形态不符", name)
	}
	r := &bscRPC{name: name, client: client, timeout: timeout, maxRetry: chain.MaxRetry}
	ctx, cancel2 := r.callContext(context.Background())
	defer cancel2()
	if _, err := client.ChainID(ctx); err != nil {
		client.Close()
		return nil, fmt.Errorf("通道[%s]的 chain.rpc 读不出 chainId：接入点不可用", name)
	}
	return r, nil
}

// looksLikeRPCURL 只判形态，不判可达性。
func looksLikeRPCURL(s string) bool {
	l := strings.ToLower(s)
	for _, p := range []string{"http://", "https://", "ws://", "wss://"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// Close 释放底层连接（供通道停启用）。
func (r *bscRPC) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	r.client.Close()
	return nil
}

// callContext 给单次调用套上超时。ctx 为 nil 时用 Background：
// 调用方从门面进来一定带 ctx，但台账反查是从 Stop 之外的路径进来的，不该让它 nil 掉。
func (r *bscRPC) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.timeout)
}

// retry 执行一次读操作，按需重试。
//
// 只重试「接入点没答上来」这一类（见 isReadRetryable）：业务性的读失败重试多少次
// 都是同一个答案，而重试会把一次故障拖成一次慢故障。
func (r *bscRPC) retry(ctx context.Context, what string, fn func(context.Context) error) error {
	attempts := r.maxRetry + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := time.Duration(i) * rpcBackoffBase
			if wait > rpcBackoffMax {
				wait = rpcBackoffMax
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		cctx, cancel := r.callContext(ctx)
		err := fn(cctx)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		if !isReadRetryable(err) {
			return err
		}
	}
	return fmt.Errorf("%s 重试 %d 次后仍失败: %w", what, attempts, last)
}

// isReadRetryable 判断读侧错误是否值得再问一次。
func isReadRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, kw := range []string{
		"timeout", "deadline exceeded", "connection reset", "connection refused",
		"broken pipe", "eof", "temporarily", "too many requests", "rate limit",
		"429", "502", "503", "504",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// ChainID 读节点的链 ID（装配期与配置比对）。
func (r *bscRPC) ChainID(ctx context.Context) (*big.Int, error) {
	var id *big.Int
	err := r.retry(ctx, "读 chainId", func(c context.Context) error {
		v, e := r.client.ChainID(c)
		if e != nil {
			return e
		}
		id = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return id, nil
}

// PendingNonce 读「含内存池在内」的下一个可用 nonce。
func (r *bscRPC) PendingNonce(ctx context.Context, addr common.Address) (uint64, error) {
	var n uint64
	err := r.retry(ctx, "读 pendingNonce", func(c context.Context) error {
		v, e := r.client.PendingNonceAt(c, addr)
		if e != nil {
			return e
		}
		n = v
		return nil
	})
	return n, err
}

// LatestNonce 读「已落块」的账户 nonce。
//
// 它是 EVM 用来判「这一笔还能不能上链」的那把尺：已落块 nonce 一旦大过交易里的 nonce，
// 同一个 nonce 位已经被别人（多半是同钱包的下一单）占了，这一笔永远进不了块。
func (r *bscRPC) LatestNonce(ctx context.Context, addr common.Address) (uint64, error) {
	var n uint64
	err := r.retry(ctx, "读账户 nonce", func(c context.Context) error {
		v, e := r.client.NonceAt(c, addr, nil)
		if e != nil {
			return e
		}
		n = v
		return nil
	})
	return n, err
}

// SuggestGasPrice 读节点建议的 gas 价（wei）。
func (r *bscRPC) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	var p *big.Int
	err := r.retry(ctx, "读 gasPrice", func(c context.Context) error {
		v, e := r.client.SuggestGasPrice(c)
		if e != nil {
			return e
		}
		p = v
		return nil
	})
	return p, err
}

// EstimateGas 估算一次调用的 gas 用量。
func (r *bscRPC) EstimateGas(ctx context.Context, msg ethereum.CallMsg) (uint64, error) {
	var g uint64
	err := r.retry(ctx, "估算 gas", func(c context.Context) error {
		v, e := r.client.EstimateGas(c, msg)
		if e != nil {
			return e
		}
		g = v
		return nil
	})
	return g, err
}

// TokenBalance 读某个地址在指定合约上的代币余额（最小单位）。
//
// 用 big.Int 而不是 int64：BEP20 的小数位由合约决定（USDT-BEP20 是 18），
// 余额一旦超过 int64 就只能在比较时保持大整数，收窄到 int64 的那一步必须显式做。
func (r *bscRPC) TokenBalance(ctx context.Context, token, owner common.Address) (*big.Int, error) {
	var out *big.Int
	err := r.retry(ctx, "读代币余额", func(c context.Context) error {
		raw, e := r.client.CallContract(c, ethereum.CallMsg{
			To:   &token,
			Data: erc20BalanceOfData(owner),
		}, nil)
		if e != nil {
			return e
		}
		v, e := decodeUint256(raw)
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// ContractDecimals 读代币合约自己的小数位。
//
// 这是唯一可信的位数来源：配置是人抄的，合约是发币时定的。抄错一位不是报错，
// 而是 10^n 倍的钱。
func (r *bscRPC) ContractDecimals(ctx context.Context, token common.Address) (int, error) {
	var out int
	err := r.retry(ctx, "读合约 decimals", func(c context.Context) error {
		raw, e := r.client.CallContract(c, ethereum.CallMsg{
			To:   &token,
			Data: erc20DecimalsSelector,
		}, nil)
		if e != nil {
			return e
		}
		v, e := decodeUint256(raw)
		if e != nil {
			return e
		}
		if !v.IsInt64() || v.Int64() < 0 || v.Int64() > 36 {
			// 0–36 是人类已用到的全部代币小数位。超出这个范围只会是
			// 「这个地址不是 ERC-20 合约」——把它的返回值当位数用会把金额放大到无意义。
			return fmt.Errorf("decimals() 返回 %s，不像一个代币合约", v.String())
		}
		out = int(v.Int64())
		return nil
	})
	return out, err
}

// NativeBalance 读原生币（BNB）余额，单位 wei。
func (r *bscRPC) NativeBalance(ctx context.Context, addr common.Address) (*big.Int, error) {
	var out *big.Int
	err := r.retry(ctx, "读原生币余额", func(c context.Context) error {
		v, e := r.client.BalanceAt(c, addr, nil)
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// SendRaw 广播一份已签名交易，返回节点回显的哈希。
//
// 写侧不重试：这里返回错误时，调用方一律按「说不清」处理（除非错误明确属于
// 「没进池」，见 rejectClass）。
//
// 直接用 eth_sendRawTransaction 而不是 ethclient 的发送方法：v1.17 的 ethclient
// 只留了同步变体（等回执），而本包的台账要求「先记账后广播、广播后按哈希查证」，
// 不能在广播里把结论等出来。
func (r *bscRPC) SendRaw(ctx context.Context, raw []byte) (common.Hash, error) {
	cctx, cancel := r.callContext(ctx)
	defer cancel()
	var hash common.Hash
	if err := r.client.Client().CallContext(cctx, &hash, "eth_sendRawTransaction", hexutil.Encode(raw)); err != nil {
		return common.Hash{}, err
	}
	return hash, nil
}

// Receipt 读回执。找不到时返回的错误仍是 ethereum.NotFound，由调用方判。
func (r *bscRPC) Receipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	var rec *types.Receipt
	err := r.retry(ctx, "读交易回执", func(c context.Context) error {
		v, e := r.client.TransactionReceipt(c, hash)
		if e != nil {
			return e
		}
		rec = v
		return nil
	})
	return rec, err
}

// TransactionByHash 按哈希问节点「这笔交易在哪」。
//
// 找不到时返回的错误仍是 ethereum.NotFound，调用方据此判断它既不在块里也不在池里。
// 回执查不到时先问这一句，是为了把「节点副本还没同步」与「这一笔真的没上链」分开。
func (r *bscRPC) TransactionByHash(ctx context.Context, hash common.Hash) (*types.Transaction, bool, error) {
	var (
		tx      *types.Transaction
		pending bool
	)
	err := r.retry(ctx, "按哈希查交易", func(c context.Context) error {
		v, p, e := r.client.TransactionByHash(c, hash)
		if e != nil {
			return e
		}
		tx, pending = v, p
		return nil
	})
	return tx, pending, err
}

// Head 读当前链头高度。
func (r *bscRPC) Head(ctx context.Context) (uint64, error) {
	var n uint64
	err := r.retry(ctx, "读链头", func(c context.Context) error {
		v, e := r.client.BlockNumber(c)
		if e != nil {
			return e
		}
		n = v
		return nil
	})
	return n, err
}

// BlockTime 读某个高度的出块时间（unix 秒）。反查窗口靠它把「72 小时」落成区块区间。
func (r *bscRPC) BlockTime(ctx context.Context, number uint64) (int64, error) {
	var t int64
	err := r.retry(ctx, "读区块时间", func(c context.Context) error {
		h, e := r.client.HeaderByNumber(c, new(big.Int).SetUint64(number))
		if e != nil {
			return e
		}
		t = int64(h.Time)
		return nil
	})
	return t, err
}

// Logs 读一段区块区间内匹配的事件日志。
func (r *bscRPC) Logs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	var out []types.Log
	err := r.retry(ctx, "读事件日志", func(c context.Context) error {
		v, e := r.client.FilterLogs(c, q)
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// erc20TransferData 拼 transfer(address,uint256) 的调用数据。
//
// 手工拼而不是引 ABI 编码器：两个参数都是 32 字节定长、无动态段，
// ABI 规则就是「选择器 + 左补零的地址 + 大端金额」，写开来只有六行。
func erc20TransferData(to common.Address, amount *big.Int) []byte {
	if amount == nil {
		amount = new(big.Int)
	}
	data := make([]byte, 0, 4+32+32)
	data = append(data, erc20TransferSelector...)
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
	return data
}

// erc20BalanceOfData 拼 balanceOf(address) 的调用数据。
func erc20BalanceOfData(owner common.Address) []byte {
	data := make([]byte, 0, 4+32)
	data = append(data, erc20BalanceOfSelector...)
	data = append(data, common.LeftPadBytes(owner.Bytes(), 32)...)
	return data
}

// addressTopic 把一个地址装成事件日志里的 topic（32 字节左补零）。
func addressTopic(a common.Address) common.Hash {
	return common.BytesToHash(common.LeftPadBytes(a.Bytes(), 32))
}

// decodeUint256 把 eth_call 的返回值解成大整数。
//
// 空返回与超长返回都判错：空返回多半是「这个地址上根本没有合约」（节点对无代码地址
// 返回 0x），把它当成余额 0 会让前置闸把一次配置错误报成「余额不足」；超长返回说明
// 这个地址上跑的不是标准 ERC-20（单个 uint256 的 ABI 编码恒为 32 字节），
// 取前 32 字节读下去等于拿一段不知道含义的字节当余额。
func decodeUint256(raw []byte) (*big.Int, error) {
	if len(raw) == 0 {
		return nil, errors.New("合约调用返回空：该地址可能不是合约")
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("合约调用返回 %d 字节，不是单个 uint256 的 32 字节：该地址可能不是标准 ERC-20", len(raw))
	}
	return new(big.Int).SetBytes(raw), nil
}

// rejectClass 把广播错误分成「确定没进池」与「说不清」。
//
// 分错的方向只有两个，代价不对称：
//   - 把「已经进池」误判成没进池，上游就会重建一单——那是双付，钱真的会多出去一次；
//   - 把「没进池」误判成说不清，这一单停在 UNKNOWN 等对账——只是慢，钱没动。
//
// 所以白名单只收「节点在入池之前就拒」的那几句明确报错；nonce too low 单独一档：
// 它说明这个 nonce 位已被占用，但占用它的可能正是本单上一次广播的那一笔，
// 必须去按哈希查证而不是直接判失败。
type rejectClass int

const (
	// rejectUnknown 说不清：停在 broadcast，交给按哈希查证。
	rejectUnknown rejectClass = iota
	// rejectBeforePool 节点明确拒收：钱一定没动，同单号允许重建。
	rejectBeforePool
	// rejectAlreadyKnown 节点说这一笔它已经见过：去查回执。
	rejectAlreadyKnown
	// rejectNonceUsed 这个 nonce 位已被占用：可能是本单已落块，去查回执。
	rejectNonceUsed
)

func classifySendError(err error) rejectClass {
	if err == nil {
		return rejectUnknown
	}
	s := strings.ToLower(err.Error())
	switch {
	case containsAny(s, "already known", "known transaction", "already exists", "already imported", "already in pool"):
		return rejectAlreadyKnown
	case strings.Contains(s, "nonce too low"):
		// nonce 被占：本单的那一笔可能已经在链上，必须先查证（不能判失败后重建）。
		return rejectNonceUsed
	case containsAny(s,
		"insufficient funds",
		"intrinsic gas too low",
		"exceeds block gas limit",
		"exceeds the block gas limit",
		"gas limit reached",
		"invalid sender",
		"invalid signature",
		"invalid chain id",
		"transaction underpriced",
		"replacement transaction underpriced",
		"gas price too low",
		"rlp:",
		"oversized data",
		"exceeds the configured cap",
		"negative value",
	):
		return rejectBeforePool
	}
	return rejectUnknown
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
