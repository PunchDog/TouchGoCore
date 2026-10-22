package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	tonapi "github.com/tonkeeper/tonapi-go"
	"github.com/tonkeeper/tongo/ton"

	"touchgocore/pay"
)

// 本文件是 tonGateway 的唯一真实实现：出款报文与查证都走 TonAPI v2 的 HTTP 接口。
//
// 为什么是它而不是 tongo 自带的 lite client：lite server 走裸 TCP + ADNL，
// 要求配置里逐字登记每个节点的公钥，且协议层根本没有「按消息 hash 反查交易」这类提问
// （tongo 自己的 wallet.SendV2 也只能靠轮询 seqno 猜）。TonAPI 在投递前会拿当前状态
// 校验这条外部消息（实测 406 文本 "cannot apply external message to current state"），
// 这把「组错的报文」挡在了动钱之前，而账户交易列表是现成的查证把手。
//
// 代价写在这里：能不能出款从此取决于第三方接口的可用性，所以本文件对每一条错误
// 都要回答「钱可能动过吗」，而不是把 HTTP 状态码直接翻译成败与败。

// defaultTONTimeout 是单次链上操作的缺省超时（配置没给 timeout_sec 时用）。
const defaultTONTimeout = 15 * time.Second

// tonBusinessRejectStatuses 是「接入点看过这条消息并拒了，它不可能已经进池」的状态码。
//
// 406 是实测拿到的那条（liteserver 校验外部消息不过）；400/415/422 是同族——报文本身
// 不成形，压根没走到投递那一步。其余一律按不确定处理：判错方向的代价不对称，
// 把真发出去的那笔当成没发出会重下一单（双付），而把没发出的当成不确定只是多查一次。
var tonBusinessRejectStatuses = map[int]bool{
	http.StatusBadRequest:            true,
	http.StatusNotAcceptable:         true,
	http.StatusUnsupportedMediaType:  true,
	http.StatusUnprocessableEntity:   true,
	http.StatusRequestEntityTooLarge: true,
}

// tonAPIGateway 把 TonAPI v2 的生成客户端接成窄网关。
type tonAPIGateway struct {
	cli      *tonapi.Client
	base     string
	timeout  time.Duration
	maxRetry int
}

var _ tonGateway = (*tonAPIGateway)(nil)

// newTonAPIGateway 按链上配置建接入点。
//
// http_base 留空时按网络取 TonAPI 的公开基址；配了就用它（自建或代理出去的同一套接口）。
// api_key 留空即匿名：实测匿名可用但会被限流，限流在本网关里判不确定而不是判失败。
func newTonAPIGateway(chain *chainConfig) (*tonAPIGateway, error) {
	base := strings.TrimSpace(chain.HTTPBase)
	if base == "" {
		if pay.IsTestnet(chain.Network) {
			base = tonapi.TestnetTonApiURL
		} else {
			base = tonapi.TonApiURL
		}
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return nil, fmt.Errorf("chain.http_base %s 不是 http/https 基址，TonAPI 接入点无法建连", base)
	}
	var sec tonapi.SecuritySource = &tonapi.Security{}
	if key := strings.TrimSpace(chain.APIKey); key != "" {
		// 只有 Authorization: Bearer 这一种鉴权：实测 X-API-KEY 与 ?api_key= 会被服务端忽略，
		// 把密钥写在错的地方不会报错，只会安静地退化成匿名额度。
		sec = tonapi.WithToken(key)
	}
	timeout := chain.Timeout
	if timeout <= 0 {
		timeout = defaultTONTimeout
	}
	hc := &http.Client{Timeout: timeout}
	cli, err := tonapi.NewClient(base, sec, tonapi.WithClient(hc))
	if err != nil {
		return nil, fmt.Errorf("TON 接入点 %s 建客户端失败: %w", base, err)
	}
	return &tonAPIGateway{cli: cli, base: base, timeout: timeout, maxRetry: chain.MaxRetry}, nil
}

// readAttempts 读侧的总尝试次数。
//
// 写侧永远不看它：广播不是「失败了再来一次」的操作，重发同一条报文要由台账决定。
func (g *tonAPIGateway) readAttempts() int {
	if g.maxRetry < 0 {
		return 1
	}
	return g.maxRetry + 1
}

// retryRead 只在「接入点没答上来」这类错误上重试。
func (g *tonAPIGateway) retryRead(label string, fn func() error) error {
	var last error
	for i := 0; i < g.readAttempts(); i++ {
		if last = fn(); last == nil {
			return nil
		}
		if errors.Is(last, context.Canceled) || errors.Is(last, context.DeadlineExceeded) {
			// 调用方自己的 ctx 到期：重试只是拖长出款，不改变结局。
			return last
		}
	}
	return fmt.Errorf("%s 读侧 %d 次尝试均失败: %w", label, g.readAttempts(), last)
}

func (g *tonAPIGateway) seqno(ctx context.Context, account ton.AccountID) (uint32, error) {
	var params tonapi.GetAccountSeqnoParams
	params.AccountID = account.ToRaw()
	var out uint32
	err := g.retryRead("seqno", func() error {
		res, err := g.cli.GetAccountSeqno(ctx, params)
		if err != nil {
			return err
		}
		if res.Seqno < 0 {
			return fmt.Errorf("接入点回了负数序号 %d", res.Seqno)
		}
		out = uint32(res.Seqno)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return out, nil
}

func (g *tonAPIGateway) account(ctx context.Context, account ton.AccountID) (*tonAccount, error) {
	var params tonapi.GetAccountParams
	params.AccountID = account.ToRaw()
	var out *tonAccount
	err := g.retryRead("account", func() error {
		res, err := g.cli.GetAccount(ctx, params)
		if err != nil {
			return err
		}
		out = &tonAccount{BalanceNanoton: res.Balance, Status: string(res.Status)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *tonAPIGateway) sendExtMessage(ctx context.Context, payload []byte) (*tonBroadcast, error) {
	var req tonapi.SendBlockchainMessageReq
	// base64 而不是 hex：与 tongo 自带 helper 的写法一致，两种编码服务端都收，
	// 少一处形态就少一处「本地能发出去但对方读成别的字节」的余地。
	req.Boc.SetTo(base64.StdEncoding.EncodeToString(payload))
	err := g.cli.SendBlockchainMessage(ctx, &req)
	if err == nil {
		return &tonBroadcast{Code: "200", Accepted: true}, nil
	}
	code, resp, answered := tonErrorStatus(err)
	if !answered {
		return &tonBroadcast{Code: "transport: " + err.Error(), Uncertain: true}, nil
	}
	text := tonErrorText(resp)
	switch {
	case tonBusinessRejectStatuses[code]:
		return &tonBroadcast{Code: fmt.Sprintf("%d %s", code, text), Rejected: true}, nil
	default:
		// 5xx、429、以及任何没被点名的状态：接入点可能已经转发过这条消息。
		return &tonBroadcast{Code: fmt.Sprintf("%d %s", code, text), Uncertain: true}, nil
	}
}

func (g *tonAPIGateway) transactions(ctx context.Context, account ton.AccountID, limit int) ([]tonTx, error) {
	if limit <= 0 {
		return nil, errors.New("TON 交易列表的条数上限必须为正")
	}
	var params tonapi.GetBlockchainAccountTransactionsParams
	params.AccountID = account.ToRaw()
	params.Limit.SetTo(int32(limit))
	params.SortOrder.SetTo(tonapi.GetBlockchainAccountTransactionsSortOrderDesc)
	var out []tonTx
	err := g.retryRead("transactions", func() error {
		res, err := g.cli.GetBlockchainAccountTransactions(ctx, params)
		if err != nil {
			return err
		}
		out = out[:0]
		for i := range res.Transactions {
			out = append(out, normalizeTx(&res.Transactions[i], account))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (g *tonAPIGateway) close() error { return nil }

// tonErrorStatus 从 ogen 客户端的错误里取 HTTP 状态码。
//
// 非 2xx 统一被包成 *tonapi.ErrorStatusCode（外层是 go-faster 的 wrap，errors.As 可穿透）；
// 取不到状态码就是请求根本没拿到响应——连不上、被切断、超时都在这一类。
func tonErrorStatus(err error) (int, *tonapi.Error, bool) {
	var esc *tonapi.ErrorStatusCode
	if errors.As(err, &esc) {
		return esc.StatusCode, &esc.Response, true
	}
	return 0, nil, false
}

// tonErrorText 取服务端给的错误正文摘要。
func tonErrorText(resp *tonapi.Error) string {
	if resp == nil {
		return ""
	}
	s := strings.TrimSpace(resp.Error)
	if code, ok := resp.ErrorCode.Get(); ok {
		return fmt.Sprintf("%s (error_code=%d)", s, code)
	}
	return s
}

// normalizeTx 把 TonAPI 的交易摘要折成本包要问的那几个问题。
//
// 判定口径刻意保守：只要有一项执行阶段读不出来就把 VerdictKnown 关掉，让上层回 UNKNOWN。
// 「读不懂就当失败」会让上游重下一单，而链上那一笔可能已经把币转出去了。
func normalizeTx(t *tonapi.Transaction, self ton.AccountID) tonTx {
	out := tonTx{
		Hash:             strings.ToLower(strings.TrimSpace(t.Hash)),
		UnixTime:         t.Utime,
		TotalFeesNanoton: t.TotalFees,
	}
	if m, ok := t.InMsg.Get(); ok {
		out.InMsgHash = strings.ToLower(strings.TrimSpace(m.Hash))
		out.InMsgKind = string(m.MsgType)
	}
	own := strings.ToLower(self.ToRaw())
	for i := range t.OutMsgs {
		m := t.OutMsgs[i]
		dest := ""
		if a, ok := m.Destination.Get(); ok {
			dest = strings.ToLower(strings.TrimSpace(a.Address))
		}
		out.Outs = append(out.Outs, tonOut{To: dest, AmountNanoton: m.Value})
		// 退回来的那一条（bounced 且收端是我们自己）就是「钱没到手，但费已经烧了」。
		if m.Bounced && sameTONAddress(dest, own) {
			out.Bounced = true
		}
	}
	out.Success, out.VerdictKnown = tonVerdict(t)
	return out
}

// tonVerdict 给出「这一笔到底成没成」以及「链上有没有说清楚」。
//
// 三个来源要同时点头：交易总判定 success、计算阶段没跳过且成功、动作阶段成功，
// 且 aborted 没置起。少任何一环都给 VerdictKnown=false——宁可不判。
func tonVerdict(t *tonapi.Transaction) (success, known bool) {
	cp, hasCP := t.ComputePhase.Get()
	ap, hasAP := t.ActionPhase.Get()
	switch {
	case !hasCP || !hasAP:
		// 缺执行阶段：可能是节点版本省略，也可能是这条外部消息根本没触发计算。
		return false, false
	case cp.Skipped:
		return false, true
	}
	cs, ok := cp.Success.Get()
	if !ok {
		return false, false
	}
	if !cs || t.Aborted || !ap.Success {
		return false, true
	}
	return t.Success, true
}
