package tron

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	sdkclient "github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// grpcGateway 是出款链路的正门：组交易、广播、查回执都走节点 gRPC。
type grpcGateway struct {
	g *sdkclient.GrpcClient
}

// defaultChainTimeout 是给 SDK 的 ctx 超时兜底。
//
// 必须给一个正数：SDK 的 newContext 直接把 grpcTimeout 交给 context.WithTimeout，
// 传 0 就是「一出生就过期」，表现是每一次链上调用都超时失败，而配置里没写超时才是常态。
const defaultChainTimeout = 15 * time.Second

// errNoTransferListing 是 gRPC 侧的能力边界：全节点接口没有「按地址列出历史转账」。
//
// 台账丢失后的反查只能靠接入点的扩展读接口（TronGrid REST），所以配了链上出款就应当
// 同时配 chain.http_base——缺它时不是静默少一层保护，而是启动期就少一层保护并被日志记下。
var errNoTransferListing = errors.New("gRPC 接入点没有转账历史列表接口，反查需要配置 chain.http_base")

func newGRPCGateway(node, apiKey string, timeout time.Duration, plaintextNode bool, dialOpts ...grpc.DialOption) (*grpcGateway, error) {
	if node == "" {
		return nil, errors.New("链上通道未配置节点接入点（pay_sdks.<段>.chain.node）")
	}
	if timeout <= 0 {
		timeout = defaultChainTimeout
	}
	// grpc-go 从 1.8x 起拒绝「没声明传输安全」的拨号：不显式给凭据就建不来连接，
	// 而报错会推迟到第一次调用。所以这一项必须在这里显式决定，不吃缺省。
	// 缺省走 TLS（公共接入点要求 TLS，证书与主机名照验）；只有自建明文节点才允许显式降级。
	cred := credentials.NewTLS(&tls.Config{})
	if plaintextNode {
		cred = insecure.NewCredentials()
	}
	dialOpts = append([]grpc.DialOption{grpc.WithTransportCredentials(cred)}, dialOpts...)
	g := sdkclient.NewGrpcClientWithTimeout(node, timeout)
	if apiKey != "" {
		// API Key 只写进 SDK 的出站元数据（TRON-PRO-API-KEY 头），不进日志与错误文案。
		if err := g.SetAPIKey(apiKey); err != nil {
			return nil, fmt.Errorf("链上接入点的 API Key 写入失败: %w", err)
		}
	}
	if err := g.Start(dialOpts...); err != nil {
		return nil, fmt.Errorf("链上接入点 %s 建连失败: %w", node, err)
	}
	return &grpcGateway{g: g}, nil
}

// buildTransfer 让节点组好 refBlock/expiration/fee_limit 后交出未签名交易。
//
// 交易号取节点回的 txid 并与本地 sha256(raw_data) 对账（两份实现来自不同代码库），
// 不一致就地拒绝——广播结果不确定时，手里那个号必须与链上那个号是同一个。
func (w *grpcGateway) buildTransfer(ctx context.Context, from, to, contract string, amount int64, feeLimit int64) (*core.Transaction, error) {
	ext, err := w.g.TRC20SendCtx(ctx, from, to, contract, big.NewInt(amount), feeLimit)
	if err != nil {
		return nil, fmt.Errorf("节点未给出可签名的转账交易: %w", err)
	}
	tx := ext.GetTransaction()
	if tx == nil {
		return nil, errors.New("节点回了一笔没有交易体的转账构造结果")
	}
	if res := ext.GetResult(); res != nil && !res.GetResult() {
		return nil, fmt.Errorf("节点拒绝构造转账交易 code=%s: %s", res.GetCode().String(), clipBytes(res.GetMessage(), 96))
	}
	if err := crossCheckTxID(ext.GetTxid(), tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// crossCheckTxID 对账节点回的交易号与本地按 sha256(raw_data) 算出的交易号。
//
// 两份实现来自不同代码库（gotron-sdk 与本包 encoding.go），对不上说明其中一侧对
// raw_data 的理解出了问题——而广播结果不确定的那一刻，唯一能拿去查证的标识必须在
// 我们手里，且与链上那个号是同一个。节点没回号时跳过：那是节点的缺失，不是分歧，
// 本地算出的那一个仍然可用（见 txIDOf 的注释）。
func crossCheckTxID(nodeTxID []byte, tx *core.Transaction) error {
	if len(nodeTxID) == 0 {
		return nil
	}
	local, err := txIDOf(tx)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(nodeTxID); got != local {
		return fmt.Errorf("节点回的交易号 %s 与本地按 sha256(raw_data) 算出的 %s 不一致", got, local)
	}
	return nil
}

func (w *grpcGateway) broadcast(ctx context.Context, tx *core.Transaction) (*broadcastResult, error) {
	ret, err := w.g.BroadcastCtx(ctx, tx)
	if err != nil {
		// 只有「请求确定没到达节点」才敢说不确定之外的结论；这一层分不出，一律按不确定处理。
		return &broadcastResult{Code: "transport", Uncertain: !isDefinitelyUnsent(err)}, err
	}
	return broadcastOutcome(ret), nil
}

// isDefinitelyUnsent 判断这个 gRPC 错误是否能肯定「请求没被节点收到」。
//
// 认不出的一律算「可能已收到」（返回 false）：把它当成没发出去去重签一笔，
// 就是双付；反过来把它当成已发出，最多是让这一单走对账，方向是安全的。
func isDefinitelyUnsent(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

// broadcastOutcome 把节点的响应码分成三类：受理、确定没进链、说不清。
//
// 只有「确定没进链」允许在同一单号上重建重签；说不清必须去按 txid 查证。
// 判定宁严勿松：DUP_TRANSACTION 是「这笔已经被节点见过」，TAPOS/过期/签名错是节点
// 在进池之前就拒了，SERVER_BUSY/NO_CONNECTION/BLOCK_UNSOLIDIFIED 都可能已经入池。
func broadcastOutcome(ret *api.Return) *broadcastResult {
	code := ret.GetCode().String()
	if ret.GetResult() {
		return &broadcastResult{Code: code, Accepted: true}
	}
	switch ret.GetCode() {
	case api.Return_SUCCESS:
		// result=false 却给了 SUCCESS 码是节点自相矛盾，按说不清处理。
		return &broadcastResult{Code: code, Uncertain: true}
	case api.Return_SIGERROR,
		api.Return_TAPOS_ERROR,
		api.Return_TRANSACTION_EXPIRATION_ERROR,
		api.Return_TOO_BIG_TRANSACTION_ERROR,
		api.Return_CONTRACT_VALIDATE_ERROR,
		api.Return_BANDWITH_ERROR:
		return &broadcastResult{Code: code, Rejected: true}
	default:
		return &broadcastResult{Code: code, Uncertain: true}
	}
}

func (w *grpcGateway) lookupTx(ctx context.Context, txID string) (*txReceipt, error) {
	ti, err := w.g.GetTransactionInfoByIDCtx(ctx, txID)
	if err != nil {
		return nil, err
	}
	// 未上链时节点回一个空消息而不是报错，这里用「没有交易号」认出来。
	if ti == nil || len(ti.GetId()) == 0 {
		return &txReceipt{}, nil
	}
	rec := ti.GetReceipt()
	verdict, known := receiptVerdict(ti.GetResult(), rec.GetResult(), rec != nil)
	fee := ti.GetFee()
	if fee == 0 {
		fee = rec.GetEnergyFee() + rec.GetNetFee()
	}
	return &txReceipt{
		Found:        true,
		InBlock:      ti.GetBlockNumber() > 0,
		Success:      verdict,
		VerdictKnown: known,
		BlockNumber:  ti.GetBlockNumber(),
		FeeSun:       fee,
		ResultCode:   ti.GetResult().String() + "/" + rec.GetResult().String(),
	}, nil
}

func (w *grpcGateway) tokenBalance(ctx context.Context, addr, contract string) (int64, error) {
	bal, err := w.g.TRC20ContractBalanceCtx(ctx, addr, contract)
	if err != nil {
		return 0, err
	}
	return bigToInt64(bal)
}

func (w *grpcGateway) account(ctx context.Context, addr string) (*accountState, error) {
	acct, err := w.g.GetAccountCtx(ctx, addr)
	if errors.Is(err, sdkclient.ErrAccountNotFound) || (err == nil && acct == nil) {
		// 未激活账户是链上事实而不是故障：它照样能收钱，但收款端会让 gas 变贵。
		return &accountState{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &accountState{BalanceSun: acct.GetBalance()}, nil
}

func (w *grpcGateway) headBlock(ctx context.Context) (int64, error) {
	blk, err := w.g.GetNowBlockCtx(ctx)
	if err != nil {
		return 0, err
	}
	return blk.GetBlockHeader().GetRawData().GetNumber(), nil
}

func (w *grpcGateway) outboundTransfers(context.Context, string, string, int64) ([]outboundTransfer, error) {
	return nil, errNoTransferListing
}

func (w *grpcGateway) close() error {
	w.g.Stop()
	return nil
}

// clipBytes 把节点回的消息压成一行可入日志的摘要（消息里不含凭证）。
func clipBytes(b []byte, n int) string {
	s := hexSafe(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func hexSafe(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

// 编译期钉住接口实现：签名改动要在编译期暴露，而不是到第一笔出款时才发现少实现了一个方法。
var _ chainGateway = (*grpcGateway)(nil)
