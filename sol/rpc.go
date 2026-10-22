package sol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// 本文件是 Solana 侧的 JSON-RPC 接入点：把一条链真正需要的几个动作各包一层。
//
// 用 solana-go 的 rpc 子包而不是手写 JSON-RPC：getLatestBlockhash / sendTransaction /
// getTransaction / getTokenAccountBalance / getAccountInfo / getSignaturesForAddress
// 这些方法在 Solana 上与链本身逐字对应，手写一遍只会多出一份「字段名拼错、
// 返回体漏解」的私货。真正属于本仓的知识是 SPL transfer 怎么组、以及哪些错误算
// 「没进池」，那两样留在本文件下半部分与 onchain.go。
//
// 读写分治的口径与 tron / bsc 一致：读侧可重试（幂等），写侧一次都不重试——重发
// 同一笔已签名交易虽然按签名天然去重，但「第一次到底进没进池」一旦说不清，就只能
// 交给按签名查证，不能在传输层自己加一次赌注。

// 读侧重试的退避基数与上限。三条链的出款门面共用同一个量级：
// 一次 RPC 抖动不该变成一单「结果未知」。
const (
	rpcBackoffBase = 100 * time.Millisecond
	rpcBackoffMax  = time.Second
)

// solRPC 是一个已经拨通的 Solana 接入点。
type solRPC struct {
	name       string
	client     *rpc.Client
	timeout    time.Duration
	maxRetry   int
	commitment rpc.CommitmentType
}

// newSolRPC 拨通接入点。
//
// 拨号期就探一次区块高度：基址写错、网络不通、把 WS 地址填进 RPC 这几类问题
// 在装配期报出来，比等第一笔出款再报省一次「钱有没有动」的判断。
func newSolRPC(chain *chainConfig, name string) (*solRPC, error) {
	if chain == nil {
		return nil, fmt.Errorf("通道[%s]缺少 chain 段（RPC 与热钱包）", name)
	}
	endpoint := strings.TrimSpace(chain.RPC)
	if endpoint == "" {
		return nil, fmt.Errorf("通道[%s]的 chain.rpc 为空，出款不知道该把交易交给哪个节点", name)
	}
	if !looksLikeRPCURL(endpoint) {
		return nil, fmt.Errorf("通道[%s]的 chain.rpc %q 不是 http/https 形态的接入点", name, endpoint)
	}
	timeout := chain.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// 访问令牌走 Authorization 头：RPC 基址常常被整串打进日志与错误文案，
	// 把密钥留在 URL 里等于让它跟着那些文案一起扩散。
	var client *rpc.Client
	if key := strings.TrimSpace(chain.APIKey); key != "" {
		client = rpc.NewWithHeaders(endpoint, map[string]string{"Authorization": "Bearer " + key})
	} else {
		client = rpc.New(endpoint)
	}
	r := &solRPC{
		name:       name,
		client:     client,
		timeout:    timeout,
		maxRetry:   chain.MaxRetry,
		commitment: rpc.CommitmentType(solCommitment(chain.Commitment)),
	}
	ctx, cancel := r.callContext(context.Background())
	defer cancel()
	if _, err := client.GetBlockHeight(ctx, r.commitment); err != nil {
		// 错误文案可能带出基址里的路径或查询串（服务商的密钥常常就在这里），
		// 而这句话会顺着「通道不启动」的日志出去，所以只留结论不留原文。
		client.Close()
		return nil, fmt.Errorf("通道[%s]的 chain.rpc 读不出区块高度：接入点不可达或形态不符", name)
	}
	return r, nil
}

// looksLikeRPCURL 只判形态，不判可达性。
//
// Solana 的公开节点只提供 http(s) 的 JSON-RPC；ws 是订阅通道，不是本包用的那一个，
// 因此它不在放行之列。
func looksLikeRPCURL(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// Close 释放底层连接（供通道停启用）。
func (r *solRPC) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	r.client.Close()
	return nil
}

// callContext 给单次调用套上超时。ctx 为 nil 时用 Background：
// 调用方从门面进来一定带 ctx，但台账反查是从 Stop 之外的路径进来的，不该让它 nil 掉。
func (r *solRPC) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
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
func (r *solRPC) retry(ctx context.Context, what string, fn func(context.Context) error) error {
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
	if errors.Is(err, rpc.ErrNotFound) {
		// 「链上没有这条记录」是确定性答案，重试只会得到同一个结论。
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

// Head 读当前区块高度。确认数用它减交易所在的 slot。
func (r *solRPC) Head(ctx context.Context) (uint64, error) {
	var out uint64
	err := r.retry(ctx, "读区块高度", func(c context.Context) error {
		v, e := r.client.GetBlockHeight(c, r.commitment)
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// LatestBlockhash 取一个新的 blockhash。
//
// 每次签名都要重取：blockhash 的有效期约 150 个区块（几十秒到两分钟），
// 复用旧值只会在广播时收到 BlockhashNotFound，而那一步的错误分不清
// 「本地组包太慢」与「节点没收到」。
func (r *solRPC) LatestBlockhash(ctx context.Context) (solana.Hash, uint64, error) {
	var (
		hash solana.Hash
		last uint64
	)
	err := r.retry(ctx, "读最新 blockhash", func(c context.Context) error {
		out, e := r.client.GetLatestBlockhash(c, r.commitment)
		if e != nil {
			return e
		}
		if out == nil || out.Value == nil {
			return errors.New("节点没有返回 blockhash")
		}
		hash = out.Value.Blockhash
		last = out.Value.LastValidBlockHeight
		return nil
	})
	return hash, last, err
}

// IsBlockhashValid 判断某个 blockhash 是否还在有效期内。
//
// 这是 Solana 白送的一条确定性判据：一旦 blockhash 失效，任何持有它的已签名交易
// 都不可能再上链。因此「签名查不到 + blockhash 已失效」可以安全地判死，
// 而不必让一单永远挂在 PENDING（见 query.go 的 deadByBlockhash）。
func (r *solRPC) IsBlockhashValid(ctx context.Context, hash solana.Hash) (bool, error) {
	var out bool
	err := r.retry(ctx, "读 blockhash 状态", func(c context.Context) error {
		v, e := r.client.IsBlockhashValid(c, hash, r.commitment)
		if e != nil {
			return e
		}
		if v == nil {
			return errors.New("节点没有返回 blockhash 状态")
		}
		out = v.Value
		return nil
	})
	return out, err
}

// SendTransaction 广播一笔已签名交易，绝不重试。
//
// 返回值是节点回给我们的签名；正常情况下它与 tx.Signatures[0] 逐字相同。
// 写侧不重试的理由与 bsc 一致：这一次到底有没有进池，说不清就只能查证。
func (r *solRPC) SendTransaction(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	cctx, cancel := r.callContext(ctx)
	defer cancel()
	return r.client.SendTransactionWithOpts(cctx, tx, rpc.TransactionOpts{
		Encoding:            solana.EncodingBase64,
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
}

// Transaction 按签名取一笔已落块交易。查不到时返回 rpc.ErrNotFound。
func (r *solRPC) Transaction(ctx context.Context, sig solana.Signature) (*rpc.GetTransactionResult, error) {
	var out *rpc.GetTransactionResult
	err := r.retry(ctx, "按签名查交易", func(c context.Context) error {
		v, e := r.client.GetTransaction(c, sig, &rpc.GetTransactionOpts{
			// 显式声明最高版本：老节点默认只回 v0 之前的交易，遇到 v0 交易会直接报错，
			// 而那是「查不动」而不是「查不到」——两者在出款结论上差一个终局。
			MaxSupportedTransactionVersion: rpc.NewTransactionVersion(0),
			Commitment:                     r.commitment,
		})
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// TokenAccountBalance 读一个 token account 的余额。
func (r *solRPC) TokenAccountBalance(ctx context.Context, account solana.PublicKey) (*rpc.UiTokenAmount, error) {
	var out *rpc.UiTokenAmount
	err := r.retry(ctx, "读 token account 余额", func(c context.Context) error {
		v, e := r.client.GetTokenAccountBalance(c, account, r.commitment)
		if e != nil {
			return e
		}
		if v == nil {
			return errors.New("节点没有返回 token account 余额")
		}
		out = v.Value
		return nil
	})
	return out, err
}

// AccountInfo 读一个账户的原始信息。不存在时返回 rpc.ErrNotFound。
func (r *solRPC) AccountInfo(ctx context.Context, account solana.PublicKey) (*rpc.Account, error) {
	var out *rpc.Account
	err := r.retry(ctx, "读账户信息", func(c context.Context) error {
		v, e := r.client.GetAccountInfo(c, account)
		if e != nil {
			return e
		}
		if v == nil || v.Value == nil {
			return rpc.ErrNotFound
		}
		out = v.Value
		return nil
	})
	return out, err
}

// NativeBalance 读原生 SOL 余额（lamports）。
func (r *solRPC) NativeBalance(ctx context.Context, account solana.PublicKey) (uint64, error) {
	var out uint64
	err := r.retry(ctx, "读原生币余额", func(c context.Context) error {
		v, e := r.client.GetBalance(c, account, r.commitment)
		if e != nil {
			return e
		}
		if v == nil {
			return errors.New("节点没有返回原生币余额")
		}
		out = v.Value
		return nil
	})
	return out, err
}

// SignaturesForAddress 从 before 往更早的方向翻一页签名。
//
// before 为零值时从最新一笔开始。返回值按时间倒序（链上口径），
// 分页的续传游标是本页最后一条的签名。
func (r *solRPC) SignaturesForAddress(ctx context.Context, account solana.PublicKey, before solana.Signature, limit int) ([]*rpc.TransactionSignature, error) {
	var out []*rpc.TransactionSignature
	err := r.retry(ctx, "反查地址签名", func(c context.Context) error {
		opts := &rpc.GetSignaturesForAddressOpts{Commitment: r.commitment}
		if limit > 0 {
			l := limit
			opts.Limit = &l
		}
		if !before.IsZero() {
			opts.Before = before
		}
		v, e := r.client.GetSignaturesForAddressWithOpts(c, account, opts)
		if e != nil {
			return e
		}
		out = v
		return nil
	})
	return out, err
}

// sendOutcome 是广播错误的分级结果。
//
// 三类的处置完全不同：
//   - alreadyKnown：节点见过这一笔，钱可能已经在路上，只能查证；
//   - blockhashGone：本地这笔的 blockhash 已失效，它不可能再上链，查证后判死；
//   - beforePool：节点在预检阶段就拒了，钱没动过，允许同单号重建重发；
//   - unknown：请求可能已经到达节点，状态停在 broadcast，结论交给按签名查证。
type sendOutcome int

const (
	sendUnknown sendOutcome = iota
	sendAlreadyKnown
	sendBlockhashGone
	sendBeforePool
)

// classifySendError 把节点错误归到上面四类。
//
// 按文案匹配是这里唯一可行的办法：solana-go 的 rpc 包把节点错误包成
// *jsonrpc.RPCError，错误码是服务端自定义的整数，各节点实现并不统一；
// 而文案里的 simulation failed / AlreadyProcessed 是链客户端自己写死的。
// 认不出来的默认走 unknown——那是唯一不会导致「重发第二笔」的分支。
func classifySendError(err error) sendOutcome {
	if err == nil {
		return sendUnknown
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "alreadyprocessed") || strings.Contains(s, "already processed"):
		return sendAlreadyKnown
	case strings.Contains(s, "blockhash not found") || strings.Contains(s, "blockhashnotfound"):
		return sendBlockhashGone
	case strings.Contains(s, "simulation failed"),
		strings.Contains(s, "preflight"),
		strings.Contains(s, "signature verification failure"),
		strings.Contains(s, "insufficient lamports"),
		strings.Contains(s, "invalid account data"):
		// 预检/模拟阶段就被拒：这一笔从未进入转发路径，钱没有动过。
		return sendBeforePool
	default:
		return sendUnknown
	}
}
